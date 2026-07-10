// Package auth handles OIDC / Google login, session cookies, and role checks.
package auth

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"strings"
	"time"

	"kube-argus/internal/httpx"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/aws/session"
	"github.com/aws/aws-sdk-go/service/secretsmanager"
	"github.com/coreos/go-oidc/v3/oidc"
	"golang.org/x/oauth2"
)

// ─── Auth (OIDC / Google / None) ────────────────────────────────────

var (
	oauthConfig    *oauth2.Config
	oidcProvider   *oidc.Provider
	oidcVerifier   *oidc.IDTokenVerifier
	sessionKey     []byte
	oidcAdminGroup string
	oidcIssuer     string
	adminEmails    map[string]bool

	// Enabled reports whether OIDC/Google login is configured.
	Enabled bool
	// Mode is one of "google", "oidc", or "none".
	Mode string
	// DefaultRole applies when auth is disabled. Either "admin" or "viewer".
	DefaultRole string
	// SessionTTL is the lifetime of a signed session cookie.
	SessionTTL time.Duration
	// CORSOrigin is the value used by the CORS middleware ("*" if empty).
	CORSOrigin string
)

// SessionData is the payload encoded into the kubeargus_session cookie.
type SessionData struct {
	Email string `json:"email"`
	Role  string `json:"role"`
	Exp   int64  `json:"exp"`
}

type ctxKey string

// UserCtxKey is the context key used to attach SessionData to requests.
const UserCtxKey ctxKey = "user"

// Hooks wired by main so this package stays free of cross-package imports.
var (
	// AuditRecord persists an audit-trail entry. Set by main.
	AuditRecord = func(actor, role, action, target, detail, ip string) {}
	// TrackUser is called from the auth middleware whenever a request carries a
	// valid session. main wires this to the presence tracker.
	TrackUser = func(email, role, ip string) {}
	// HasActiveJIT reports whether the user has an active JIT grant for the
	// given workload. Set by main; lets RequireAdminOrJIT live in this package.
	HasActiveJIT = func(email, namespace, ownerKind, ownerName string) bool { return false }
)

// envWithFallback reads primary env var, falls back to legacy name if empty.
func envWithFallback(primary, legacy string) string {
	if v := os.Getenv(primary); v != "" {
		return v
	}
	return os.Getenv(legacy)
}

// LoadSecretsFromAWS pulls secrets from AWS Secrets Manager into the process
// env (no-op if AWS_SECRET_NAME is unset). Must run before Init.
func LoadSecretsFromAWS() {
	secretName := os.Getenv("AWS_SECRET_NAME")
	if secretName == "" {
		slog.Info("AWS_SECRET_NAME not set, skipping Secrets Manager")
		return
	}
	region := os.Getenv("AWS_REGION")
	if region == "" {
		region = os.Getenv("AWS_DEFAULT_REGION")
	}
	if region == "" {
		region = "us-east-1"
	}

	sess, err := session.NewSession(&aws.Config{Region: aws.String(region)})
	if err != nil {
		slog.Warn("AWS session init failed, secrets won't load from SM", "error", err)
		return
	}
	svc := secretsmanager.New(sess)
	result, err := svc.GetSecretValue(&secretsmanager.GetSecretValueInput{
		SecretId: aws.String(secretName),
	})
	if err != nil {
		slog.Error("failed to fetch secret", "secret", secretName, "error", err)
		return
	}
	if result.SecretString == nil {
		slog.Warn("secret has no string value", "secret", secretName)
		return
	}

	var secrets map[string]string
	if err := json.Unmarshal([]byte(*result.SecretString), &secrets); err != nil {
		slog.Error("failed to parse secret JSON", "error", err)
		return
	}

	envKeys := []string{"OIDC_ISSUER", "OIDC_CLIENT_ID", "OIDC_CLIENT_SECRET", "SESSION_SECRET", "OIDC_ADMIN_GROUP", "GOOGLE_CLIENT_ID", "GOOGLE_CLIENT_SECRET", "ADMIN_EMAILS", "DEFAULT_ROLE", "CLUSTER_NAME", "LLM_GATEWAY_URL", "LLM_GATEWAY_KEY", "LLM_GATEWAY_MODEL", "PROMETHEUS_URL", "PROMETHEUS_USER", "PROMETHEUS_KEY", "SLACK_WEBHOOK_URL", "SLACK_SIGNING_SECRET", "NOTIFY_WEBHOOK_URL", "NOTIFY_WEBHOOK_SECRET"}
	loaded := 0
	for _, k := range envKeys {
		if v, ok := secrets[k]; ok && v != "" && os.Getenv(k) == "" {
			os.Setenv(k, v)
			loaded++
		}
	}
	slog.Info("loaded auth secrets from AWS Secrets Manager", "count", loaded, "secret", secretName)
}

