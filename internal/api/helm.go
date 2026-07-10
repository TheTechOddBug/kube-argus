package api

import (
	"bytes"
	"compress/gzip"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	"helm.sh/helm/v3/pkg/action"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/discovery"
	"k8s.io/client-go/discovery/cached/memory"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/restmapper"
	"k8s.io/client-go/tools/clientcmd"
	clientcmdapi "k8s.io/client-go/tools/clientcmd/api"
	sigsyaml "sigs.k8s.io/yaml"

	"kube-argus/internal/audit"
	"kube-argus/internal/auth"
	"kube-argus/internal/httpx"
)

// ─── Helm Releases ───────────────────────────────────────────────────
//
// Helm 3 stores each release revision as a Kubernetes Secret. The Secret has
// type "helm.sh/release.v1", standard labels (name, owner=helm, status,
// version), and the actual release payload in data["release"] as
// base64(gzip(JSON)) — Helm double-encodes so the gzipped bytes survive YAML
// transport intact.

type helmRelease struct {
	Name       string `json:"name"`
	Namespace  string `json:"namespace"`
	Status     string `json:"status"`
	Revision   int    `json:"revision"`
	UpdatedAgo string `json:"updatedAgo"`
	UpdatedAt  string `json:"updatedAt"`
	Chart      string `json:"chart,omitempty"`
}

// GET /api/helm/releases?namespace=X — latest revision per release.
func apiHelmReleases(w http.ResponseWriter, r *http.Request) {
	if clientset == nil {
		httpx.Error(w, "clientset not initialized", 500)
		return
	}
	nsFilter := r.URL.Query().Get("namespace")

	secrets, err := clientset.CoreV1().Secrets(nsFilter).List(r.Context(), metav1.ListOptions{
		LabelSelector: "owner=helm",
	})
	if err != nil {
		httpx.K8sError(w, err)
		return
	}

	// Group by (namespace, name); keep the highest revision.
	type key struct{ ns, name string }
	latest := map[key]helmRelease{}
	for _, s := range secrets.Items {
		name := s.Labels["name"]
		if name == "" {
			continue
		}
		ver, _ := strconv.Atoi(s.Labels["version"])
		k := key{s.Namespace, name}
		if existing, ok := latest[k]; ok && ver <= existing.Revision {
			continue
		}
		latest[k] = helmRelease{
			Name:       name,
			Namespace:  s.Namespace,
			Status:     s.Labels["status"],
			Revision:   ver,
			UpdatedAgo: shortDur(time.Since(s.CreationTimestamp.Time)),
			UpdatedAt:  s.CreationTimestamp.Time.Format(time.RFC3339),
		}
	}

	out := make([]helmRelease, 0, len(latest))
	for _, rel := range latest {
		out = append(out, rel)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Namespace != out[j].Namespace {
			return out[i].Namespace < out[j].Namespace
		}
		return out[i].Name < out[j].Name
	})
	httpx.JSON(w, out)
}

// GET /api/helm/releases/{ns}/{name}
// Returns the decoded release JSON (latest revision) — chart info, manifest,
// values, status. Reads the secret, base64-decodes, gunzips, parses JSON.
func apiHelmRelease(w http.ResponseWriter, r *http.Request) {
	if clientset == nil {
		httpx.Error(w, "clientset not initialized", 500)
		return
	}
	parts := strings.Split(strings.TrimPrefix(r.URL.Path, "/api/helm/releases/"), "/")
	if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
		httpx.Error(w, "use /api/helm/releases/{ns}/{name}", 400)
		return
	}
	ns, name := parts[0], parts[1]

	secrets, err := clientset.CoreV1().Secrets(ns).List(r.Context(), metav1.ListOptions{
		LabelSelector: fmt.Sprintf("owner=helm,name=%s", name),
	})
	if err != nil {
		httpx.K8sError(w, err)
		return
	}
	if len(secrets.Items) == 0 {
		httpx.Error(w, "release not found", 404)
		return
	}

	// Pick highest revision
	maxVer := -1
	latestIdx := -1
	for i := range secrets.Items {
		ver, _ := strconv.Atoi(secrets.Items[i].Labels["version"])
		if ver > maxVer {
			maxVer = ver
			latestIdx = i
		}
	}
	if latestIdx < 0 {
		httpx.Error(w, "release not found", 404)
		return
	}
	secret := secrets.Items[latestIdx]

	encoded := secret.Data["release"]
	if len(encoded) == 0 {
		httpx.Error(w, "release data missing on secret", 500)
		return
	}

	// secret.Data is already base64-decoded by the client-go transport, but
	// Helm double-encoded — so the bytes we got are the ASCII of another
	// base64 string. Decode again to get the gzipped JSON.
	decoded, err := base64.StdEncoding.DecodeString(string(encoded))
	if err != nil {
		httpx.Error(w, "base64 decode failed: "+err.Error(), 500)
		return
	}
	gz, err := gzip.NewReader(bytes.NewReader(decoded))
	if err != nil {
		httpx.Error(w, "gzip decode failed: "+err.Error(), 500)
		return
	}
	defer gz.Close()
	raw, err := io.ReadAll(gz)
	if err != nil {
		httpx.Error(w, "gzip read failed: "+err.Error(), 500)
		return
	}

	var release any
	if err := json.Unmarshal(raw, &release); err != nil {
		httpx.Error(w, "release json parse failed: "+err.Error(), 500)
		return
	}
	maybeRedact(r, release, ns, name, "latest")
	httpx.JSON(w, release)
}

