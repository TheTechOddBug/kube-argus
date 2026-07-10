package main

import (
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"runtime/debug"
	"strings"

	_ "golang.org/x/crypto/ssh"
	"golang.org/x/term"

	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
	metricsv "k8s.io/metrics/pkg/client/clientset/versioned"

	"kube-argus/internal/api"
	"kube-argus/internal/audit"
	"kube-argus/internal/auth"
	"kube-argus/internal/jit"
	"kube-argus/internal/notify"
)

var clusterName string

// parseLogLevel maps a case-insensitive level string to a slog.Level.
// Returns (level, true) for recognized values; (slog.LevelInfo, false) otherwise.
func parseLogLevel(s string) (slog.Level, bool) {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "debug":
		return slog.LevelDebug, true
	case "info":
		return slog.LevelInfo, true
	case "warn":
		return slog.LevelWarn, true
	case "error":
		return slog.LevelError, true
	default:
		return slog.LevelInfo, false
	}
}

// initLogger configures the global slog default logger with a JSON handler
// writing to stdout. The minimum level is read from the LOG_LEVEL env var.
func initLogger() {
	levelStr := os.Getenv("LOG_LEVEL")
	level, recognized := parseLogLevel(levelStr)

	handler := slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{
		Level: level,
	})
	slog.SetDefault(slog.New(handler))

	if levelStr != "" && !recognized {
		slog.Warn("unrecognized LOG_LEVEL value, defaulting to info", "LOG_LEVEL", levelStr)
	}
}

