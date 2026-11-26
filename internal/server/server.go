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
	"sync"
	"time"

	"github.com/ashu-wolf/gostore/internal/domain"
	"github.com/ashu-wolf/gostore/internal/metrics"
	"github.com/ashu-wolf/gostore/internal/storage"
)

const (
	defaultMaxObjectSize         = int64(1 << 30) // 1 GiB
	defaultMaxConcurrentRequests = 256
	defaultIdleTransferTimeout   = 60 * time.Second
	defaultRegion                = "us-east-1"
)

type (
	requestIDKey  struct{}
	authResultKey struct{}
	reqMetricsKey struct{}
)

type reqMetrics struct {
	op      string
	errCode string
}

// Options configures the HTTP handler. Zero fields use defaults.
type Options struct {
	MaxObjectSize         int64
	MaxListKeys           int
	MaxConcurrentRequests int
	IdleTransferTimeout   time.Duration
	AccessKey             string
	SecretKey             string
	Region                string
	ClockSkew             time.Duration
	Now                   func() time.Time
	AllowUnauthenticated  bool
	Metrics               *metrics.Registry
}

// Handler serves GoStore's S3 and operational endpoints and coordinates
// readiness and graceful request draining.
type Handler struct {
	log                 *slog.Logger
	store               *storage.Engine
	metrics             *metrics.Registry
	maxObjectSize       int64
	maxListKeys         int
	idleTransferTimeout time.Duration
	sem                 chan struct{}
	wg                  sync.WaitGroup
	allowUnauth         bool
	authCfg             sigV4Config
}

// New returns the application's HTTP handler, initially marked ready.
// Bucket names cannot be "-", so paths under /-/ never collide with them.
func New(log *slog.Logger, store *storage.Engine, opts ...Options) *Handler {
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
	if opt.MaxConcurrentRequests <= 0 {
		opt.MaxConcurrentRequests = defaultMaxConcurrentRequests
	}
	if opt.IdleTransferTimeout <= 0 {
		opt.IdleTransferTimeout = defaultIdleTransferTimeout
	}
	if opt.Region == "" {
		opt.Region = defaultRegion
	}
	if opt.ClockSkew <= 0 {
		opt.ClockSkew = defaultClockSkew
	}
	if opt.Now == nil {
		opt.Now = time.Now
	}
	reg := opt.Metrics
	if reg == nil {
		reg = metrics.New()
	}
	reg.SetReady(true)

	return &Handler{
		log:                 log,
		store:               store,
		metrics:             reg,
		maxObjectSize:       opt.MaxObjectSize,
		maxListKeys:         opt.MaxListKeys,
		idleTransferTimeout: opt.IdleTransferTimeout,
		sem:                 make(chan struct{}, opt.MaxConcurrentRequests),
		allowUnauth:         opt.AllowUnauthenticated,
		authCfg: sigV4Config{
			accessKey: opt.AccessKey,
			secretKey: opt.SecretKey,
			region:    opt.Region,
			service:   "s3",
			clockSkew: opt.ClockSkew,
			now:       opt.Now,
		},
	}
}

// SetReady updates readiness for /-/readyz and new S3 requests.
func (h *Handler) SetReady(ready bool) {
	h.metrics.SetReady(ready)
}

// Ready reports whether the handler is currently ready to accept requests.
func (h *Handler) Ready() bool {
	return h.metrics.Ready()
}

// Wait blocks until all in-flight S3 request handlers have returned.
func (h *Handler) Wait() {
	h.wg.Wait()
}

// Metrics returns the handler's metric registry.
func (h *Handler) Metrics() *metrics.Registry {
	return h.metrics
}

