package api

import (
	"testing"

	apiextv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
)

// servedVersion picks the storage version when marked served, falling back
// to the first served version. Returns "" when no version is served (which
// shouldn't happen for a healthy CRD but must degrade gracefully).
func TestServedVersion(t *testing.T) {
	cases := []struct {
		name     string
		versions []apiextv1.CustomResourceDefinitionVersion
		want     string
	}{
		{
			name: "storage-and-served — prefer storage",
			versions: []apiextv1.CustomResourceDefinitionVersion{
				{Name: "v1beta1", Served: true, Storage: false},
				{Name: "v1", Served: true, Storage: true},
			},
			want: "v1",
		},
		{
			name: "storage-version not served — fall back to first served",
			versions: []apiextv1.CustomResourceDefinitionVersion{
				{Name: "v1", Served: false, Storage: true},
				{Name: "v1beta1", Served: true, Storage: false},
			},
			want: "v1beta1",
		},
		{
			name: "no served versions — empty",
			versions: []apiextv1.CustomResourceDefinitionVersion{
				{Name: "v1alpha1", Served: false, Storage: true},
			},
			want: "",
		},
		{
			name: "single served — takes it",
			versions: []apiextv1.CustomResourceDefinitionVersion{
				{Name: "v2", Served: true, Storage: true},
			},
			want: "v2",
		},
		{
			name:     "empty versions — empty",
			versions: nil,
			want:     "",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			crd := &apiextv1.CustomResourceDefinition{
				Spec: apiextv1.CustomResourceDefinitionSpec{Versions: c.versions},
			}
			if got := servedVersion(crd); got != c.want {
				t.Errorf("servedVersion = %q, want %q", got, c.want)
			}
		})
	}
}