// ─── Redaction for non-admin views ──────────────────────────────────
//
// Helm releases routinely contain secrets in three places:
//   1. config       — user-supplied values (passwords, tokens, DSNs)
//   2. chart.values — chart defaults (sometimes contain templated secrets)
//   3. manifest     — the rendered K8s YAML, which embeds Secret objects
//                     with base64-encoded data
//
// For non-admin requests we replace every leaf value in (1) and (2) with a
// placeholder, parse (3) and mask Secret.data/.stringData, and drop chart
// files (which sometimes bundle credentials).

const redactPlaceholder = "***REDACTED-NON-ADMIN***"

// sensitiveSubstrings — if a leaf key contains any of these (case-insensitive),
// its string value is considered sensitive and will be redacted. Numbers,
// booleans, and nulls are never redacted (a viewer learning that replicas=3
// or serviceType=ClusterIP isn't a leak).
var sensitiveSubstrings = []string{
	"password", "passwd",
	"secret",     // also matches secretkey, encryptionsecret, etc.
	"token",      // jwt, oauth, bearer, etc.
	"apikey", "api_key",
	"accesskey", "access_key",
	"privatekey", "private_key",
	"signingkey", "signing_key",
	"encryptionkey", "encryption_key",
	"credential", // credential, credentials
	"bearer",
	"dsn", // database DSN strings frequently contain passwords
}

// refSuffixes — keys ending in these are treated as references to a resource
// (not the secret itself), so we DON'T redact even when the rest of the key
// contains "secret"/"token"/etc. Examples preserved: imagePullSecretName,
// existingSecretRef, tlsSecretRef, tokenName.
var refSuffixes = []string{
	"name", "names",
	"ref", "refs", "reference", "references",
}

func isSensitiveLeafKey(key string) bool {
	if key == "" {
		return false
	}
	lower := strings.ToLower(key)
	for _, suf := range refSuffixes {
		if strings.HasSuffix(lower, suf) {
			return false
		}
	}
	for _, tok := range sensitiveSubstrings {
		if strings.Contains(lower, tok) {
			return true
		}
	}
	return false
}

// valueHolderKeys — when we're inside a sensitive-key branch (e.g. under a
// key called "password"), and we encounter a leaf like {"value": "..."} or
// {"raw": "..."}, we treat the inner string as the secret's value even
// though the immediate key isn't itself sensitive. This handles the common
// pattern `password: { value: "..." }` without over-redacting siblings.
var valueHolderKeys = map[string]bool{
	"value":       true,
	"raw":         true,
	"rawvalue":    true,
	"data":        true,
	"content":     true,
	"string":      true,
	"stringvalue": true,
	"plaintext":   true,
}

// redactValues walks the structure and replaces only string leaves whose
// containing key looks sensitive (or which sit under a sensitive-key
// ancestor with a value-holder wrapper key). Numbers, booleans, and
// references (keys ending in "Name", "Ref", etc.) pass through unchanged.
func redactValues(v any) any {
	return redactValuesAt(v, "", false)
}

func redactValuesAt(v any, parentKey string, sensitiveAncestor bool) any {
	switch t := v.(type) {
	case map[string]any:
		out := make(map[string]any, len(t))
		for k, val := range t {
			// A key marks the sub-tree as sensitive if it's directly sensitive
			// OR we're already under a sensitive ancestor. We propagate the
			// flag so nested value-holders under `password: {...}` still hit.
			childAncestor := sensitiveAncestor || isSensitiveLeafKey(k)
			out[k] = redactValuesAt(val, k, childAncestor)
		}
		return out
	case []any:
		out := make([]any, len(t))
		for i, val := range t {
			out[i] = redactValuesAt(val, parentKey, sensitiveAncestor)
		}
		return out
	case string:
		if isSensitiveLeafKey(parentKey) {
			return redactPlaceholder
		}
		// Nested value-holder under a sensitive ancestor:
		//   password:
		//     value: "actual-secret"        ← this triggers
		//     description: "prod password"  ← this preserves (not value-holder)
		if sensitiveAncestor && valueHolderKeys[strings.ToLower(parentKey)] {
			return redactPlaceholder
		}
		return t
	default:
		// numbers, bools, nil — preserved
		return v
	}
}

