package httpx

import (
	"encoding/json"
	"fmt"
	"net/http/httptest"
	"strings"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	k8serr "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

func TestJSON_WritesContentTypeAndBody(t *testing.T) {
	rec := httptest.NewRecorder()
	JSON(rec, map[string]string{"hello": "world"})

	if got := rec.Header().Get("Content-Type"); got != "application/json" {
		t.Errorf("Content-Type = %q, want application/json", got)
	}
	if rec.Code != 200 {
		t.Errorf("status = %d, want 200", rec.Code)
	}
	var got map[string]string
	if err := json.NewDecoder(rec.Body).Decode(&got); err != nil {
		t.Fatalf("body not valid JSON: %v", err)
	}
	if got["hello"] != "world" {
		t.Errorf("body = %v, want {hello: world}", got)
	}
}

func TestError_WritesStatusAndErrorField(t *testing.T) {
	rec := httptest.NewRecorder()
	Error(rec, "bad input", 400)

	if rec.Code != 400 {
		t.Errorf("status = %d, want 400", rec.Code)
	}
	if got := rec.Header().Get("Content-Type"); got != "application/json" {
		t.Errorf("Content-Type = %q, want application/json", got)
	}
	body := rec.Body.String()
	if !strings.Contains(body, `"error":"bad input"`) {
		t.Errorf("body missing error field: %q", body)
	}
}

// K8sError must translate NotFound to 404 and everything else to 500.
// A regression here would send NotFound as 500 which is confusing during
// terminated-node / deleted-workload scenarios.
func TestK8sError_TranslatesNotFoundTo404(t *testing.T) {
	rec := httptest.NewRecorder()
	nf := k8serr.NewNotFound(schema.GroupResource{Resource: "pods"}, "gone")
	K8sError(rec, nf)

	if rec.Code != 404 {
		t.Errorf("expected 404 for NotFound, got %d", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "not found") {
		t.Errorf("expected 'not found' body, got %q", rec.Body.String())
	}
}

func TestK8sError_UnknownIs500(t *testing.T) {
	rec := httptest.NewRecorder()
	K8sError(rec, fmt.Errorf("connection refused"))

	if rec.Code != 500 {
		t.Errorf("expected 500 for generic error, got %d", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "connection refused") {
		t.Errorf("expected error message in body, got %q", rec.Body.String())
	}
}

// Make sure a legitimate k8s API error (Status object) still round-trips.
func TestK8sError_APIStatusUnauthorized(t *testing.T) {
	rec := httptest.NewRecorder()
	e := &k8serr.StatusError{ErrStatus: metav1.Status{
		Status: metav1.StatusFailure, Code: 401, Reason: metav1.StatusReasonUnauthorized,
		Message: "you must be logged in",
	}}
	K8sError(rec, e)
	// Not-NotFound → 500 branch; the JSON body includes the k8s error text.
	if rec.Code != 500 {
		t.Errorf("expected 500 for non-NotFound k8s error, got %d", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "you must be logged in") {
		t.Errorf("expected k8s message in body, got %q", rec.Body.String())
	}
}
