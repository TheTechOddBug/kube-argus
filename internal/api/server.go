package api

import (
	"compress/gzip"
	"context"
	"net/http"
	"strings"
	"time"

	"kube-argus/internal/auth"
)

// CORSWrap is the public alias used by main.
func CORSWrap(h http.Handler) http.Handler { return corsWrap(h) }

// GzipWrap is the public alias used by main.
func GzipWrap(h http.Handler) http.Handler { return gzipWrap(h) }

func corsWrap(h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		origin := auth.CORSOrigin
		if origin == "" {
			origin = "*"
		}
		w.Header().Set("Access-Control-Allow-Origin", origin)
		w.Header().Set("Access-Control-Allow-Methods", "GET,POST,DELETE,OPTIONS")
		w.Header().Set("Access-Control-Allow-Headers", "Content-Type")
		if r.Method == "OPTIONS" {
			return
		}
		h.ServeHTTP(w, r)
	})
}

type gzResponseWriter struct {
	http.ResponseWriter
	gz *gzip.Writer
}

func (g *gzResponseWriter) Write(b []byte) (int, error) { return g.gz.Write(b) }

func gzipWrap(h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.Contains(r.Header.Get("Accept-Encoding"), "gzip") {
			h.ServeHTTP(w, r)
			return
		}
		p := r.URL.Path
		if p == "/api/exec" || p == "/api/ws/presence" || p == "/api/events" || p == "/api/ai/diagnose" || strings.HasSuffix(p, "/agglogs") || r.URL.Query().Get("follow") == "true" || (strings.HasSuffix(p, "/drain") && r.URL.Query().Get("stream") == "true") {
			h.ServeHTTP(w, r)
			return
		}
		if strings.HasPrefix(p, "/api/") {
			w.Header().Set("Content-Encoding", "gzip")
			gz := gzip.NewWriter(w)
			defer gz.Close()
			h.ServeHTTP(&gzResponseWriter{ResponseWriter: w, gz: gz}, r)
			return
		}
		h.ServeHTTP(w, r)
	})
}

func ctx() (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.Background(), 15*time.Second)
}
