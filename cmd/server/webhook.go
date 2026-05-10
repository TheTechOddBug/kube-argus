package main

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	corev1 "k8s.io/api/core/v1"
	k8serr "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// ─── Generic Webhook Notifications (single endpoint, per-event filter) ──

type webhookConfig struct {
	URL    string   `json:"url"`
	Secret string   `json:"secret,omitempty"`
	Events []string `json:"events"` // empty = all events
}

var webhookCfg struct {
	mu     sync.RWMutex
	config webhookConfig
}

const (
	webhookCMKeyURL    = "webhook_url"
	webhookCMKeySecret = "webhook_signing_secret"
	webhookCMKeyEvents = "webhook_events"
	// Cleanup target: short-lived destinations schema (never released).
	webhookCMKeyDestsLegacy = "webhook_destinations"

	webhookSecretMask  = "••••••••"
	webhookTimeout     = 8 * time.Second
	webhookSigHeader   = "X-KubeArgus-Signature"
	webhookEventHeader = "X-KubeArgus-Event"
)

// webhookKnownEvents is the canonical list shown in the UI as filter options.
var webhookKnownEvents = []string{
	"jit.requested",
	"jit.approved",
	"jit.denied",
	"jit.revoked",
	"jit.expired",
}

func webhookGetConfig() webhookConfig {
	webhookCfg.mu.RLock()
	defer webhookCfg.mu.RUnlock()
	out := webhookCfg.config
	if out.Events != nil {
		events := make([]string, len(out.Events))
		copy(events, out.Events)
		out.Events = events
	}
	return out
}

func initWebhook() {
	webhookRestoreConfig()

	webhookCfg.mu.Lock()
	if webhookCfg.config.URL == "" {
		webhookCfg.config.URL = os.Getenv("NOTIFY_WEBHOOK_URL")
	}
	if webhookCfg.config.Secret == "" {
		webhookCfg.config.Secret = os.Getenv("NOTIFY_WEBHOOK_SECRET")
	}
	webhookCfg.mu.Unlock()

	if c := webhookGetConfig(); c.URL != "" {
		slog.Info("webhook: notifications enabled", "signed", c.Secret != "", "filtered", len(c.Events) > 0)
	}
}

func webhookRestoreConfig() {
	c, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	cm, err := clientset.CoreV1().ConfigMaps(jitCMNamespace).Get(c, slackCMName, metav1.GetOptions{})
	if err != nil {
		return
	}

	cfg := webhookConfig{
		URL:    cm.Data[webhookCMKeyURL],
		Secret: cm.Data[webhookCMKeySecret],
	}
	if eventsJSON := cm.Data[webhookCMKeyEvents]; eventsJSON != "" {
		_ = json.Unmarshal([]byte(eventsJSON), &cfg.Events)
	}

	// One-time migration from the unreleased destinations schema: take the
	// first enabled destination if present and the new keys are unset.
	if cfg.URL == "" {
		if destsJSON := cm.Data[webhookCMKeyDestsLegacy]; destsJSON != "" {
			var dests []struct {
				URL     string   `json:"url"`
				Secret  string   `json:"secret"`
				Events  []string `json:"events"`
				Enabled bool     `json:"enabled"`
			}
			if json.Unmarshal([]byte(destsJSON), &dests) == nil {
				for _, d := range dests {
					if d.Enabled && d.URL != "" {
						cfg = webhookConfig{URL: d.URL, Secret: d.Secret, Events: d.Events}
						break
					}
				}
			}
		}
	}

	webhookCfg.mu.Lock()
	webhookCfg.config = cfg
	webhookCfg.mu.Unlock()
}

func webhookPersistConfig() {
	cfg := webhookGetConfig()
	eventsJSON, _ := json.Marshal(cfg.Events)

	c, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	cm, err := clientset.CoreV1().ConfigMaps(jitCMNamespace).Get(c, slackCMName, metav1.GetOptions{})
	if k8serr.IsNotFound(err) {
		newCM := &corev1.ConfigMap{
			ObjectMeta: metav1.ObjectMeta{
				Name:      slackCMName,
				Namespace: jitCMNamespace,
				Labels:    map[string]string{"app.kubernetes.io/managed-by": "kube-argus"},
			},
			Data: map[string]string{
				webhookCMKeyURL:    cfg.URL,
				webhookCMKeySecret: cfg.Secret,
				webhookCMKeyEvents: string(eventsJSON),
			},
		}
		if _, err := clientset.CoreV1().ConfigMaps(jitCMNamespace).Create(c, newCM, metav1.CreateOptions{}); err != nil {
			slog.Error("webhook: persist create failed", "error", err)
		}
		return
	}
	if err != nil {
		slog.Error("webhook: persist get failed", "error", err)
		return
	}
	if cm.Data == nil {
		cm.Data = make(map[string]string)
	}
	cm.Data[webhookCMKeyURL] = cfg.URL
	cm.Data[webhookCMKeySecret] = cfg.Secret
	cm.Data[webhookCMKeyEvents] = string(eventsJSON)
	// Remove the unreleased destinations key once we've migrated past it.
	delete(cm.Data, webhookCMKeyDestsLegacy)

	if _, err := clientset.CoreV1().ConfigMaps(jitCMNamespace).Update(c, cm, metav1.UpdateOptions{}); err != nil {
		slog.Error("webhook: persist update failed", "error", err)
	}
}

// ─── Settings API ───────────────────────────────────────────────────