// Init configures the auth subsystem from env vars. Call once at startup.
func Init() {
	SessionTTL = 8 * time.Hour
	if ttl := os.Getenv("SESSION_TTL"); ttl != "" {
		if d, err := time.ParseDuration(ttl); err == nil && d > 0 {
			SessionTTL = d
		}
	}

	CORSOrigin = os.Getenv("CORS_ORIGIN")

	DefaultRole = os.Getenv("DEFAULT_ROLE")
	if DefaultRole != "admin" && DefaultRole != "viewer" {
		DefaultRole = "viewer"
	}

	adminEmails = map[string]bool{}
	if raw := os.Getenv("ADMIN_EMAILS"); raw != "" {
		for _, e := range strings.Split(raw, ",") {
			if trimmed := strings.TrimSpace(strings.ToLower(e)); trimmed != "" {
				adminEmails[trimmed] = true
			}
		}
	}

	oidcAdminGroup = envWithFallback("OIDC_ADMIN_GROUP", "OKTA_ADMIN_GROUP")
	if oidcAdminGroup == "" {
		oidcAdminGroup = "admin"
	}

	googleClientID := os.Getenv("GOOGLE_CLIENT_ID")
	googleClientSecret := os.Getenv("GOOGLE_CLIENT_SECRET")
	oidcClientID := envWithFallback("OIDC_CLIENT_ID", "OKTA_CLIENT_ID")
	oidcClientSecret := envWithFallback("OIDC_CLIENT_SECRET", "OKTA_CLIENT_SECRET")
	oidcIssuerEnv := envWithFallback("OIDC_ISSUER", "OKTA_ISSUER")

	var issuer, clientID, clientSecret string
	var scopes []string

	switch {
	case googleClientID != "" && googleClientSecret != "":
		Mode = "google"
		issuer = "https://accounts.google.com"
		clientID = googleClientID
		clientSecret = googleClientSecret
		scopes = []string{oidc.ScopeOpenID, "profile", "email"}

	case oidcIssuerEnv != "" && oidcClientID != "" && oidcClientSecret != "":
		Mode = "oidc"
		issuer = oidcIssuerEnv
		clientID = oidcClientID
		clientSecret = oidcClientSecret
		scopes = []string{oidc.ScopeOpenID, "profile", "email", "groups"}

	default:
		Mode = "none"
		Enabled = false
		slog.Info("auth mode: none", "default_role", DefaultRole)
		return
	}

	oidcIssuer = issuer

	secret := os.Getenv("SESSION_SECRET")
	if secret == "" {
		b := make([]byte, 32)
		rand.Read(b)
		sessionKey = b
		slog.Warn("SESSION_SECRET not set, generated random key (sessions won't survive restarts)")
	} else {
		var err error
		sessionKey, err = hex.DecodeString(secret)
		if err != nil {
			sessionKey = []byte(secret)
		}
	}

	ctx := context.Background()
	var err error
	oidcProvider, err = oidc.NewProvider(ctx, issuer)
	if err != nil {
		slog.Error("OIDC provider init failed", "error", err)
		os.Exit(1)
	}

	oauthConfig = &oauth2.Config{
		ClientID:     clientID,
		ClientSecret: clientSecret,
		Endpoint:     oidcProvider.Endpoint(),
		Scopes:       scopes,
	}

	oidcVerifier = oidcProvider.Verifier(&oidc.Config{ClientID: clientID})
	Enabled = true
	slog.Info("auth mode configured", "mode", Mode, "issuer", issuer)
}

