package api

import (
	"strings"
	"testing"
)

// redactValues should selectively redact only strings under sensitive-looking
// keys — leaving mundane configuration (replicas, hostnames, image tags,
// resource references) visible.
func TestRedactValues_SensitiveKeysOnly(t *testing.T) {
	in := map[string]any{
		"username":  "alice",                  // not in sensitive list → keep
		"password":  "s3cret",                 // sensitive → redact
		"image":     map[string]any{"tag": "1.7.0", "repository": "appsmith/appsmith"},
		"replicas":  3,                        // number → never redacted
		"enabled":   true,                     // bool → never redacted
		"empty":     nil,                      // nil → preserved
		"db": map[string]any{
			"host":     "db.prod.internal",   // ordinary → keep
			"port":     5432,                  // number → keep
			"password": "real-secret-value",   // sensitive → redact
			"dsn":      "postgres://user:pw@host/db", // dsn pattern → redact
			"credentials": map[string]any{
				"token": "ghp_xxxxx",          // sensitive → redact
			},
		},
		// Resource-reference cases: keys end in Name/Ref/etc., values are
		// kubernetes resource names, NOT the secret itself → MUST preserve.
		"existingSecretName": "db-creds-secret",
		"imagePullSecrets":   []any{map[string]any{"name": "ghcr-pull"}},
		"tlsSecretRef":       "frontend-tls",
		"tokenName":          "my-sa-token",
	}

	out, ok := redactValues(in).(map[string]any)
	if !ok {
		t.Fatal("expected map result")
	}

	// Sensitive keys redacted
	if out["password"] != redactPlaceholder {
		t.Errorf("password should be redacted, got %v", out["password"])
	}
	db := out["db"].(map[string]any)
	if db["password"] != redactPlaceholder {
		t.Errorf("nested db.password should be redacted, got %v", db["password"])
	}
	if db["dsn"] != redactPlaceholder {
		t.Errorf("db.dsn should be redacted, got %v", db["dsn"])
	}
	creds := db["credentials"].(map[string]any)
	if creds["token"] != redactPlaceholder {
		t.Errorf("db.credentials.token should be redacted, got %v", creds["token"])
	}

	// Non-sensitive scalars preserved
	if out["username"] != "alice" {
		t.Errorf("username should NOT be redacted, got %v", out["username"])
	}
	if out["replicas"] != 3 {
		t.Errorf("replicas (int) should be preserved, got %v", out["replicas"])
	}
	if out["enabled"] != true {
		t.Errorf("enabled (bool) should be preserved, got %v", out["enabled"])
	}
	if out["empty"] != nil {
		t.Errorf("nil should be preserved, got %v", out["empty"])
	}
	img := out["image"].(map[string]any)
	if img["tag"] != "1.7.0" || img["repository"] != "appsmith/appsmith" {
		t.Errorf("image fields should be preserved, got %v", img)
	}
	if db["host"] != "db.prod.internal" {
		t.Errorf("db.host should be preserved, got %v", db["host"])
	}

	// Resource-reference keys preserved (not the secret itself)
	if out["existingSecretName"] != "db-creds-secret" {
		t.Errorf("existingSecretName (Name suffix) should NOT be redacted, got %v", out["existingSecretName"])
	}
	if out["tlsSecretRef"] != "frontend-tls" {
		t.Errorf("tlsSecretRef (Ref suffix) should NOT be redacted, got %v", out["tlsSecretRef"])
	}
	if out["tokenName"] != "my-sa-token" {
		t.Errorf("tokenName (Name suffix) should NOT be redacted, got %v", out["tokenName"])
	}
	pull := out["imagePullSecrets"].([]any)[0].(map[string]any)
	if pull["name"] != "ghcr-pull" {
		t.Errorf("imagePullSecrets[].name should NOT be redacted, got %v", pull["name"])
	}
}

// A value wrapped in a sub-object under a sensitive key must be caught,
// e.g. `password: { value: "actual-secret", description: "prod" }`.
// Common in Helm charts that support both direct string and structured
// forms. We want:
//   - the value-holder ("value") string redacted
//   - the description string preserved (it's metadata, not the secret)
//   - non-string siblings preserved
func TestRedactValues_ValueHolderUnderSensitiveKey(t *testing.T) {
	in := map[string]any{
		"password": map[string]any{
			"value":       "actual-secret",
			"description": "prod DB password",
			"rotated":     true,
			"length":      32,
		},
		"apiKey": map[string]any{
			"raw":    "sk-abcdef",
			"source": "vault",
		},
		// Untainted branch: no sensitive ancestor, value-holder harmless
		"logging": map[string]any{
			"value": "info",
			"raw":   "structured",
		},
	}

	out := redactValues(in).(map[string]any)

	pw := out["password"].(map[string]any)
	if pw["value"] != redactPlaceholder {
		t.Errorf("password.value should be redacted, got %v", pw["value"])
	}
	if pw["description"] != "prod DB password" {
		t.Errorf("password.description should be preserved, got %v", pw["description"])
	}
	if pw["rotated"] != true || pw["length"] != 32 {
		t.Errorf("non-string siblings should be preserved, got %v", pw)
	}

	api := out["apiKey"].(map[string]any)
	if api["raw"] != redactPlaceholder {
		t.Errorf("apiKey.raw should be redacted, got %v", api["raw"])
	}
	if api["source"] != "vault" {
		t.Errorf("apiKey.source should be preserved, got %v", api["source"])
	}

	// Untainted branch — no sensitive ancestor
	lg := out["logging"].(map[string]any)
	if lg["value"] != "info" || lg["raw"] != "structured" {
		t.Errorf("logging.* should be preserved (no sensitive ancestor), got %v", lg)
	}
}