func maskSecret(s string) string {
	if s == "" {
		return ""
	}
	return webhookSecretMask
}

func maskURL(s string) string {
	if s == "" {
		return ""
	}
	if len(s) <= 40 {
		return s
	}
	return s[:40] + "••••••"
}

func apiWebhookSettings(w http.ResponseWriter, r *http.Request) {
	if !requireAdmin(w, r) {
		return
	}

	switch r.Method {
	case "GET":
		cfg := webhookGetConfig()
		j(w, map[string]any{
			"webhookURL":    maskURL(cfg.URL),
			"signingSecret": maskSecret(cfg.Secret),
			"events":        cfg.Events,
			"enabled":       cfg.URL != "",
			"signed":        cfg.Secret != "",
			"knownEvents":   webhookKnownEvents,
		})

	case "PUT":
		var body struct {
			WebhookURL    *string  `json:"webhookURL"`
			SigningSecret *string  `json:"signingSecret"`
			Events        []string `json:"events"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			je(w, "invalid JSON", 400)
			return
		}
		webhookCfg.mu.Lock()
		if body.WebhookURL != nil {
			val := strings.TrimSpace(*body.WebhookURL)
			if !strings.Contains(val, "••••••") {
				webhookCfg.config.URL = val
			}
		}
		if body.SigningSecret != nil && *body.SigningSecret != webhookSecretMask {
			webhookCfg.config.Secret = *body.SigningSecret
		}
		// Events is full-replace; nil means "no change", empty array means "all events".
		if body.Events != nil {
			webhookCfg.config.Events = body.Events
		}
		webhookCfg.mu.Unlock()

		go webhookPersistConfig()

		if sd, ok := r.Context().Value(userCtxKey).(*sessionData); ok && sd != nil {
			auditRecord(sd.Email, sd.Role, "settings.webhook", "", "updated", clientIP(r))
		}
		j(w, map[string]string{"status": "ok"})

	case "POST":
		// Test webhook by sending a synthetic event.
		var body struct {
			WebhookURL string `json:"webhookURL"`
		}
		json.NewDecoder(r.Body).Decode(&body)
		saved := webhookGetConfig()
		url := body.WebhookURL
		secret := saved.Secret
		if url == "" || strings.Contains(url, "••••••") {
			url = saved.URL
		}
		if url == "" {
			je(w, `{"error":"webhook URL not provided"}`, 400)
			return
		}

		payload := map[string]any{
			"event":     "test",
			"cluster":   clusterName,
			"timestamp": time.Now().UTC().Format(time.RFC3339),
			"data": map[string]any{
				"message": "Kube-Argus webhook integration test",
			},
		}
		raw, _ := json.Marshal(payload)

		ctx, cancel := context.WithTimeout(r.Context(), webhookTimeout)
		defer cancel()
		req, _ := http.NewRequestWithContext(ctx, "POST", url, bytes.NewReader(raw))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set(webhookEventHeader, "test")
		if secret != "" {
			req.Header.Set(webhookSigHeader, "sha256="+webhookSign(raw, secret))
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			je(w, fmt.Sprintf("failed to reach webhook: %s", err.Error()), 502)
			return
		}
		defer resp.Body.Close()
		if resp.StatusCode < 200 || resp.StatusCode >= 300 {
			je(w, fmt.Sprintf("webhook returned status %d", resp.StatusCode), 502)
			return
		}
		j(w, map[string]string{"status": "ok"})

	default:
		je(w, "method not allowed", 405)
	}
}

// ─── JIT Notifications ──────────────────────────────────────────────

// configAcceptsEvent — empty Events list means "all events".
func configAcceptsEvent(c webhookConfig, event string) bool {
	if len(c.Events) == 0 {
		return true
	}
	for _, e := range c.Events {
		if e == event {
			return true
		}
	}
	return false
}

// webhookNotifyJIT posts a JIT lifecycle event when the configured webhook
// is set and subscribed to that event type.
func webhookNotifyJIT(event string, req *jitRequest, actor string) {
	cfg := webhookGetConfig()
	if cfg.URL == "" || req == nil {
		return
	}
	if !configAcceptsEvent(cfg, event) {
		return
	}
	payload := map[string]any{
		"event":     event,
		"cluster":   clusterName,
		"timestamp": time.Now().UTC().Format(time.RFC3339),
		"data":      req,
	}
	if actor != "" {
		payload["actor"] = actor
	}
	webhookPost(cfg.URL, cfg.Secret, event, payload)
}

func webhookPost(url, secret, event string, payload map[string]any) {
	raw, err := json.Marshal(payload)
	if err != nil {
		slog.Warn("webhook: marshal failed", "error", err)
		return
	}
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), webhookTimeout)
		defer cancel()
		req, err := http.NewRequestWithContext(ctx, "POST", url, bytes.NewReader(raw))
		if err != nil {
			slog.Warn("webhook: build request failed", "error", err)
			return
		}
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set(webhookEventHeader, event)
		if secret != "" {
			req.Header.Set(webhookSigHeader, "sha256="+webhookSign(raw, secret))
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			slog.Warn("webhook: post failed", "event", event, "error", err)
			return
		}
		defer resp.Body.Close()
		if resp.StatusCode < 200 || resp.StatusCode >= 300 {
			slog.Warn("webhook: non-2xx response", "event", event, "status", resp.StatusCode)
		}
	}()
}

func webhookSign(body []byte, secret string) string {
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(body)
	return hex.EncodeToString(mac.Sum(nil))
}