func main() {
	initLogger()

	isTTY := term.IsTerminal(int(os.Stdout.Fd()))
	cyan, bold, uline, reset := "\033[36m", "\033[1m", "\033[4m", "\033[0m"
	if !isTTY {
		cyan, bold, uline, reset = "", "", "", ""
	}
	fmt.Fprint(os.Stdout, cyan+`
  _  ___   _ ____  _____      _    ____   ____ _   _ ____
 | |/ / | | | __ )| ____|    / \  |  _ \ / ___| | | / ___|
 | ' /| | | |  _ \|  _|     / _ \ | |_) | |  _| | | \___ \
 | . \| |_| | |_) | |___   / ___ \|  _ <| |_| | |_| |___) |
 |_|\_\\___/|____/|_____| /_/   \_\_| \_\\____|\___/|____/
`+reset+"\n")
	fmt.Fprint(os.Stdout, "  "+bold+"Real-time Kubernetes Dashboard"+reset+"\n")
	fmt.Fprint(os.Stdout, "  Created by "+cyan+"Manish Chaudhary"+reset+" ("+uline+"https://github.com/manishchaudhary101"+reset+")\n")
	fmt.Fprintln(os.Stdout)

	auth.LoadSecretsFromAWS()

	cfg, err := kubeConfig()
	if err != nil {
		slog.Error("kubeconfig failed", "error", err)
		os.Exit(1)
	}
	clientset, err := kubernetes.NewForConfig(cfg)
	if err != nil {
		slog.Error("clientset init failed", "error", err)
		os.Exit(1)
	}
	metricsCl, err := metricsv.NewForConfig(cfg)
	if err != nil {
		slog.Warn("metrics-server client init failed", "error", err)
		metricsCl = nil
	} else {
		slog.Info("metrics-server client initialized")
	}

	api.Init(clientset, metricsCl, cfg, clusterName)

	slog.Info("warming cache...")
	prevGC := debug.SetGCPercent(400)
	api.StartCache()
	debug.SetGCPercent(prevGC)
	runtime.GC()
	nm, pm := api.CacheReady()
	if nm && pm {
		slog.Info("cache ready", "metrics_server", "node + pod metrics available")
	} else if nm || pm {
		slog.Info("cache ready", "metrics_server", "partial", "node", nm, "pod", pm)
	} else if api.MetricsClientPresent() {
		slog.Warn("cache ready", "metrics_server", "no data returned — check APIService and RBAC")
	} else {
		slog.Info("cache ready", "metrics_server", "disabled")
	}

	api.StartSpotAdvisor()
	api.InitLLM()
	api.InitPrometheus()

	auth.Init()

	jit.SetClientset(clientset)
	jit.ResolvePodOwner = api.ResolvePodOwner
	jit.InitPersistence()
	jit.Restore()
	if n := jit.HasRequests(); n > 0 {
		slog.Info("jit: restored requests", "count", n)
	}
	jit.StartExpiryLoop()

	// Wire cross-package callbacks before any package that fires events runs.
	auth.AuditRecord = audit.Record
	auth.TrackUser = audit.TrackUser
	auth.HasActiveJIT = jit.HasActive

	notify.Init(clientset, jit.Namespace())
	notify.Cluster = func() string { return clusterName }
	notify.ClientIP = auth.ClientIP
	notify.RequireAdmin = auth.RequireAdmin
	notify.AuditRecord = audit.Record
	notify.CurrentUser = func(r *http.Request) (string, string, bool) {
		sd, ok := r.Context().Value(auth.UserCtxKey).(*auth.SessionData)
		if !ok || sd == nil {
			return "", "", false
		}
		return sd.Email, sd.Role, true
	}
	notify.JITApprove = jit.Approve
	notify.JITDeny = jit.Deny
	notify.JITLookup = jit.Lookup
	notify.InitSlack()
	notify.InitWebhook()
	api.InitCRDClients()

	audit.InitPersistence(clientset, jit.Namespace())
	audit.Restore()

	mux := http.NewServeMux()

	mux.HandleFunc("/auth/login", auth.LoginHandler)
	mux.HandleFunc("/auth/callback", auth.CallbackHandler)
	mux.HandleFunc("/auth/logout", auth.LogoutHandler)
	mux.HandleFunc("/api/me", auth.MeHandler)
	api.RegisterRoutes(mux)
	mux.HandleFunc("/health", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(200)
		w.Write([]byte(`{"status":"ok"}`))
	})

	webRoot := "web/dist"
	if _, err := os.Stat(webRoot); err == nil {
		fs := http.FileServer(http.Dir(webRoot))
		mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
			if strings.HasPrefix(r.URL.Path, "/api") || strings.HasPrefix(r.URL.Path, "/auth") {
				http.NotFound(w, r)
				return
			}
			p := filepath.Join(webRoot, filepath.Clean(r.URL.Path))
			if fi, e := os.Stat(p); e == nil && !fi.IsDir() {
				if strings.HasPrefix(r.URL.Path, "/assets/") {
					w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
				}
				fs.ServeHTTP(w, r)
				return
			}
			if strings.HasPrefix(r.URL.Path, "/assets/") {
				http.NotFound(w, r)
				return
			}
			w.Header().Set("Cache-Control", "no-cache")
			http.ServeFile(w, r, filepath.Join(webRoot, "index.html"))
		})
	}

	addr := ":8080"
	if p := os.Getenv("PORT"); p != "" {
		addr = ":" + p
	}
	slog.Info("kube-argus listening", "addr", addr)
	if err := http.ListenAndServe(addr, api.GzipWrap(auth.Middleware(api.CORSWrap(mux)))); err != nil {
		slog.Error("server exited", "error", err)
		os.Exit(1)
	}
}

func kubeConfig() (*rest.Config, error) {
	if name := os.Getenv("CLUSTER_NAME"); name != "" {
		clusterName = name
	}
	if os.Getenv("KUBERNETES_SERVICE_HOST") != "" {
		if clusterName == "" {
			clusterName = "in-cluster"
		}
		return rest.InClusterConfig()
	}
	kc := os.Getenv("KUBECONFIG")
	if kc == "" {
		kc = filepath.Join(os.Getenv("HOME"), ".kube", "config")
	}
	if clusterName == "" {
		if raw, err := clientcmd.NewDefaultClientConfigLoadingRules().Load(); err == nil {
			ctx := raw.CurrentContext
			if i := strings.LastIndex(ctx, "/"); i >= 0 {
				ctx = ctx[i+1:]
			}
			clusterName = ctx
		}
	}
	if clusterName == "" {
		clusterName = "unknown"
	}
	return clientcmd.BuildConfigFromFlags("", kc)
}
