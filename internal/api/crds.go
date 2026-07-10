package api

import (
	"encoding/json"
	"fmt"
	"kube-argus/internal/httpx"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"

	apiextv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	apiextclient "k8s.io/apiextensions-apiserver/pkg/client/clientset/clientset"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic"
)

// ─── CRD Browser ─────────────────────────────────────────────────────
//
// Surfaces every CustomResourceDefinition installed in the cluster and lets
// you list/inspect their instances. Backed by the dynamic client so we don't
// need to know about each CRD's Go types at compile time.

var (
	dynamicClient dynamic.Interface
	apiextClient  apiextclient.Interface

	crdCache struct {
		mu      sync.RWMutex
		crds    []apiextv1.CustomResourceDefinition
		fetched time.Time
	}
)

const crdCacheTTL = 30 * time.Second

func initCRDClients() {
	if restCfg == nil {
		return
	}
	dc, err := dynamic.NewForConfig(restCfg)
	if err != nil {
		return
	}
	dynamicClient = dc

	ac, err := apiextclient.NewForConfig(restCfg)
	if err == nil {
		apiextClient = ac
	}
}

// crdList returns every CRD installed in the cluster, cached briefly.
func crdList(r *http.Request) ([]apiextv1.CustomResourceDefinition, error) {
	crdCache.mu.RLock()
	if time.Since(crdCache.fetched) < crdCacheTTL && crdCache.crds != nil {
		out := crdCache.crds
		crdCache.mu.RUnlock()
		return out, nil
	}
	crdCache.mu.RUnlock()

	if apiextClient == nil {
		return nil, fmt.Errorf("apiextensions client not initialized")
	}

	list, err := apiextClient.ApiextensionsV1().CustomResourceDefinitions().List(r.Context(), metav1.ListOptions{})
	if err != nil {
		return nil, err
	}
	crdCache.mu.Lock()
	crdCache.crds = list.Items
	crdCache.fetched = time.Now()
	crdCache.mu.Unlock()
	return list.Items, nil
}

// servedVersion returns the storage version of a CRD (preferred) or the first
// served version. Returns "" if no version is served.
func servedVersion(crd *apiextv1.CustomResourceDefinition) string {
	for _, v := range crd.Spec.Versions {
		if v.Storage && v.Served {
			return v.Name
		}
	}
	for _, v := range crd.Spec.Versions {
		if v.Served {
			return v.Name
		}
	}
	return ""
}

// GET /api/crds — list all CRDs grouped by API group.
func apiCRDs(w http.ResponseWriter, r *http.Request) {
	crds, err := crdList(r)
	if err != nil {
		httpx.Error(w, err.Error(), 500)
		return
	}

	type entry struct {
		Group      string   `json:"group"`
		Version    string   `json:"version"`
		Kind       string   `json:"kind"`
		Plural     string   `json:"plural"`
		Singular   string   `json:"singular"`
		Scope      string   `json:"scope"` // Namespaced | Cluster
		ShortNames []string `json:"shortNames,omitempty"`
		Categories []string `json:"categories,omitempty"`
	}

	out := make([]entry, 0, len(crds))
	for i := range crds {
		c := &crds[i]
		v := servedVersion(c)
		if v == "" {
			continue
		}
		out = append(out, entry{
			Group:      c.Spec.Group,
			Version:    v,
			Kind:       c.Spec.Names.Kind,
			Plural:     c.Spec.Names.Plural,
			Singular:   c.Spec.Names.Singular,
			Scope:      string(c.Spec.Scope),
			ShortNames: c.Spec.Names.ShortNames,
			Categories: c.Spec.Names.Categories,
		})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Group != out[j].Group {
			return out[i].Group < out[j].Group
		}
		return out[i].Kind < out[j].Kind
	})
	httpx.JSON(w, out)
}