// redactManifestSecrets parses the multi-doc YAML manifest and masks .data
// and .stringData on any Secret resource. Non-Secret docs pass through
// unchanged. Unparseable docs pass through unchanged (Helm sometimes emits
// extra whitespace blocks that aren't YAML).
func redactManifestSecrets(manifest string) string {
	if strings.TrimSpace(manifest) == "" {
		return manifest
	}
	docs := strings.Split(manifest, "\n---\n")
	out := make([]string, 0, len(docs))
	for _, doc := range docs {
		out = append(out, redactOneDocIfSecret(doc))
	}
	return strings.Join(out, "\n---\n")
}

func redactOneDocIfSecret(doc string) string {
	var m map[string]any
	if err := sigsyaml.Unmarshal([]byte(doc), &m); err != nil || m == nil {
		return doc
	}
	kind, _ := m["kind"].(string)
	if kind != "Secret" {
		return doc
	}
	if d, ok := m["data"].(map[string]any); ok {
		for k := range d {
			d[k] = "REDACTED-NON-ADMIN"
		}
	}
	if sd, ok := m["stringData"].(map[string]any); ok {
		for k := range sd {
			sd[k] = "REDACTED-NON-ADMIN"
		}
	}
	if buf, err := sigsyaml.Marshal(m); err == nil {
		return string(buf)
	}
	return doc
}

// redactReleaseForViewer mutates the release in place: blanks config,
// chart.values, chart.dependencies[i].values; drops chart.files and
// chart.templates; rewrites manifest. Caller must check role first.
func redactReleaseForViewer(release map[string]any) {
	if v, ok := release["config"]; ok {
		release["config"] = redactValues(v)
	}
	if chart, ok := release["chart"].(map[string]any); ok {
		redactChartForViewer(chart)
	}
	if m, ok := release["manifest"].(string); ok {
		release["manifest"] = redactManifestSecrets(m)
	}
}

// redactChartForViewer handles a single chart node: redacts values, drops
// files and templates, and recurses into dependencies (subcharts share the
// same layout and can also embed secrets in their defaults).
func redactChartForViewer(chart map[string]any) {
	if v, ok := chart["values"]; ok {
		chart["values"] = redactValues(v)
	}
	// chart.files may bundle credentials; chart.templates is raw source that
	// can contain string literals — safest to drop both entirely.
	delete(chart, "files")
	delete(chart, "templates")

	// Recurse into subchart dependencies. Each dep is a full chart node.
	if deps, ok := chart["dependencies"].([]any); ok {
		for i := range deps {
			if dep, ok := deps[i].(map[string]any); ok {
				redactChartForViewer(dep)
			}
		}
	}
}

// maybeRedact applies the redaction if the caller is not admin and records
// the read in the audit trail. The release is mutated in place. No-op for
// admins (full visibility, no audit noise).
func maybeRedact(r *http.Request, release any, ns, name string, kind string) {
	if auth.IsAdmin(r) {
		return
	}
	m, ok := release.(map[string]any)
	if !ok {
		return
	}
	redactReleaseForViewer(m)
	if sd, ok := r.Context().Value(auth.UserCtxKey).(*auth.SessionData); ok && sd != nil {
		audit.Record(sd.Email, sd.Role, "helm.release.view",
			fmt.Sprintf("Release %s/%s", ns, name),
			fmt.Sprintf("%s redacted (config, chart.values, Secret manifests masked)", kind),
			auth.ClientIP(r))
	}
}

// ─── Helm action SDK glue ───────────────────────────────────────────

// helmRESTGetter implements genericclioptions.RESTClientGetter from our
// existing *rest.Config so the Helm SDK can run without reading kubeconfig.
type helmRESTGetter struct {
	cfg       *rest.Config
	namespace string
}