func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	start := time.Now()
	id := newRequestID()
	w.Header().Set("X-Amz-Request-Id", id)
	ctx := context.WithValue(r.Context(), requestIDKey{}, id)
	r = r.WithContext(ctx)

	// Parse from EscapedPath directly so net/http's path cleaning never
	// redirects or alters keys like "../../etc/passwd".
	raw := strings.TrimPrefix(r.URL.EscapedPath(), "/")
	if raw == "-" || strings.HasPrefix(raw, "-/") {
		h.serveOperational(w, r, raw)
		h.logRequest(id, r.Method, start)
		return
	}

	rm := &reqMetrics{op: "unclassified"}
	r = r.WithContext(context.WithValue(r.Context(), reqMetricsKey{}, rm))
	rec := &statusRecorder{ResponseWriter: w, status: http.StatusOK}
	defer func() {
		h.metrics.ObserveRequest(rm.op, rec.status, rm.errCode, time.Since(start))
		h.logRequest(id, r.Method, start)
	}()

	if !h.metrics.Ready() {
		setOp(r, "rejected")
		rec.Header().Set("Connection", "close")
		rec.Header().Set("Retry-After", "1")
		h.writeError(rec, r, http.StatusServiceUnavailable, "ServiceUnavailable", "Server is shutting down.")
		return
	}

	if err := r.Context().Err(); err != nil {
		setOp(r, "canceled")
		return
	}

	select {
	case h.sem <- struct{}{}:
		defer func() { <-h.sem }()
	default:
		setOp(r, "throttled")
		rec.Header().Set("Retry-After", "1")
		h.writeError(rec, r, http.StatusServiceUnavailable, "SlowDown", "Please reduce your request rate.")
		return
	}

	h.wg.Add(1)
	defer h.wg.Done()
	h.metrics.IncActive()
	defer h.metrics.DecActive()

	if !h.allowUnauth {
		auth, authErr := verifySigV4(r, h.authCfg)
		if authErr != nil {
			setOp(r, "auth")
			h.writeError(rec, r, authErr.status, authErr.code, authErr.message)
			return
		}
		r = r.WithContext(context.WithValue(r.Context(), authResultKey{}, auth))
	}

	if raw == "" {
		h.handleService(rec, r)
		return
	}

	bucketEsc, keyEsc, hasSlash := strings.Cut(raw, "/")
	bucket, err := url.PathUnescape(bucketEsc)
	if err != nil {
		setOp(r, "invalid_uri")
		h.writeError(rec, r, http.StatusBadRequest, "InvalidURI", "Could not parse the specified URI.")
		return
	}
	if !hasSlash || keyEsc == "" {
		h.handleBucket(rec, r, bucket)
		return
	}
	key, err := url.PathUnescape(keyEsc)
	if err != nil {
		setOp(r, "invalid_uri")
		h.writeError(rec, r, http.StatusBadRequest, "InvalidURI", "Could not parse the specified URI.")
		return
	}
	h.handleObject(rec, r, bucket, key)
}

func (h *Handler) serveOperational(w http.ResponseWriter, r *http.Request, raw string) {
	switch raw {
	case "-/healthz":
		if r.Method != http.MethodGet {
			h.writeError(w, r, http.StatusMethodNotAllowed, "MethodNotAllowed", "The specified method is not allowed against this resource.")
			return
		}
		w.Header().Set("Content-Type", "text/plain")
		_, _ = io.WriteString(w, "ok\n")
	case "-/readyz":
		if r.Method != http.MethodGet {
			h.writeError(w, r, http.StatusMethodNotAllowed, "MethodNotAllowed", "The specified method is not allowed against this resource.")
			return
		}
		w.Header().Set("Content-Type", "text/plain")
		if !h.metrics.Ready() {
			w.WriteHeader(http.StatusServiceUnavailable)
			_, _ = io.WriteString(w, "not ready\n")
			return
		}
		_, _ = io.WriteString(w, "ready\n")
	case "-/metrics":
		if r.Method != http.MethodGet {
			h.writeError(w, r, http.StatusMethodNotAllowed, "MethodNotAllowed", "The specified method is not allowed against this resource.")
			return
		}
		w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
		h.metrics.WritePrometheus(w)
	default:
		http.NotFound(w, r)
	}
}

func (h *Handler) logRequest(id, method string, start time.Time) {
	h.log.Info("request",
		"request_id", id,
		"method", method,
		"duration", time.Since(start),
	)
}

type statusRecorder struct {
	http.ResponseWriter
	status      int
	wroteHeader bool
}

func (s *statusRecorder) WriteHeader(code int) {
	if !s.wroteHeader {
		s.status = code
		s.wroteHeader = true
	}
	s.ResponseWriter.WriteHeader(code)
}

func (s *statusRecorder) Write(p []byte) (int, error) {
	if !s.wroteHeader {
		s.wroteHeader = true
	}
	return s.ResponseWriter.Write(p)
}

func (s *statusRecorder) Unwrap() http.ResponseWriter {
	return s.ResponseWriter
}

func setOp(r *http.Request, op string) {
	if rm, ok := r.Context().Value(reqMetricsKey{}).(*reqMetrics); ok {
		rm.op = op
	}
}

func setErrCode(r *http.Request, code string) {
	if rm, ok := r.Context().Value(reqMetricsKey{}).(*reqMetrics); ok {
		rm.errCode = code
	}
}

func requestID(r *http.Request) string {
	if id, ok := r.Context().Value(requestIDKey{}).(string); ok {
		return id
	}
	return ""
}

func authFromContext(r *http.Request) (authResult, bool) {
	a, ok := r.Context().Value(authResultKey{}).(authResult)
	return a, ok
}

func newRequestID() string {
	var b [8]byte
	_, _ = rand.Read(b[:])
	return hex.EncodeToString(b[:])
}
