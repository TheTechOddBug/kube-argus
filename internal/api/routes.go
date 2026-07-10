package api

import (
	"net/http"

	"kube-argus/internal/audit"
	"kube-argus/internal/auth"
	"kube-argus/internal/httpx"
	"kube-argus/internal/jit"
	"kube-argus/internal/notify"
)

// StartCache builds the informer factory and starts the in-memory cluster
// cache. Must be called after Init.
func StartCache() { startCacheLoop() }

// StartSpotAdvisor kicks off the background fetch for AWS spot pricing data.
func StartSpotAdvisor() { startSpotAdvisorLoop() }

// InitLLM wires the optional LLM gateway for /api/ai/* endpoints.
func InitLLM() { initLLM() }

// InitPrometheus wires the optional Prometheus client for query-backed views.
func InitPrometheus() { initPrometheus() }

// InitCRDClients builds the dynamic + apiextensions clients used by /api/crds.
func InitCRDClients() { initCRDClients() }

// CacheReady reports whether the cache loop produced metrics-server data yet.
func CacheReady() (nodeMetrics, podMetrics bool) {
	cache.mu.RLock()
	defer cache.mu.RUnlock()
	return cache.nodeMetrics != nil, cache.podMetrics != nil
}

// MetricsClientPresent reports whether the metrics-server client was
// initialized at startup. main uses this for the "metrics_server disabled"
// log line on cache warm-up.
func MetricsClientPresent() bool { return metricsCl != nil }

// RegisterRoutes attaches every HTTP handler exposed by this package to mux.
// JIT routes come from the jit package; Slack / webhook and auth routes are
// registered separately by main.
func RegisterRoutes(mux *http.ServeMux) {
	mux.HandleFunc("/api/overview", apiOverview)
	mux.HandleFunc("/api/nodes", apiNodes)
	mux.HandleFunc("/api/nodes/", apiNodeAction)
	mux.HandleFunc("/api/workloads", apiWorkloads)
	mux.HandleFunc("/api/workloads/", apiWorkloadAction)
	mux.HandleFunc("/api/search", apiSearch)
	mux.HandleFunc("/api/pods", apiPods)
	mux.HandleFunc("/api/pod-sparklines", apiPodSparklines)
	mux.HandleFunc("/api/pods/", apiPodDetail)
	mux.HandleFunc("/api/ingresses", apiIngresses)
	mux.HandleFunc("/api/ingresses/", apiIngressDescribe)
	mux.HandleFunc("/api/services", apiServices)
	mux.HandleFunc("/api/services/", apiServiceDetail)
	mux.HandleFunc("/api/events", apiEvents)
	mux.HandleFunc("/api/hpa", apiHPA)
	mux.HandleFunc("/api/hpa/", apiHPADetail)
	mux.HandleFunc("/api/configs", apiConfigs)
	mux.HandleFunc("/api/configs/", apiConfigData)
	mux.HandleFunc("/api/exec", apiExec)
	mux.HandleFunc("/api/spot-advisor", apiSpotAdvisor)
	mux.HandleFunc("/api/spot-interruptions", apiSpotInterruptions)
	mux.HandleFunc("/api/topology-spread", apiTopologySpread)
	mux.HandleFunc("/api/metrics/node", apiMetricsNode)
	mux.HandleFunc("/api/metrics/pod", apiMetricsPod)
	mux.HandleFunc("/api/metrics/workload", apiMetricsWorkload)
	mux.HandleFunc("/api/restart-timeline", apiRestartTimeline)
	mux.HandleFunc("/api/pdbs", apiPDBs)
	mux.HandleFunc("/api/cronjobs/", apiCronJobHistory)
	mux.HandleFunc("/api/namespace-costs", apiNamespaceCosts)
	mux.HandleFunc("/api/workload-sizing", apiWorkloadSizing)
	mux.HandleFunc("/api/alerts", apiAlerts)
	mux.HandleFunc("/api/ai/diagnose", apiAIDiagnose)
	mux.HandleFunc("/api/ai/spot-analysis", apiAISpotAnalysis)
	mux.HandleFunc("/api/namespaces", apiNamespaces)
	mux.HandleFunc("/api/cluster-info", func(w http.ResponseWriter, r *http.Request) {
		httpx.JSON(w, map[string]string{"name": clusterName})
	})
	mux.HandleFunc("/api/storage", apiStorage)
	mux.HandleFunc("/api/config-drift", apiConfigDrift)
	mux.HandleFunc("/api/yaml/", apiYaml)

	mux.HandleFunc("/api/jit/requests", jit.RequestsHandler)
	mux.HandleFunc("/api/jit/my-grants", jit.MyGrantsHandler)
	mux.HandleFunc("/api/jit/", jit.ActionHandler)

	mux.HandleFunc("/api/slack/interact", notify.SlackInteractHandler)
	mux.HandleFunc("/api/settings/slack", notify.SlackSettingsHandler)
	mux.HandleFunc("/api/settings/webhook", notify.WebhookSettingsHandler)
	mux.HandleFunc("/api/audit", audit.Handler)
	mux.HandleFunc("/api/crds", apiCRDs)
	mux.HandleFunc("/api/crd/", apiCRDResource)
	mux.HandleFunc("/api/helm/releases", apiHelmReleases)
	mux.HandleFunc("/api/helm/releases/", helmDispatch)
	mux.HandleFunc("/api/ws/presence", audit.PresenceHandler)
	mux.HandleFunc("/api/online-users", func(w http.ResponseWriter, r *http.Request) {
		if !auth.RequireAdmin(w, r) {
			return
		}
		httpx.JSON(w, audit.GetOnlineUsers())
	})
}