// Subcharts share the same layout and can embed secrets in their defaults.
// redactReleaseForViewer must recurse into every chart.dependencies[].
func TestRedactReleaseForViewer_RecursesSubcharts(t *testing.T) {
	rel := map[string]any{
		"chart": map[string]any{
			"values": map[string]any{"password": "top-level"},
			"dependencies": []any{
				map[string]any{
					"metadata": map[string]any{"name": "redis"},
					"values":   map[string]any{"authPassword": "subchart-secret"},
					"files":    []any{map[string]any{"name": "creds", "data": "b64"}},
					"templates": []any{
						map[string]any{"name": "secret.yaml", "data": "raw template with {{ .Values.password }}"},
					},
				},
				map[string]any{
					"metadata": map[string]any{"name": "postgres"},
					"values":   map[string]any{"host": "db.internal", "credentials": "pg-creds"},
				},
			},
		},
	}

	redactReleaseForViewer(rel)

	chart := rel["chart"].(map[string]any)
	deps := chart["dependencies"].([]any)

	redis := deps[0].(map[string]any)
	if v := redis["values"].(map[string]any)["authPassword"]; v != redactPlaceholder {
		t.Errorf("subchart values not redacted, got %v", v)
	}
	if _, present := redis["files"]; present {
		t.Errorf("subchart chart.files should be dropped")
	}
	if _, present := redis["templates"]; present {
		t.Errorf("subchart chart.templates should be dropped (raw template source can embed secrets)")
	}
	// Metadata preserved
	if redis["metadata"].(map[string]any)["name"] != "redis" {
		t.Errorf("subchart metadata should be preserved")
	}

	postgres := deps[1].(map[string]any)
	pv := postgres["values"].(map[string]any)
	if pv["credentials"] != redactPlaceholder {
		t.Errorf("postgres subchart credentials not redacted, got %v", pv["credentials"])
	}
	if pv["host"] != "db.internal" {
		t.Errorf("postgres non-sensitive host should be preserved, got %v", pv["host"])
	}
}

func TestRedactReleaseForViewer_DropsTopLevelTemplates(t *testing.T) {
	rel := map[string]any{
		"chart": map[string]any{
			"templates": []any{
				map[string]any{"name": "secret.yaml", "data": "raw template with tokens"},
			},
		},
	}
	redactReleaseForViewer(rel)
	if _, present := rel["chart"].(map[string]any)["templates"]; present {
		t.Fatal("chart.templates must be dropped for non-admin views")
	}
}

// splitReleasePath is the pure path splitter helmDispatch uses to route.
func TestSplitReleasePath(t *testing.T) {
	cases := []struct {
		path string
		want []string
	}{
		{"/api/helm/releases/myns/myrelease", []string{"myns", "myrelease"}},
		{"/api/helm/releases/myns/myrelease/history", []string{"myns", "myrelease", "history"}},
		{"/api/helm/releases/myns/myrelease/rollback", []string{"myns", "myrelease", "rollback"}},
		{"/api/helm/releases/myns/myrelease/revisions/5", []string{"myns", "myrelease", "revisions", "5"}},
		{"/api/helm/releases/", []string{""}},
	}
	for _, c := range cases {
		got := splitReleasePath("/api/helm/releases/", c.path)
		if len(got) != len(c.want) {
			t.Errorf("splitReleasePath(%q): len=%d want=%d (%v)", c.path, len(got), len(c.want), got)
			continue
		}
		for i := range got {
			if got[i] != c.want[i] {
				t.Errorf("splitReleasePath(%q)[%d] = %q, want %q", c.path, i, got[i], c.want[i])
			}
		}
	}
}