func signSession(sd SessionData) (string, error) {
	payload, _ := json.Marshal(sd)
	b64 := base64.RawURLEncoding.EncodeToString(payload)
	mac := hmac.New(sha256.New, sessionKey)
	mac.Write([]byte(b64))
	sig := hex.EncodeToString(mac.Sum(nil))
	return b64 + "." + sig, nil
}

func verifySession(cookie string) (*SessionData, error) {
	parts := strings.SplitN(cookie, ".", 2)
	if len(parts) != 2 {
		return nil, fmt.Errorf("invalid session format")
	}
	mac := hmac.New(sha256.New, sessionKey)
	mac.Write([]byte(parts[0]))
	expected := hex.EncodeToString(mac.Sum(nil))
	if !hmac.Equal([]byte(parts[1]), []byte(expected)) {
		return nil, fmt.Errorf("invalid signature")
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[0])
	if err != nil {
		return nil, err
	}
	var sd SessionData
	if err := json.Unmarshal(payload, &sd); err != nil {
		return nil, err
	}
	if time.Now().Unix() > sd.Exp {
		return nil, fmt.Errorf("session expired")
	}
	return &sd, nil
}

func setSessionCookie(w http.ResponseWriter, sd SessionData) {
	val, err := signSession(sd)
	if err != nil {
		return
	}
	http.SetCookie(w, &http.Cookie{
		Name:     "kubeargus_session",
		Value:    val,
		Path:     "/",
		HttpOnly: true,
		Secure:   os.Getenv("INSECURE_COOKIE") != "true",
		SameSite: http.SameSiteLaxMode,
		MaxAge:   int(SessionTTL.Seconds()),
	})
}

func clearSessionCookie(w http.ResponseWriter) {
	http.SetCookie(w, &http.Cookie{
		Name:     "kubeargus_session",
		Value:    "",
		Path:     "/",
		HttpOnly: true,
		MaxAge:   -1,
	})
}

func callbackURL(r *http.Request) string {
	scheme := "https"
	if r.TLS == nil {
		if fwd := r.Header.Get("X-Forwarded-Proto"); fwd != "" {
			scheme = fwd
		} else {
			scheme = "http"
		}
	}
	host := r.Host
	if fwd := r.Header.Get("X-Forwarded-Host"); fwd != "" {
		host = fwd
	}
	return scheme + "://" + host + "/auth/callback"
}

// LoginHandler initiates the OIDC authorization flow.
func LoginHandler(w http.ResponseWriter, r *http.Request) {
	if oauthConfig == nil {
		httpx.Error(w, "authentication not configured — set OIDC_ISSUER, OIDC_CLIENT_ID, and OIDC_CLIENT_SECRET", 500)
		return
	}
	oauthConfig.RedirectURL = callbackURL(r)
	state := fmt.Sprintf("%d", time.Now().UnixNano())
	http.SetCookie(w, &http.Cookie{Name: "oauth_state", Value: state, Path: "/", HttpOnly: true, MaxAge: 600, SameSite: http.SameSiteLaxMode, Secure: r.TLS != nil || r.Header.Get("X-Forwarded-Proto") == "https"})
	http.Redirect(w, r, oauthConfig.AuthCodeURL(state), http.StatusFound)
}

