package api

import (
	"context"
	"log/slog"
	"sort"
	"strings"
	"sync"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	autov2 "k8s.io/api/autoscaling/v2"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	netv1 "k8s.io/api/networking/v1"
	policyv1 "k8s.io/api/policy/v1"
	storagev1 "k8s.io/api/storage/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	metricsapi "k8s.io/metrics/pkg/apis/metrics/v1beta1"

	"k8s.io/client-go/informers"
	kcache "k8s.io/client-go/tools/cache"
)

// ─── Background Cache ────────────────────────────────────────────────
//
// Uses Kubernetes SharedInformerFactory (List+Watch) instead of periodic
// full re-lists.  The informer maintains an in-memory store for each
// resource type via a persistent watch stream.  rebuildFromInformers()
// copies the current state from those stores into the typed list fields
// that API handlers already read — zero API server calls.
//
// Metrics-server doesn't support watch, so node/pod metrics are polled
// separately on a 10-second timer.

// rsOwner holds the resolved owner of a ReplicaSet (typically a Deployment).
type rsOwner struct {
	Kind string
	Name string
}

type clusterCache struct {
	mu sync.RWMutex
	// Hot, high-cardinality types stored as pointer slices to avoid the
	// per-rebuild value-copy cost (millions of struct copies on big clusters).
	pods        []*corev1.Pod
	events      []*corev1.Event
	replicasets []*appsv1.ReplicaSet

	nodes          *corev1.NodeList
	deployments    *appsv1.DeploymentList
	statefulsets   *appsv1.StatefulSetList
	daemonsets     *appsv1.DaemonSetList
	services       *corev1.ServiceList
	jobs           *batchv1.JobList
	cronjobs       *batchv1.CronJobList
	namespaces     *corev1.NamespaceList
	ingresses      *netv1.IngressList
	hpas           *autov2.HorizontalPodAutoscalerList
	configMeta     []configMeta
	secretMeta     []configMeta
	nodeMetrics    *metricsapi.NodeMetricsList
	podMetrics     *metricsapi.PodMetricsList
	pdbs           *policyv1.PodDisruptionBudgetList
	pvcs           *corev1.PersistentVolumeClaimList
	pvs            *corev1.PersistentVolumeList
	storageClasses *storagev1.StorageClassList
	configDrift    []interface{}
	lastRefresh    time.Time

	// Pre-computed lookup maps (rebuilt each cycle).
	podMetricsMap map[string][2]int64 // "ns/name" → [cpuMillis, memMiB]
	rsOwners      map[string]rsOwner  // "ns/rsName" → resolved owner
}

type configMeta struct {
	Name         string
	Namespace    string
	Keys         []string
	Type         string
	CreatedAt    time.Time
	LastModified time.Time
	Version      string
}

var (
	cache             = &clusterCache{}
	informerFactory   informers.SharedInformerFactory
	informerRebuildCh = make(chan struct{}, 1)
)

// triggerRebuild sends a non-blocking signal to rebuild the cache from
// informer stores.  Multiple rapid calls coalesce into a single rebuild.
func triggerRebuild() {
	select {
	case informerRebuildCh <- struct{}{}:
	default: // rebuild already pending
	}
}

// ─── Rebuild from Informer Stores ────────────────────────────────────
//
// Reads current state from in-memory informer stores (zero API calls)
// and copies it into the clusterCache fields that handlers consume.

