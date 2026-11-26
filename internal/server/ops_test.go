//go:build linux

package server

import (
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/ashu-wolf/gostore/internal/storage"
)

func TestReadyzAndMetrics(t *testing.T) {
	dir := t.TempDir()
	engine, _, err := storage.OpenWithOptions(dir, storage.Options{
		DisableSpaceCheck:   true,
		DisableExpiryWorker: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer engine.Close()

	h := New(slog.New(slog.NewTextHandler(io.Discard, nil)), engine, Options{AllowUnauthenticated: true})
	ts := httptest.NewServer(h)
	defer ts.Close()

	resp, body := doReq(t, http.MethodGet, ts.URL+"/-/readyz", nil, nil)
	if resp.StatusCode != http.StatusOK || string(body) != "ready\n" {
		t.Fatalf("readyz: status=%d body=%q", resp.StatusCode, body)
	}

	doReq(t, http.MethodPut, ts.URL+"/photos", nil, nil)
	doReq(t, http.MethodPut, ts.URL+"/photos/secret-key-name.bin", strings.NewReader("payload-123"), nil)
	doReq(t, http.MethodGet, ts.URL+"/photos/secret-key-name.bin", nil, nil)
	doReq(t, http.MethodGet, ts.URL+"/photos/missing-secret-key.bin", nil, nil)

	// Scrape metrics and verify expected families and zero key leakage.
	mResp, mBody := doReq(t, http.MethodGet, ts.URL+"/-/metrics", nil, nil)
	if mResp.StatusCode != http.StatusOK {
		t.Fatalf("metrics status=%d", mResp.StatusCode)
	}
	metricsText := string(mBody)
	for _, want := range []string{
		"gostore_ready 1",
		"gostore_active_requests 0",
		`gostore_requests_total{op="create_bucket",status="200"} 1`,
		`gostore_requests_total{op="put_object",status="200"} 1`,
		`gostore_requests_total{op="get_object",status="200"} 1`,
		`gostore_requests_total{op="get_object",status="404"} 1`,
		`gostore_errors_total{op="get_object",code="NoSuchKey"} 1`,
		`gostore_request_bytes_total{direction="in"} 11`,
		`gostore_request_bytes_total{direction="out"} 11`,
		`gostore_request_duration_seconds_count{op="put_object"} 1`,
	} {
		if !strings.Contains(metricsText, want) {
			t.Fatalf("metrics missing %q:\n%s", want, metricsText)
		}
	}
	if strings.Contains(metricsText, "secret-key-name") || strings.Contains(metricsText, "missing-secret-key") {
		t.Fatalf("metrics leaked object key:\n%s", metricsText)
	}

	// Mark unready: /-/readyz and new S3 requests return 503; /-/healthz stays 200.
	h.SetReady(false)
	resp, body = doReq(t, http.MethodGet, ts.URL+"/-/readyz", nil, nil)
	if resp.StatusCode != http.StatusServiceUnavailable || string(body) != "not ready\n" {
		t.Fatalf("unready /-/readyz: status=%d body=%q", resp.StatusCode, body)
	}
	resp, body = doReq(t, http.MethodGet, ts.URL+"/", nil, nil)
	if resp.StatusCode != http.StatusServiceUnavailable || decodeErrorXML(t, body).Code != "ServiceUnavailable" {
		t.Fatalf("unready S3 request: status=%d body=%s", resp.StatusCode, body)
	}
	resp, body = doReq(t, http.MethodGet, ts.URL+"/-/healthz", nil, nil)
	if resp.StatusCode != http.StatusOK || string(body) != "ok\n" {
		t.Fatalf("healthz when unready: status=%d body=%q", resp.StatusCode, body)
	}
}
