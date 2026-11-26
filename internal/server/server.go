// Package server builds the HTTP handler.
package server

import (
	"crypto/rand"
	"encoding/hex"
	"io"
	"log/slog"
	"net/http"
	"time"
)

// New returns the application's HTTP handler.
// Bucket names cannot be "-", so paths under /-/ never collide with them.
func New(log *slog.Logger) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /-/healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/plain")
		_, _ = io.WriteString(w, "ok\n")
	})
	return withRequestLog(log, mux)
}

func withRequestLog(log *slog.Logger, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		id := newRequestID()
		w.Header().Set("X-Amz-Request-Id", id)
		next.ServeHTTP(w, r)
		log.Info("request",
			"request_id", id,
			"method", r.Method,
			"duration", time.Since(start),
		)
	})
}

func newRequestID() string {
	var b [8]byte
	_, _ = rand.Read(b[:])
	return hex.EncodeToString(b[:])
}
