package notify

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"sync"
	"time"

	"kube-argus/internal/httpx"

	corev1 "k8s.io/api/core/v1"
	k8serr "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// ─── Slack Configuration ────────────────────────────────────────────

var slackCfg struct {
	mu            sync.RWMutex
	webhookURL    string
	signingSecret string
}

// SlackCMName is the ConfigMap that stores Slack + webhook settings.
const SlackCMName = "kube-argus-settings"

func slackGetConfig() (webhookURL, signingSecret string) {
	slackCfg.mu.RLock()
	defer slackCfg.mu.RUnlock()
	return slackCfg.webhookURL, slackCfg.signingSecret
}

// SlackIsEnabled reports whether a Slack webhook URL is configured.
func SlackIsEnabled() bool {
	url, _ := slackGetConfig()
	return url != ""
}

// InitSlack loads the persisted Slack configuration (ConfigMap takes
// precedence) and falls back to SLACK_WEBHOOK_URL / SLACK_SIGNING_SECRET env
// vars. Init must be called first.
func InitSlack() {
	slackRestoreConfig()

	slackCfg.mu.Lock()
	if slackCfg.webhookURL == "" {
		slackCfg.webhookURL = os.Getenv("SLACK_WEBHOOK_URL")
	}
	if slackCfg.signingSecret == "" {
		slackCfg.signingSecret = os.Getenv("SLACK_SIGNING_SECRET")
	}
	slackCfg.mu.Unlock()

	if SlackIsEnabled() {
		_, secret := slackGetConfig()
		slog.Info("slack: notifications enabled", "interactive", secret != "")
	}
}

func slackRestoreConfig() {
	if clientset == nil {
		return
	}
	c, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	cm, err := clientset.CoreV1().ConfigMaps(jitCMNamespace).Get(c, SlackCMName, metav1.GetOptions{})
	if err != nil {
		return
	}

	slackCfg.mu.Lock()
	slackCfg.webhookURL = cm.Data["slack_webhook_url"]
	slackCfg.signingSecret = cm.Data["slack_signing_secret"]
	slackCfg.mu.Unlock()
}

func slackPersistConfig() {
	if clientset == nil {
		return
	}
	webhookURL, signingSecret := slackGetConfig()

	c, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	data := map[string]string{
		"slack_webhook_url":    webhookURL,
		"slack_signing_secret": signingSecret,
	}

	cm, err := clientset.CoreV1().ConfigMaps(jitCMNamespace).Get(c, SlackCMName, metav1.GetOptions{})
	if k8serr.IsNotFound(err) {
		newCM := &corev1.ConfigMap{
			ObjectMeta: metav1.ObjectMeta{
				Name:      SlackCMName,
				Namespace: jitCMNamespace,
				Labels:    map[string]string{"app.kubernetes.io/managed-by": "kube-argus"},
			},
			Data: data,
		}
		if _, err := clientset.CoreV1().ConfigMaps(jitCMNamespace).Create(c, newCM, metav1.CreateOptions{}); err != nil {
			slog.Error("slack: persist create failed", "error", err)
		}
		return
	}
	if err != nil {
		slog.Error("slack: persist get failed", "error", err)
		return
	}

	if cm.Data == nil {
		cm.Data = make(map[string]string)
	}
	cm.Data["slack_webhook_url"] = webhookURL
	cm.Data["slack_signing_secret"] = signingSecret

	if _, err := clientset.CoreV1().ConfigMaps(jitCMNamespace).Update(c, cm, metav1.UpdateOptions{}); err != nil {
		slog.Error("slack: persist update failed", "error", err)
	}
}

// ─── Settings API ───────────────────────────────────────────────────