func (h *helmRESTGetter) ToRESTConfig() (*rest.Config, error) {
	return rest.CopyConfig(h.cfg), nil
}
func (h *helmRESTGetter) ToDiscoveryClient() (discovery.CachedDiscoveryInterface, error) {
	cfg, _ := h.ToRESTConfig()
	cfg.Burst = 100
	dc, err := discovery.NewDiscoveryClientForConfig(cfg)
	if err != nil {
		return nil, err
	}
	return memory.NewMemCacheClient(dc), nil
}
func (h *helmRESTGetter) ToRESTMapper() (meta.RESTMapper, error) {
	dc, err := h.ToDiscoveryClient()
	if err != nil {
		return nil, err
	}
	return restmapper.NewDeferredDiscoveryRESTMapper(dc), nil
}
func (h *helmRESTGetter) ToRawKubeConfigLoader() clientcmd.ClientConfig {
	return clientcmd.NewDefaultClientConfig(
		clientcmdapi.Config{},
		&clientcmd.ConfigOverrides{Context: clientcmdapi.Context{Namespace: h.namespace}},
	)
}

func helmActionConfig(namespace string) (*action.Configuration, error) {
	if restCfg == nil {
		return nil, fmt.Errorf("rest config not initialized")
	}
	getter := &helmRESTGetter{cfg: restCfg, namespace: namespace}
	cfg := new(action.Configuration)
	logf := func(format string, v ...interface{}) {
		slog.Debug("helm sdk", "msg", fmt.Sprintf(format, v...))
	}
	if err := cfg.Init(getter, namespace, "secrets", logf); err != nil {
		return nil, err
	}
	return cfg, nil
}

// ─── New endpoints: history, revision, rollback, uninstall ──────────

func splitReleasePath(prefix, path string) []string {
	return strings.Split(strings.TrimPrefix(path, prefix), "/")
}

// helmDispatch routes `/api/helm/releases/...` to the right handler based on
// path shape and method. Replaces the prior single-handler registration.
func helmDispatch(w http.ResponseWriter, r *http.Request) {
	parts := splitReleasePath("/api/helm/releases/", r.URL.Path)
	switch {
	case len(parts) == 2 && r.Method == "GET":
		apiHelmRelease(w, r) // latest revision detail
	case len(parts) == 2 && r.Method == "DELETE":
		apiHelmUninstall(w, r)
	case len(parts) == 3 && parts[2] == "history" && r.Method == "GET":
		apiHelmHistory(w, r)
	case len(parts) == 3 && parts[2] == "rollback" && r.Method == "POST":
		apiHelmRollback(w, r)
	case len(parts) == 4 && parts[2] == "revisions" && r.Method == "GET":
		apiHelmRevision(w, r)
	default:
		httpx.Error(w, "unknown helm route", 404)
	}
}

// GET /api/helm/releases/{ns}/{name}/history
// Returns all revisions (up to 64) with status, timestamps, chart, description.
func apiHelmHistory(w http.ResponseWriter, r *http.Request) {
	parts := splitReleasePath("/api/helm/releases/", r.URL.Path)
	ns, name := parts[0], parts[1]
	cfg, err := helmActionConfig(ns)
	if err != nil {
		httpx.Error(w, "helm init: "+err.Error(), 500)
		return
	}
	hist := action.NewHistory(cfg)
	hist.Max = 64
	releases, err := hist.Run(name)
	if err != nil {
		httpx.K8sError(w, err)
		return
	}
	type entry struct {
		Revision    int    `json:"revision"`
		Status      string `json:"status"`
		UpdatedAt   string `json:"updatedAt"`
		UpdatedAgo  string `json:"updatedAgo"`
		Description string `json:"description"`
		Chart       string `json:"chart,omitempty"`
		AppVersion  string `json:"appVersion,omitempty"`
	}
	out := make([]entry, 0, len(releases))
	for _, rel := range releases {
		chart, appVer := "", ""
		if rel.Chart != nil && rel.Chart.Metadata != nil {
			chart = rel.Chart.Metadata.Name + "-" + rel.Chart.Metadata.Version
			appVer = rel.Chart.Metadata.AppVersion
		}
		updated := rel.Info.LastDeployed.Time
		out = append(out, entry{
			Revision:    rel.Version,
			Status:      rel.Info.Status.String(),
			UpdatedAt:   updated.Format(time.RFC3339),
			UpdatedAgo:  shortDur(time.Since(updated)),
			Description: rel.Info.Description,
			Chart:       chart,
			AppVersion:  appVer,
		})
	}
	// Newest revision first.
	sort.Slice(out, func(i, j int) bool { return out[i].Revision > out[j].Revision })
	httpx.JSON(w, out)
}

