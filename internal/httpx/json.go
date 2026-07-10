// Package httpx contains small HTTP helpers shared across the server.
package httpx

import (
	"encoding/json"
	"net/http"

	k8serr "k8s.io/apimachinery/pkg/api/errors"
)

// JSON writes v as a JSON response with Content-Type application/json.
// On encoding failure it falls back to Error with a 500 status.
func JSON(w http.ResponseWriter, v interface{}) {
	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(v); err != nil {
		Error(w, "encoding failed", http.StatusInternalServerError)
	}
}

// Error writes a JSON error body {"error": msg} with the given status code.
func Error(w http.ResponseWriter, msg string, code int) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	json.NewEncoder(w).Encode(map[string]string{"error": msg})
}

// K8sError writes a JSON error, returning 404 for k8s NotFound errors, 500 otherwise.
func K8sError(w http.ResponseWriter, err error) {
	if k8serr.IsNotFound(err) {
		Error(w, "not found", http.StatusNotFound)
	} else {
		Error(w, err.Error(), http.StatusInternalServerError)
	}
}

// JSONGz writes v as JSON. The gzipWrap middleware already handles compression
// transparently; this helper exists for API symmetry with the older codebase.
func JSONGz(w http.ResponseWriter, _ *http.Request, v interface{}) {
	JSON(w, v)
}