// SlackSettingsHandler handles /api/settings/slack — GET (masked view),
// PUT (update), POST (send a test message).
func SlackSettingsHandler(w http.ResponseWriter, r *http.Request) {
	if !RequireAdmin(w, r) {
		return
	}

	switch r.Method {
	case "GET":
		webhookURL, signingSecret := slackGetConfig()
		// Mask secrets for display
		maskedWebhook := ""
		if webhookURL != "" {
			maskedWebhook = webhookURL[:minInt(30, len(webhookURL))] + "••••••"
		}
		maskedSecret := ""
		if signingSecret != "" {
			maskedSecret = "••••••••"
		}
		httpx.JSON(w, map[string]any{
			"webhookURL":    maskedWebhook,
			"signingSecret": maskedSecret,
			"enabled":       webhookURL != "",
			"interactive":   signingSecret != "",
		})

	case "PUT":
		var body map[string]string
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			httpx.Error(w, "invalid JSON", 400)
			return
		}

		slackCfg.mu.Lock()
		if val, ok := body["webhookURL"]; ok {
			slackCfg.webhookURL = val
		}
		if val, ok := body["signingSecret"]; ok {
			slackCfg.signingSecret = val
		}
		slackCfg.mu.Unlock()

		go slackPersistConfig()

		if email, role, ok := CurrentUser(r); ok {
			AuditRecord(email, role, "settings.slack", "", "updated", ClientIP(r))
		}

		httpx.JSON(w, map[string]string{"status": "ok"})

	case "POST":
		var testBody struct {
			WebhookURL string `json:"webhookURL"`
		}
		json.NewDecoder(r.Body).Decode(&testBody)
		webhookURL := testBody.WebhookURL
		if webhookURL == "" {
			webhookURL, _ = slackGetConfig()
		}
		if webhookURL == "" {
			httpx.Error(w, `{"error":"webhook URL not provided"}`, 400)
			return
		}
		blocks := []map[string]any{
			{
				"type": "section",
				"text": map[string]any{
					"type": "mrkdwn",
					"text": fmt.Sprintf(":white_check_mark: *Kube-Argus Slack integration is working!* (%s)\nYou will receive JIT access request notifications in this channel.", Cluster()),
				},
			},
		}
		raw, _ := json.Marshal(map[string]any{"blocks": blocks})
		resp, err := http.Post(webhookURL, "application/json", bytes.NewReader(raw))
		if err != nil {
			httpx.Error(w, fmt.Sprintf("failed to reach Slack: %s", err.Error()), 502)
			return
		}
		resp.Body.Close()
		if resp.StatusCode != 200 {
			httpx.Error(w, fmt.Sprintf("Slack returned status %d", resp.StatusCode), 502)
			return
		}
		httpx.JSON(w, map[string]string{"status": "ok"})

	default:
		httpx.Error(w, "method not allowed", 405)
	}
}

// ─── JIT Notifications ──────────────────────────────────────────────

func slackNotifyJITRequested(req *JITPayload) {
	webhookURL, signingSecret := slackGetConfig()
	if webhookURL == "" {
		return
	}

	resource := req.Namespace
	if req.OwnerKind != "" && req.OwnerName != "" {
		resource += " / " + req.OwnerKind + " " + req.OwnerName
	} else if req.Pod != "" {
		resource += " / Pod " + req.Pod
	}

	blocks := []map[string]any{
		{
			"type": "header",
			"text": map[string]any{"type": "plain_text", "text": "JIT Access Request - " + Cluster()},
		},
		{
			"type": "section",
			"fields": []map[string]any{
				{"type": "mrkdwn", "text": fmt.Sprintf("*Requester:*\n%s", req.Email)},
				{"type": "mrkdwn", "text": fmt.Sprintf("*Duration:*\n%s", req.Duration)},
				{"type": "mrkdwn", "text": fmt.Sprintf("*Resource:*\n%s", resource)},
				{"type": "mrkdwn", "text": fmt.Sprintf("*Reason:*\n%s", req.Reason)},
			},
		},
	}

	if signingSecret != "" {
		blocks = append(blocks, map[string]any{
			"type":     "actions",
			"block_id": "jit_actions_" + req.ID,
			"elements": []map[string]any{
				{
					"type":      "button",
					"text":      map[string]any{"type": "plain_text", "text": "Approve"},
					"style":     "primary",
					"action_id": "jit_approve",
					"value":     req.ID,
				},
				{
					"type":      "button",
					"text":      map[string]any{"type": "plain_text", "text": "Deny"},
					"style":     "danger",
					"action_id": "jit_deny",
					"value":     req.ID,
				},
			},
		})
	}

	slackPost(webhookURL, map[string]any{"blocks": blocks})
}

func slackNotifyJITResult(req *JITPayload, action, actor string) {
	webhookURL, _ := slackGetConfig()
	if webhookURL == "" {
		return
	}

	resource := req.Namespace
	if req.OwnerKind != "" && req.OwnerName != "" {
		resource += " / " + req.OwnerKind + " " + req.OwnerName
	}

	var emoji, verb string
	switch action {
	case "approve":
		emoji, verb = ":white_check_mark:", "approved"
	case "deny":
		emoji, verb = ":x:", "denied"
	case "revoke":
		emoji, verb = ":no_entry_sign:", "revoked"
	}

	blocks := []map[string]any{
		{
			"type": "section",
			"text": map[string]any{
				"type": "mrkdwn",
				"text": fmt.Sprintf("%s *JIT request %s* by *%s* (%s)\n*Requester:* %s\n*Resource:* %s",
					emoji, verb, actor, Cluster(), req.Email, resource),
			},
		},
	}

	slackPost(webhookURL, map[string]any{"blocks": blocks})
}

func slackPost(url string, payload map[string]any) {
	raw, err := json.Marshal(payload)
	if err != nil {
		slog.Warn("slack: marshal failed", "error", err)
		return
	}
	go func() {
		resp, err := http.Post(url, "application/json", bytes.NewReader(raw))
		if err != nil {
			slog.Warn("slack: post failed", "error", err)
			return
		}
		resp.Body.Close()
		if resp.StatusCode != 200 {
			slog.Warn("slack: non-200 response", "status", resp.StatusCode)
		}
	}()
}