// crdGVR resolves a {group, version, plural} triplet to a GroupVersionResource
// after verifying the CRD exists and the version is served.
func crdGVR(r *http.Request, group, version, plural string) (schema.GroupVersionResource, bool, error) {
	crds, err := crdList(r)
	if err != nil {
		return schema.GroupVersionResource{}, false, err
	}
	for _, c := range crds {
		if c.Spec.Group != group || c.Spec.Names.Plural != plural {
			continue
		}
		// Verify the requested version is served
		for _, v := range c.Spec.Versions {
			if v.Name == version && v.Served {
				return schema.GroupVersionResource{Group: group, Version: version, Resource: plural},
					c.Spec.Scope == apiextv1.NamespaceScoped, nil
			}
		}
		return schema.GroupVersionResource{}, false, fmt.Errorf("version %q not served by %s/%s", version, group, plural)
	}
	return schema.GroupVersionResource{}, false, fmt.Errorf("CRD %s/%s not found", group, plural)
}

// GET /api/crd/{group}/{version}/{plural}?namespace=X — list instances.
// GET /api/crd/{group}/{version}/{plural}/{ns}/{name} — get one instance (full YAML).
func apiCRDResource(w http.ResponseWriter, r *http.Request) {
	if dynamicClient == nil {
		httpx.Error(w, "dynamic client not initialized", 500)
		return
	}
	path := strings.TrimPrefix(r.URL.Path, "/api/crd/")
	parts := strings.Split(path, "/")
	if len(parts) < 3 {
		httpx.Error(w, "use /api/crd/{group}/{version}/{plural}[/{ns}/{name}]", 400)
		return
	}
	group, version, plural := parts[0], parts[1], parts[2]
	// "core" group is represented as empty string in k8s API
	if group == "_" || group == "core" {
		group = ""
	}

	gvr, namespaced, err := crdGVR(r, group, version, plural)
	if err != nil {
		httpx.Error(w, err.Error(), 404)
		return
	}

	// /{ns}/{name} → get single instance
	if len(parts) >= 5 {
		ns, name := parts[3], parts[4]
		var obj any
		var getErr error
		if namespaced {
			obj, getErr = dynamicClient.Resource(gvr).Namespace(ns).Get(r.Context(), name, metav1.GetOptions{})
		} else {
			obj, getErr = dynamicClient.Resource(gvr).Get(r.Context(), name, metav1.GetOptions{})
		}
		if getErr != nil {
			httpx.K8sError(w, getErr)
			return
		}
		httpx.JSON(w, obj)
		return
	}

	// /{group}/{version}/{plural}?namespace=X → list
	nsFilter := r.URL.Query().Get("namespace")
	var listErr error
	var list any
	if namespaced && nsFilter != "" {
		list, listErr = dynamicClient.Resource(gvr).Namespace(nsFilter).List(r.Context(), metav1.ListOptions{Limit: 500})
	} else {
		list, listErr = dynamicClient.Resource(gvr).List(r.Context(), metav1.ListOptions{Limit: 500})
	}
	if listErr != nil {
		httpx.K8sError(w, listErr)
		return
	}

	// Project the dynamic list into a thin, browse-friendly shape.
	raw, err := json.Marshal(list)
	if err != nil {
		httpx.Error(w, err.Error(), 500)
		return
	}
	var unstructured struct {
		Items []struct {
			Metadata struct {
				Name              string            `json:"name"`
				Namespace         string            `json:"namespace,omitempty"`
				CreationTimestamp string            `json:"creationTimestamp"`
				Labels            map[string]string `json:"labels,omitempty"`
			} `json:"metadata"`
		} `json:"items"`
	}
	if err := json.Unmarshal(raw, &unstructured); err != nil {
		httpx.Error(w, err.Error(), 500)
		return
	}

	type instance struct {
		Name      string `json:"name"`
		Namespace string `json:"namespace,omitempty"`
		Age       string `json:"age"`
		Labels    int    `json:"labels"`
	}
	out := make([]instance, 0, len(unstructured.Items))
	for _, item := range unstructured.Items {
		age := ""
		if ts, err := time.Parse(time.RFC3339, item.Metadata.CreationTimestamp); err == nil {
			age = shortDur(time.Since(ts))
		}
		out = append(out, instance{
			Name:      item.Metadata.Name,
			Namespace: item.Metadata.Namespace,
			Age:       age,
			Labels:    len(item.Metadata.Labels),
		})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Namespace != out[j].Namespace {
			return out[i].Namespace < out[j].Namespace
		}
		return out[i].Name < out[j].Name
	})
	httpx.JSON(w, out)
}