func (c *clusterCache) rebuildFromInformers() {
	// Read from informer stores — all in-memory, no network I/O
	nodePtrs, _ := informerFactory.Core().V1().Nodes().Lister().List(labels.Everything())
	podPtrs, _ := informerFactory.Core().V1().Pods().Lister().List(labels.Everything())
	depPtrs, _ := informerFactory.Apps().V1().Deployments().Lister().List(labels.Everything())
	stsPtrs, _ := informerFactory.Apps().V1().StatefulSets().Lister().List(labels.Everything())
	dsPtrs, _ := informerFactory.Apps().V1().DaemonSets().Lister().List(labels.Everything())
	svcPtrs, _ := informerFactory.Core().V1().Services().Lister().List(labels.Everything())
	jobPtrs, _ := informerFactory.Batch().V1().Jobs().Lister().List(labels.Everything())
	cjPtrs, _ := informerFactory.Batch().V1().CronJobs().Lister().List(labels.Everything())
	nsPtrs, _ := informerFactory.Core().V1().Namespaces().Lister().List(labels.Everything())
	eventPtrs, _ := informerFactory.Core().V1().Events().Lister().List(labels.Everything())
	ingPtrs, _ := informerFactory.Networking().V1().Ingresses().Lister().List(labels.Everything())
	hpaPtrs, _ := informerFactory.Autoscaling().V2().HorizontalPodAutoscalers().Lister().List(labels.Everything())
	pdbPtrs, _ := informerFactory.Policy().V1().PodDisruptionBudgets().Lister().List(labels.Everything())
	rsPtrs, _ := informerFactory.Apps().V1().ReplicaSets().Lister().List(labels.Everything())
	pvcPtrs, _ := informerFactory.Core().V1().PersistentVolumeClaims().Lister().List(labels.Everything())
	pvPtrs, _ := informerFactory.Core().V1().PersistentVolumes().Lister().List(labels.Everything())
	scPtrs, _ := informerFactory.Storage().V1().StorageClasses().Lister().List(labels.Everything())
	cmPtrs, _ := informerFactory.Core().V1().ConfigMaps().Lister().List(labels.Everything())
	secPtrs, _ := informerFactory.Core().V1().Secrets().Lister().List(labels.Everything())

	// Convert pointer slices to typed lists (handlers expect *TypeList)
	nodes := &corev1.NodeList{Items: make([]corev1.Node, len(nodePtrs))}
	for i, p := range nodePtrs {
		nodes.Items[i] = *p
	}

	deps := &appsv1.DeploymentList{Items: make([]appsv1.Deployment, len(depPtrs))}
	for i, p := range depPtrs {
		deps.Items[i] = *p
	}

	sts := &appsv1.StatefulSetList{Items: make([]appsv1.StatefulSet, len(stsPtrs))}
	for i, p := range stsPtrs {
		sts.Items[i] = *p
	}

	ds := &appsv1.DaemonSetList{Items: make([]appsv1.DaemonSet, len(dsPtrs))}
	for i, p := range dsPtrs {
		ds.Items[i] = *p
	}

	svcs := &corev1.ServiceList{Items: make([]corev1.Service, len(svcPtrs))}
	for i, p := range svcPtrs {
		svcs.Items[i] = *p
	}

	jobs := &batchv1.JobList{Items: make([]batchv1.Job, len(jobPtrs))}
	for i, p := range jobPtrs {
		jobs.Items[i] = *p
	}

	cjobs := &batchv1.CronJobList{Items: make([]batchv1.CronJob, len(cjPtrs))}
	for i, p := range cjPtrs {
		cjobs.Items[i] = *p
	}

	nsList := &corev1.NamespaceList{Items: make([]corev1.Namespace, len(nsPtrs))}
	for i, p := range nsPtrs {
		nsList.Items[i] = *p
	}

	ings := &netv1.IngressList{Items: make([]netv1.Ingress, len(ingPtrs))}
	for i, p := range ingPtrs {
		ings.Items[i] = *p
	}

	hpas := &autov2.HorizontalPodAutoscalerList{Items: make([]autov2.HorizontalPodAutoscaler, len(hpaPtrs))}
	for i, p := range hpaPtrs {
		hpas.Items[i] = *p
	}

	pdbs := &policyv1.PodDisruptionBudgetList{Items: make([]policyv1.PodDisruptionBudget, len(pdbPtrs))}
	for i, p := range pdbPtrs {
		pdbs.Items[i] = *p
	}

	pvcList := &corev1.PersistentVolumeClaimList{Items: make([]corev1.PersistentVolumeClaim, len(pvcPtrs))}
	for i, p := range pvcPtrs {
		pvcList.Items[i] = *p
	}

	pvList := &corev1.PersistentVolumeList{Items: make([]corev1.PersistentVolume, len(pvPtrs))}
	for i, p := range pvPtrs {
		pvList.Items[i] = *p
	}

	scList := &storagev1.StorageClassList{Items: make([]storagev1.StorageClass, len(scPtrs))}
	for i, p := range scPtrs {
		scList.Items[i] = *p
	}

	// ── Compute ConfigMap metadata ──
	cmMeta := make([]configMeta, 0, len(cmPtrs))
	for _, cm := range cmPtrs {
		if cm.Name == "kube-root-ca.crt" {
			continue
		}
		keys := make([]string, 0, len(cm.Data)+len(cm.BinaryData))
		for k := range cm.Data {
			keys = append(keys, k)
		}
		for k := range cm.BinaryData {
			keys = append(keys, k+" (binary)")
		}
		sort.Strings(keys)
		lastMod := cm.CreationTimestamp.Time
		if cm.ManagedFields != nil {
			for _, mf := range cm.ManagedFields {
				if mf.Time != nil && mf.Time.Time.After(lastMod) {
					lastMod = mf.Time.Time
				}
			}
		}
		cmMeta = append(cmMeta, configMeta{Name: cm.Name, Namespace: cm.Namespace, Keys: keys, CreatedAt: cm.CreationTimestamp.Time, LastModified: lastMod, Version: cm.ResourceVersion})
	}

	// ── Compute Secret metadata ──
	secMeta := make([]configMeta, 0, len(secPtrs))
	for _, s := range secPtrs {
		if s.Type == corev1.SecretTypeServiceAccountToken {
			continue
		}
		if strings.HasPrefix(string(s.Type), "helm.sh/") {
			continue
		}
		keys := make([]string, 0, len(s.Data)+len(s.StringData))
		for k := range s.Data {
			keys = append(keys, k)
		}
		for k := range s.StringData {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		lastMod := s.CreationTimestamp.Time
		if s.ManagedFields != nil {
			for _, mf := range s.ManagedFields {
				if mf.Time != nil && mf.Time.Time.After(lastMod) {
					lastMod = mf.Time.Time
				}
			}
		}
		secMeta = append(secMeta, configMeta{Name: s.Name, Namespace: s.Namespace, Keys: keys, Type: string(s.Type), CreatedAt: s.CreationTimestamp.Time, LastModified: lastMod, Version: s.ResourceVersion})
	}

	// ── Pre-build ReplicaSet → owner lookup map ──
	rsMap := map[string]rsOwner{}
	for _, rs := range rsPtrs {
		for _, ref := range rs.OwnerReferences {
			if ref.Controller != nil && *ref.Controller {
				rsMap[rs.Namespace+"/"+rs.Name] = rsOwner{Kind: ref.Kind, Name: ref.Name}
				break
			}
		}
	}

	// Store everything under write lock
	c.mu.Lock()
	c.nodes = nodes
	c.pods = podPtrs
	c.deployments = deps
	c.statefulsets = sts
	c.daemonsets = ds
	c.services = svcs
	c.jobs = jobs
	c.cronjobs = cjobs
	c.namespaces = nsList
	c.events = eventPtrs
	c.ingresses = ings
	c.hpas = hpas
	c.configMeta = cmMeta
	c.secretMeta = secMeta
	c.pdbs = pdbs
	c.replicasets = rsPtrs
	c.pvcs = pvcList
	c.pvs = pvList
	c.storageClasses = scList
	c.rsOwners = rsMap
	c.lastRefresh = time.Now()
	c.mu.Unlock()

	// Compute config drift outside the lock
	drift := computeConfigDrift(podPtrs, cmMeta, secMeta)
	c.mu.Lock()
	c.configDrift = drift
	c.mu.Unlock()
}

// ─── Metrics Polling ─────────────────────────────────────────────────
//
// Metrics-server doesn't support Watch, so node/pod metrics are polled
// on a separate 10-second timer.

func (c *clusterCache) refreshMetrics() {
	if metricsCl == nil {
		return
	}

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	var nodeMetrics *metricsapi.NodeMetricsList
	var podMetrics *metricsapi.PodMetricsList
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		var err error
		nodeMetrics, err = metricsCl.MetricsV1beta1().NodeMetricses().List(ctx, metav1.ListOptions{})
		if err != nil {
			slog.Warn("metrics-server node metrics failed", "error", err)
		}
	}()
	go func() {
		defer wg.Done()
		var err error
		podMetrics, err = metricsCl.MetricsV1beta1().PodMetricses("").List(ctx, metav1.ListOptions{})
		if err != nil {
			slog.Warn("metrics-server pod metrics failed", "error", err)
		}
	}()
	wg.Wait()

	pm := map[string][2]int64{}
	if podMetrics != nil {
		for _, m := range podMetrics.Items {
			var cpu, mem int64
			for _, ct := range m.Containers {
				cpu += ct.Usage.Cpu().MilliValue()
				mem += ct.Usage.Memory().Value() / (1024 * 1024)
			}
			pm[m.Namespace+"/"+m.Name] = [2]int64{cpu, mem}
		}
		podSparklines.record(pm)
	}

	c.mu.Lock()
	if nodeMetrics != nil {
		c.nodeMetrics = nodeMetrics
	}
	if podMetrics != nil {
		c.podMetrics = podMetrics
	}
	c.podMetricsMap = pm
	c.mu.Unlock()
}