// ─── Slack Interactive Endpoint ─────────────────────────────────────

// SlackInteractHandler verifies Slack-signed POSTs and dispatches button
// clicks to the JIT approve/deny callbacks wired by main.
func SlackInteractHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != "POST" {
		httpx.Error(w, "POST only", 405)
		return
	}
	_, signingSecret := slackGetConfig()
	if signingSecret == "" {
		httpx.Error(w, "interactive messages not configured", 503)
		return
	}

	body, err := io.ReadAll(r.Body)
	if err != nil {
		httpx.Error(w, "bad request", 400)
		return
	}

	if !verifySlackSignature(r.Header, body, signingSecret) {
		httpx.Error(w, "invalid signature", 401)
		return
	}

	parsed, err := url.ParseQuery(string(body))
	if err != nil {
		httpx.Error(w, "bad form data", 400)
		return
	}
	payloadStr := parsed.Get("payload")
	if payloadStr == "" {
		httpx.Error(w, "missing payload", 400)
		return
	}

	var payload struct {
		Type string `json:"type"`
		User struct {
			ID       string `json:"id"`
			Username string `json:"username"`
			Name     string `json:"name"`
		} `json:"user"`
		Actions []struct {
			ActionID string `json:"action_id"`
			Value    string `json:"value"`
		} `json:"actions"`
		ResponseURL string `json:"response_url"`
	}
	if err := json.Unmarshal([]byte(payloadStr), &payload); err != nil {
		httpx.Error(w, "invalid payload", 400)
		return
	}

	if len(payload.Actions) == 0 {
		w.WriteHeader(200)
		return
	}

	act := payload.Actions[0]
	reqID := act.Value
	actor := payload.User.Name
	if actor == "" {
		actor = payload.User.Username
	}

	var resultText string

	switch act.ActionID {
	case "jit_approve":
		if err := JITApprove(reqID, actor, "slack"); err != nil {
			resultText = fmt.Sprintf(":warning: Could not approve: %s", err.Error())
		} else {
			resultText = fmt.Sprintf(":white_check_mark: *Approved* by %s", actor)
		}
	case "jit_deny":
		if err := JITDeny(reqID, actor, "slack"); err != nil {
			resultText = fmt.Sprintf(":warning: Could not deny: %s", err.Error())
		} else {
			resultText = fmt.Sprintf(":x: *Denied* by %s", actor)
		}
	default:
		w.WriteHeader(200)
		return
	}

	if payload.ResponseURL != "" {
		req := JITLookup(reqID)
		var resource, email, duration, reason string
		if req != nil {
			email = req.Email
			duration = req.Duration
			reason = req.Reason
			resource = req.Namespace
			if req.OwnerKind != "" && req.OwnerName != "" {
				resource += " / " + req.OwnerKind + " " + req.OwnerName
			} else if req.Pod != "" {
				resource += " / Pod " + req.Pod
			}
		}

		updateBlocks := []map[string]any{
			{
				"type": "header",
				"text": map[string]any{"type": "plain_text", "text": "JIT Access Request - " + Cluster()},
			},
			{
				"type": "section",
				"fields": []map[string]any{
					{"type": "mrkdwn", "text": fmt.Sprintf("*Requester:*\n%s", email)},
					{"type": "mrkdwn", "text": fmt.Sprintf("*Duration:*\n%s", duration)},
					{"type": "mrkdwn", "text": fmt.Sprintf("*Resource:*\n%s", resource)},
					{"type": "mrkdwn", "text": fmt.Sprintf("*Reason:*\n%s", reason)},
				},
			},
			{
				"type": "section",
				"text": map[string]any{"type": "mrkdwn", "text": resultText},
			},
		}

		updatePayload, _ := json.Marshal(map[string]any{
			"replace_original": true,
			"blocks":           updateBlocks,
		})

		go func() {
			req, _ := http.NewRequest("POST", payload.ResponseURL, bytes.NewReader(updatePayload))
			req.Header.Set("Content-Type", "application/json")
			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				slog.Warn("slack: response_url update failed", "error", err)
				return
			}
			resp.Body.Close()
		}()
	}

	w.WriteHeader(200)
}

func verifySlackSignature(headers http.Header, body []byte, secret string) bool {
	timestamp := headers.Get("X-Slack-Request-Timestamp")
	sig := headers.Get("X-Slack-Signature")
	if timestamp == "" || sig == "" {
		return false
	}

	ts, err := strconv.ParseInt(timestamp, 10, 64)
	if err != nil {
		return false
	}
	diff := time.Now().Unix() - ts
	if diff < 0 {
		diff = -diff
	}
	if diff > 300 {
		return false
	}

	baseString := "v0:" + timestamp + ":" + string(body)
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(baseString))
	expected := "v0=" + hex.EncodeToString(mac.Sum(nil))

	return hmac.Equal([]byte(sig), []byte(expected))
}

func minInt(a, b int) int {
	if a < b {
		return a
	}
	return b
}
