//go:build linux

// Package server builds the S3-compatible HTTP handler.
package server

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/ashu-wolf/gostore/internal/domain"
	"github.com/ashu-wolf/gostore/internal/storage"
)

const defaultMaxObjectSize = int64(1 << 30) // 1 GiB

type requestIDKey struct{}

// Options configures the HTTP handler. Zero fields use defaults.
type Options struct {
	MaxObjectSize int64
	MaxListKeys   int
}

type handler struct {
	log           *slog.Logger
	store         *storage.Engine
	maxObjectSize int64
	maxListKeys   int
}

// New returns the application's HTTP handler.
// Bucket names cannot be "-", so paths under /-/ never collide with them.
func New(log *slog.Logger, store *storage.Engine, opts ...Options) http.Handler {
	var opt Options
	if len(opts) > 0 {
		opt = opts[0]
	}
	if opt.MaxObjectSize <= 0 {
		opt.MaxObjectSize = defaultMaxObjectSize
	}
	if opt.MaxListKeys <= 0 || opt.MaxListKeys > domain.DefaultMaxKeys {
		opt.MaxListKeys = domain.DefaultMaxKeys
	}
	h := &handler{
		log:           log,
		store:         store,
		maxObjectSize: opt.MaxObjectSize,
		maxListKeys:   opt.MaxListKeys,
	}
	return withRequestLog(log, h)
}

func (h *handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	// Parse from EscapedPath directly so net/http's path cleaning never
	// redirects or alters keys like "../../etc/passwd".
	raw := strings.TrimPrefix(r.URL.EscapedPath(), "/")
	if raw == "-/healthz" {
		if r.Method != http.MethodGet {
			h.writeError(w, r, http.StatusMethodNotAllowed, "MethodNotAllowed", "The specified method is not allowed against this resource.")
			return
		}
		w.Header().Set("Content-Type", "text/plain")
		_, _ = io.WriteString(w, "ok\n")
		return
	}
	if raw == "-" || strings.HasPrefix(raw, "-/") {
		http.NotFound(w, r)
		return
	}

	if raw == "" {
		h.handleService(w, r)
		return
	}

	bucketEsc, keyEsc, hasSlash := strings.Cut(raw, "/")
	bucket, err := url.PathUnescape(bucketEsc)
	if err != nil {
		h.writeError(w, r, http.StatusBadRequest, "InvalidURI", "Could not parse the specified URI.")
		return
	}
	if !hasSlash || keyEsc == "" {
		h.handleBucket(w, r, bucket)
		return
	}
	key, err := url.PathUnescape(keyEsc)
	if err != nil {
		h.writeError(w, r, http.StatusBadRequest, "InvalidURI", "Could not parse the specified URI.")
		return
	}
	h.handleObject(w, r, bucket, key)
}

func withRequestLog(log *slog.Logger, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		id := newRequestID()
		w.Header().Set("X-Amz-Request-Id", id)
		ctx := context.WithValue(r.Context(), requestIDKey{}, id)
		next.ServeHTTP(w, r.WithContext(ctx))
		log.Info("request",
			"request_id", id,
			"method", r.Method,
			"duration", time.Since(start),
		)
	})
}

func requestID(r *http.Request) string {
	if id, ok := r.Context().Value(requestIDKey{}).(string); ok {
		return id
	}
	return ""
}

func newRequestID() string {
	var b [8]byte
	_, _ = rand.Read(b[:])
	return hex.EncodeToString(b[:])
}