// CallbackHandler handles the OIDC redirect with an authorization code.
func CallbackHandler(w http.ResponseWriter, r *http.Request) {
	if oauthConfig == nil {
		httpx.Error(w, "authentication not configured", 500)
		return
	}
	if r.URL.Query().Get("code") == "" {
		http.Redirect(w, r, "/auth/login", http.StatusFound)
		return
	}

	stateCookie, err := r.Cookie("oauth_state")
	if err != nil || r.URL.Query().Get("state") != stateCookie.Value {
		http.Redirect(w, r, "/auth/login", http.StatusFound)
		return
	}
	http.SetCookie(w, &http.Cookie{Name: "oauth_state", Value: "", Path: "/", MaxAge: -1})

	oauthConfig.RedirectURL = callbackURL(r)
	token, err := oauthConfig.Exchange(r.Context(), r.URL.Query().Get("code"))
	if err != nil {
		httpx.Error(w, "token exchange failed: "+err.Error(), 500)
		return
	}

	rawIDToken, ok := token.Extra("id_token").(string)
	if !ok {
		httpx.Error(w, "no id_token in response", 500)
		return
	}

	idToken, err := oidcVerifier.Verify(r.Context(), rawIDToken)
	if err != nil {
		httpx.Error(w, "token verify failed: "+err.Error(), 500)
		return
	}

	var claims struct {
		Email  string   `json:"email"`
		Groups []string `json:"groups"`
	}
	if err := idToken.Claims(&claims); err != nil {
		httpx.Error(w, "claims parse failed: "+err.Error(), 500)
		return
	}

	role := "viewer"
	if adminEmails[strings.ToLower(claims.Email)] {
		role = "admin"
	} else {
		for _, g := range claims.Groups {
			if g == oidcAdminGroup {
				role = "admin"
				break
			}
		}
	}

	sd := SessionData{Email: claims.Email, Role: role, Exp: time.Now().Add(SessionTTL).Unix()}
	setSessionCookie(w, sd)
	slog.Info("auth: user logged in", "email", claims.Email, "role", role)
	AuditRecord(claims.Email, role, "login", "", "role: "+role, ClientIP(r))

	redirectTo := "/"
	if rc, err := r.Cookie("kubeargus_return"); err == nil && rc.Value != "" && strings.HasPrefix(rc.Value, "/") {
		redirectTo = rc.Value
		http.SetCookie(w, &http.Cookie{Name: "kubeargus_return", Value: "", Path: "/", MaxAge: -1})
	}
	http.Redirect(w, r, redirectTo, http.StatusFound)
}

// LogoutHandler clears the session cookie and (when supported) redirects to
// the IdP end-session endpoint.
func LogoutHandler(w http.ResponseWriter, r *http.Request) {
	if sd, ok := r.Context().Value(UserCtxKey).(*SessionData); ok && sd != nil {
		AuditRecord(sd.Email, sd.Role, "logout", "", "", ClientIP(r))
	}
	clearSessionCookie(w)
	if oidcIssuer != "" && oidcProvider != nil && oauthConfig != nil {
		scheme := "https"
		if r.TLS == nil && r.Header.Get("X-Forwarded-Proto") == "" {
			scheme = "http"
		}
		postRedirect := scheme + "://" + r.Host + "/"

		var claims struct {
			EndSessionEndpoint string `json:"end_session_endpoint"`
		}
		if err := oidcProvider.Claims(&claims); err == nil && claims.EndSessionEndpoint != "" {
			logoutURL := claims.EndSessionEndpoint +
				"?client_id=" + oauthConfig.ClientID +
				"&post_logout_redirect_uri=" + postRedirect
			http.Redirect(w, r, logoutURL, http.StatusFound)
			return
		}

		http.Redirect(w, r, postRedirect, http.StatusFound)
		return
	}
	http.Redirect(w, r, "/", http.StatusFound)
}

// MeHandler returns the current user identity (or anonymous when auth is off).
func MeHandler(w http.ResponseWriter, r *http.Request) {
	if !Enabled {
		httpx.JSON(w, map[string]string{"email": "anonymous", "role": DefaultRole, "authMode": Mode})
		return
	}
	sd, ok := r.Context().Value(UserCtxKey).(*SessionData)
	if !ok || sd == nil {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(401)
		json.NewEncoder(w).Encode(map[string]string{"error": "unauthorized", "authMode": Mode})
		return
	}
	httpx.JSON(w, map[string]string{"email": sd.Email, "role": sd.Role, "authMode": Mode})
}

