package api

import (
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	metricsv "k8s.io/metrics/pkg/client/clientset/versioned"
)

// Shared cluster handles. main.go wires these once at startup; every handler
// in this package reads them as package-level globals to keep the diff small.
// (dynamicClient + apiextClient are declared in crds.go.)
var (
	clientset   *kubernetes.Clientset
	metricsCl   *metricsv.Clientset
	restCfg     *rest.Config
	clusterName string
)

// Init wires the kube clients and cluster name. Must be called before any
// handler runs.
func Init(cs *kubernetes.Clientset, mc *metricsv.Clientset, cfg *rest.Config, name string) {
	clientset = cs
	metricsCl = mc
	restCfg = cfg
	clusterName = name
}

// ClusterName returns the discovered cluster name (used by callers outside the
// package, e.g. main's /api/cluster-info handler).
func ClusterName() string { return clusterName }

// Clientset exposes the kube clientset to other packages.
func Clientset() kubernetes.Interface { return clientset }

// RestConfig exposes the rest config (used by exec).
func RestConfig() *rest.Config { return restCfg }
