//go:build linux

package server

import (
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/ashu-wolf/gostore/internal/storage"
)

func newTestServer(t *testing.T, opts ...Options) (*httptest.Server, *storage.Engine, string) {
	t.Helper()
	dir := t.TempDir()
	engine, _, err := storage.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = engine.Close() })
	var opt Options
	if len(opts) > 0 {
		opt = opts[0]
	}
	if opt.AccessKey == "" && opt.SecretKey == "" {
		opt.AllowUnauthenticated = true
	}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	ts := httptest.NewServer(New(log, engine, opt))
	t.Cleanup(ts.Close)
	return ts, engine, dir
}

func TestHealthz(t *testing.T) {
	dir := t.TempDir()
	engine, _, err := storage.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer engine.Close()

	h := New(slog.New(slog.NewTextHandler(io.Discard, nil)), engine)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/-/healthz", nil))

	if rec.Code != http.StatusOK || rec.Body.String() != "ok\n" {
		t.Fatalf("got status %d, body %q", rec.Code, rec.Body.String())
	}
	if rec.Header().Get("X-Amz-Request-Id") == "" {
		t.Fatal("missing request ID")
	}

	// Operational paths under /-/ must not hit bucket handlers.
	rec2 := httptest.NewRecorder()
	h.ServeHTTP(rec2, httptest.NewRequest(http.MethodGet, "/-/other", nil))
	if rec2.Code != http.StatusNotFound {
		t.Fatalf("got status %d for /-/other", rec2.Code)
	}
}