// GET /api/helm/releases/{ns}/{name}/revisions/{rev}
// Returns the full decoded release for a specific revision (chart, manifest,
// values, status). Used by the UI to support value-diffs between two revisions.
func apiHelmRevision(w http.ResponseWriter, r *http.Request) {
	parts := splitReleasePath("/api/helm/releases/", r.URL.Path)
	ns, name, revStr := parts[0], parts[1], parts[3]
	rev, err := strconv.Atoi(revStr)
	if err != nil || rev <= 0 {
		httpx.Error(w, "revision must be a positive integer", 400)
		return
	}
	secrets, err := clientset.CoreV1().Secrets(ns).List(r.Context(), metav1.ListOptions{
		LabelSelector: fmt.Sprintf("owner=helm,name=%s,version=%d", name, rev),
	})
	if err != nil {
		httpx.K8sError(w, err)
		return
	}
	if len(secrets.Items) == 0 {
		httpx.Error(w, "revision not found", 404)
		return
	}
	encoded := secrets.Items[0].Data["release"]
	if len(encoded) == 0 {
		httpx.Error(w, "release data missing", 500)
		return
	}
	decoded, err := base64.StdEncoding.DecodeString(string(encoded))
	if err != nil {
		httpx.Error(w, "base64 decode failed: "+err.Error(), 500)
		return
	}
	gz, err := gzip.NewReader(bytes.NewReader(decoded))
	if err != nil {
		httpx.Error(w, "gzip decode failed: "+err.Error(), 500)
		return
	}
	defer gz.Close()
	raw, err := io.ReadAll(gz)
	if err != nil {
		httpx.Error(w, "gzip read failed: "+err.Error(), 500)
		return
	}
	var release any
	if err := json.Unmarshal(raw, &release); err != nil {
		httpx.Error(w, "release json parse failed: "+err.Error(), 500)
		return
	}
	maybeRedact(r, release, ns, name, fmt.Sprintf("rev %d", rev))
	httpx.JSON(w, release)
}

// POST /api/helm/releases/{ns}/{name}/rollback?revision=N — admin only.
func apiHelmRollback(w http.ResponseWriter, r *http.Request) {
	if !auth.RequireAdmin(w, r) {
		return
	}
	parts := splitReleasePath("/api/helm/releases/", r.URL.Path)
	ns, name := parts[0], parts[1]
	revStr := r.URL.Query().Get("revision")
	rev, err := strconv.Atoi(revStr)
	if err != nil || rev <= 0 {
		httpx.Error(w, "?revision=N required (positive int)", 400)
		return
	}
	cfg, err := helmActionConfig(ns)
	if err != nil {
		httpx.Error(w, "helm init: "+err.Error(), 500)
		return
	}
	rb := action.NewRollback(cfg)
	rb.Version = rev
	rb.Wait = false
	rb.Timeout = 60 * time.Second
	if err := rb.Run(name); err != nil {
		httpx.Error(w, "rollback failed: "+err.Error(), 500)
		return
	}
	if sd, ok := r.Context().Value(auth.UserCtxKey).(*auth.SessionData); ok && sd != nil {
		audit.Record(sd.Email, sd.Role, "helm.rollback", fmt.Sprintf("Release %s/%s", ns, name), fmt.Sprintf("to revision %d", rev), auth.ClientIP(r))
	}
	httpx.JSON(w, map[string]any{"status": "ok", "revision": rev})
}

// DELETE /api/helm/releases/{ns}/{name}[?keepHistory=true] — admin only.
func apiHelmUninstall(w http.ResponseWriter, r *http.Request) {
	if !auth.RequireAdmin(w, r) {
		return
	}
	parts := splitReleasePath("/api/helm/releases/", r.URL.Path)
	ns, name := parts[0], parts[1]
	keepHistory := r.URL.Query().Get("keepHistory") == "true"
	cfg, err := helmActionConfig(ns)
	if err != nil {
		httpx.Error(w, "helm init: "+err.Error(), 500)
		return
	}
	un := action.NewUninstall(cfg)
	un.KeepHistory = keepHistory
	un.Wait = false
	un.Timeout = 60 * time.Second
	if _, err := un.Run(name); err != nil {
		httpx.Error(w, "uninstall failed: "+err.Error(), 500)
		return
	}
	if sd, ok := r.Context().Value(auth.UserCtxKey).(*auth.SessionData); ok && sd != nil {
		detail := ""
		if keepHistory {
			detail = "kept history"
		}
		audit.Record(sd.Email, sd.Role, "helm.uninstall", fmt.Sprintf("Release %s/%s", ns, name), detail, auth.ClientIP(r))
	}
	httpx.JSON(w, map[string]string{"status": "ok"})
}