// Middleware enforces a valid session cookie on protected paths.
func Middleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !Enabled {
			next.ServeHTTP(w, r)
			return
		}

		path := r.URL.Path
		if path == "/health" || strings.HasPrefix(path, "/auth/") || strings.HasPrefix(path, "/api/slack/") {
			next.ServeHTTP(w, r)
			return
		}

		cookie, err := r.Cookie("kubeargus_session")
		if err != nil {
			if strings.HasPrefix(path, "/api/") {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(401)
				json.NewEncoder(w).Encode(map[string]string{"error": "unauthorized"})
			} else {
				returnTo := r.URL.RequestURI()
				if returnTo != "" && returnTo != "/" {
					http.SetCookie(w, &http.Cookie{Name: "kubeargus_return", Value: returnTo, Path: "/", HttpOnly: true, MaxAge: 300, SameSite: http.SameSiteLaxMode})
				}
				http.Redirect(w, r, "/auth/login", http.StatusFound)
			}
			return
		}

		sd, err := verifySession(cookie.Value)
		if err != nil {
			clearSessionCookie(w)
			if strings.HasPrefix(path, "/api/") {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(401)
				json.NewEncoder(w).Encode(map[string]string{"error": "unauthorized"})
			} else {
				returnTo := r.URL.RequestURI()
				if returnTo != "" && returnTo != "/" {
					http.SetCookie(w, &http.Cookie{Name: "kubeargus_return", Value: returnTo, Path: "/", HttpOnly: true, MaxAge: 300, SameSite: http.SameSiteLaxMode})
				}
				http.Redirect(w, r, "/auth/login", http.StatusFound)
			}
			return
		}

		TrackUser(sd.Email, sd.Role, ClientIP(r))
		ctx := context.WithValue(r.Context(), UserCtxKey, sd)
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

// RequireAdmin writes a 403 and returns false if the request is not from an
// admin user. The "no auth + DEFAULT_ROLE=admin" case still returns true.
func RequireAdmin(w http.ResponseWriter, r *http.Request) bool {
	if !Enabled {
		if DefaultRole == "admin" {
			return true
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(403)
		json.NewEncoder(w).Encode(map[string]string{"error": "forbidden", "message": "admin access required — set DEFAULT_ROLE=admin or sign in with an admin account"})
		return false
	}
	sd, ok := r.Context().Value(UserCtxKey).(*SessionData)
	if !ok || sd == nil || sd.Role != "admin" {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(403)
		json.NewEncoder(w).Encode(map[string]string{"error": "forbidden", "message": "admin access required"})
		return false
	}
	return true
}

// IsAdmin returns true iff the request is from an admin user (or auth is off
// and DEFAULT_ROLE=admin). Same semantics as RequireAdmin without the 403.
func IsAdmin(r *http.Request) bool {
	if !Enabled {
		return DefaultRole == "admin"
	}
	sd, ok := r.Context().Value(UserCtxKey).(*SessionData)
	return ok && sd != nil && sd.Role == "admin"
}

// RequireAdminOrJIT allows access if the user is admin OR has an active JIT
// grant for the workload. Writes a 403 response on failure.
func RequireAdminOrJIT(w http.ResponseWriter, r *http.Request, namespace, ownerKind, ownerName string) bool {
	email := "anonymous"
	role := DefaultRole

	if Enabled {
		sd, ok := r.Context().Value(UserCtxKey).(*SessionData)
		if !ok || sd == nil {
			slog.Warn("jit-exec: denied, no session", "namespace", namespace, "owner_kind", ownerKind, "owner_name", ownerName)
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(403)
			json.NewEncoder(w).Encode(map[string]string{"error": "forbidden"})
			return false
		}
		email = sd.Email
		role = sd.Role
	}

	if role == "admin" {
		return true
	}

	if HasActiveJIT(email, namespace, ownerKind, ownerName) {
		slog.Info("jit-exec: granted via JIT", "email", email, "namespace", namespace, "owner_kind", ownerKind, "owner_name", ownerName)
		return true
	}

	slog.Warn("jit-exec: denied, no active JIT grant", "email", email, "role", role, "namespace", namespace, "owner_kind", ownerKind, "owner_name", ownerName)
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(403)
	json.NewEncoder(w).Encode(map[string]string{"error": "forbidden", "message": "admin access or approved JIT request required"})
	return false
}

// Session pulls the SessionData attached to the request context, if any.
func Session(r *http.Request) *SessionData {
	sd, _ := r.Context().Value(UserCtxKey).(*SessionData)
	return sd
}

// ClientIP returns the originating IP, honoring X-Forwarded-For.
func ClientIP(r *http.Request) string {
	if fwd := r.Header.Get("X-Forwarded-For"); fwd != "" {
		parts := strings.SplitN(fwd, ",", 2)
		return strings.TrimSpace(parts[0])
	}
	host, _, _ := strings.Cut(r.RemoteAddr, ":")
	return host
}