func TestIsSensitiveLeafKey(t *testing.T) {
	cases := []struct {
		key  string
		want bool
	}{
		{"password", true},
		{"PASSWORD", true},
		{"dbPassword", true},
		{"db_password", true},
		{"APPSMITH_ENCRYPTION_PASSWORD", true},
		{"apiKey", true},
		{"api_key", true},
		{"token", true},
		{"bearerToken", true},
		{"privateKey", true},
		{"credentials", true},
		{"dsn", true},
		// Resource references — should NOT redact
		{"existingSecretName", false},
		{"imagePullSecretName", false},
		{"tokenName", false},
		{"secretRef", false},
		{"tlsSecretRef", false},
		// Mundane fields — should NOT redact
		{"replicas", false},
		{"image", false},
		{"tag", false},
		{"host", false},
		{"port", false},
		{"serviceType", false},
		{"namespace", false},
		// Empty
		{"", false},
	}
	for _, c := range cases {
		if got := isSensitiveLeafKey(c.key); got != c.want {
			t.Errorf("isSensitiveLeafKey(%q) = %v, want %v", c.key, got, c.want)
		}
	}
}

// redactManifestSecrets must mask .data and .stringData on Secret resources
// while passing every other resource through unchanged.
func TestRedactManifestSecrets_MasksOnlySecrets(t *testing.T) {
	manifest := `apiVersion: v1
kind: ConfigMap
metadata:
  name: app-config
data:
  greeting: hello

---

apiVersion: v1
kind: Secret
metadata:
  name: db-creds
type: Opaque
data:
  password: czNjcjN0
  username: YWxpY2U=
stringData:
  api-key: plaintext-value

---

apiVersion: apps/v1
kind: Deployment
metadata:
  name: api
spec:
  replicas: 3
`

	out := redactManifestSecrets(manifest)

	// ConfigMap untouched
	if !strings.Contains(out, "greeting: hello") {
		t.Error("ConfigMap value was incorrectly modified")
	}
	// Deployment untouched
	if !strings.Contains(out, "replicas: 3") {
		t.Error("Deployment was incorrectly modified")
	}
	// Secret .data values masked, keys preserved
	if strings.Contains(out, "czNjcjN0") || strings.Contains(out, "YWxpY2U=") {
		t.Error("Secret data value leaked into redacted manifest")
	}
	if !strings.Contains(out, "password:") {
		t.Error("Secret data KEY should remain visible (only value masked)")
	}
	// Secret .stringData value masked
	if strings.Contains(out, "plaintext-value") {
		t.Error("Secret stringData value leaked into redacted manifest")
	}
}

func TestRedactManifestSecrets_EmptyManifest(t *testing.T) {
	if got := redactManifestSecrets(""); got != "" {
		t.Errorf("empty manifest should pass through, got %q", got)
	}
}

// redactReleaseForViewer must blank config, chart.values, drop chart.files,
// and rewrite the manifest — leaving non-sensitive metadata (info, version)
// intact.
func TestRedactReleaseForViewer_BlanksSensitiveFields(t *testing.T) {
	rel := map[string]any{
		"name":      "myapp",
		"namespace": "prod",
		"version":   7,
		"info": map[string]any{
			"status":      "deployed",
			"description": "Install complete",
		},
		"config": map[string]any{
			"db": map[string]any{
				"password": "real-secret",
			},
		},
		"chart": map[string]any{
			"metadata": map[string]any{
				"name":    "myapp",
				"version": "1.2.3",
			},
			"values": map[string]any{
				"defaultPassword": "in-chart-defaults",
			},
			"files": []any{
				map[string]any{"name": "secrets.yaml", "data": "base64creds"},
			},
		},
		"manifest": "apiVersion: v1\nkind: Secret\nmetadata:\n  name: foo\ndata:\n  k: dmFsdWU=\n",
	}

	redactReleaseForViewer(rel)

	// Metadata stays
	if rel["name"] != "myapp" || rel["namespace"] != "prod" || rel["version"] != 7 {
		t.Errorf("non-sensitive top-level metadata mutated: %+v", rel)
	}
	if info, ok := rel["info"].(map[string]any); !ok || info["status"] != "deployed" {
		t.Error("release info was unexpectedly modified")
	}

	// config.db.password masked
	config := rel["config"].(map[string]any)
	db := config["db"].(map[string]any)
	if db["password"] != redactPlaceholder {
		t.Fatalf("config.db.password not redacted: %v", db["password"])
	}

	// chart.values: `defaultPassword` matches "password" → must be redacted
	chart := rel["chart"].(map[string]any)
	values := chart["values"].(map[string]any)
	if values["defaultPassword"] != redactPlaceholder {
		t.Fatalf("chart.values.defaultPassword not redacted: %v", values["defaultPassword"])
	}

	// chart.files dropped entirely
	if _, present := chart["files"]; present {
		t.Error("chart.files should be removed from non-admin view")
	}

	// chart.metadata stays (public chart info)
	meta := chart["metadata"].(map[string]any)
	if meta["name"] != "myapp" || meta["version"] != "1.2.3" {
		t.Error("chart.metadata should not be redacted")
	}

	// manifest had Secret → must not contain the secret value
	man := rel["manifest"].(string)
	if strings.Contains(man, "dmFsdWU=") {
		t.Error("manifest Secret value leaked after redaction")
	}
}