// refresh rebuilds resource state from informer stores and re-polls
// metrics.  Retained for backward-compatibility with mutation handlers
// that call `go cache.refresh()`.
func (c *clusterCache) refresh() {
	c.rebuildFromInformers()
	c.refreshMetrics()
}

// ─── Startup ─────────────────────────────────────────────────────────

func startCacheLoop() {
	// SharedInformerFactory: one initial List per resource type, then a
	// persistent Watch stream for incremental updates.  Full re-list
	// (resync) every 30 minutes as a consistency safety net.
	informerFactory = informers.NewSharedInformerFactory(clientset, 30*time.Minute)

	// Register all informers before Start()
	informerFactory.Core().V1().Nodes().Informer()
	informerFactory.Core().V1().Pods().Informer()
	informerFactory.Apps().V1().Deployments().Informer()
	informerFactory.Apps().V1().StatefulSets().Informer()
	informerFactory.Apps().V1().DaemonSets().Informer()
	informerFactory.Core().V1().Services().Informer()
	informerFactory.Batch().V1().Jobs().Informer()
	informerFactory.Batch().V1().CronJobs().Informer()
	informerFactory.Core().V1().Namespaces().Informer()
	informerFactory.Core().V1().Events().Informer()
	informerFactory.Networking().V1().Ingresses().Informer()
	informerFactory.Autoscaling().V2().HorizontalPodAutoscalers().Informer()
	informerFactory.Core().V1().ConfigMaps().Informer()
	informerFactory.Core().V1().Secrets().Informer()
	informerFactory.Policy().V1().PodDisruptionBudgets().Informer()
	informerFactory.Apps().V1().ReplicaSets().Informer()
	informerFactory.Core().V1().PersistentVolumeClaims().Informer()
	informerFactory.Core().V1().PersistentVolumes().Informer()
	informerFactory.Storage().V1().StorageClasses().Informer()

	stopCh := make(chan struct{})
	informerFactory.Start(stopCh)
	informerFactory.WaitForCacheSync(stopCh)

	// Initial cache build
	cache.rebuildFromInformers()
	cache.refreshMetrics()

	// ── Event-driven rebuilds with 500ms debounce ──
	handler := kcache.ResourceEventHandlerFuncs{
		AddFunc:    func(_ interface{}) { triggerRebuild() },
		UpdateFunc: func(_, _ interface{}) { triggerRebuild() },
		DeleteFunc: func(_ interface{}) { triggerRebuild() },
	}
	informerFactory.Core().V1().Pods().Informer().AddEventHandler(handler)
	informerFactory.Core().V1().Nodes().Informer().AddEventHandler(handler)
	informerFactory.Apps().V1().Deployments().Informer().AddEventHandler(handler)
	informerFactory.Apps().V1().StatefulSets().Informer().AddEventHandler(handler)
	informerFactory.Apps().V1().DaemonSets().Informer().AddEventHandler(handler)
	informerFactory.Apps().V1().ReplicaSets().Informer().AddEventHandler(handler)
	informerFactory.Batch().V1().Jobs().Informer().AddEventHandler(handler)
	informerFactory.Batch().V1().CronJobs().Informer().AddEventHandler(handler)
	informerFactory.Core().V1().Services().Informer().AddEventHandler(handler)
	informerFactory.Core().V1().Events().Informer().AddEventHandler(handler)
	informerFactory.Core().V1().ConfigMaps().Informer().AddEventHandler(handler)
	informerFactory.Core().V1().Secrets().Informer().AddEventHandler(handler)

	go func() {
		for range informerRebuildCh {
			time.Sleep(500 * time.Millisecond) // coalesce rapid events
			select {
			case <-informerRebuildCh:
			default:
			}
			cache.rebuildFromInformers()
		}
	}()

	// Metrics-server doesn't support Watch — poll separately
	go func() {
		ticker := time.NewTicker(10 * time.Second)
		for range ticker.C {
			cache.refreshMetrics()
		}
	}()
}
