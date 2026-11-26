//go:build linux

package main

import (
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"

	"github.com/ashu-wolf/gostore/internal/server"
	"github.com/ashu-wolf/gostore/internal/storage"
)

func TestPercentile(t *testing.T) {
	vals := []float64{10, 20, 30, 40, 50, 60, 70, 80, 90, 100}
	if got := percentile(vals, 0.50); got != 50 {
		t.Fatalf("p50 = %v, want 50", got)
	}
	if got := percentile(vals, 0.99); got != 90 {
		t.Fatalf("p99 = %v, want 90", got)
	}
	if got := percentile(nil, 0.50); got != 0 {
		t.Fatalf("empty p50 = %v, want 0", got)
	}
}

func TestBenchClientAgainstServer(t *testing.T) {
	store, _, err := storage.OpenWithOptions(t.TempDir(), storage.Options{
		DisableSpaceCheck:   true,
		DisableExpiryWorker: true,
	})
	if err != nil {
		t.Fatalf("OpenWithOptions: %v", err)
	}
	defer store.Close()

	h := server.New(slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError})), store, server.Options{
		AccessKey: benchAccessKey,
		SecretKey: benchSecretKey,
		Region:    benchRegion,
	})
	ts := httptest.NewServer(h)
	defer ts.Close()

	c := newBenchClient(ts.URL, benchAccessKey, benchSecretKey, benchRegion)
	if err := c.createBucket("bench-test"); err != nil {
		t.Fatalf("createBucket: %v", err)
	}
	if err := c.putObject("bench-test", "dir/a.txt", []byte("hello")); err != nil {
		t.Fatalf("putObject: %v", err)
	}
	n, err := c.getObject("bench-test", "dir/a.txt")
	if err != nil || n != 5 {
		t.Fatalf("getObject = (%d, %v), want (5, nil)", n, err)
	}
	if _, err := c.listObjects("bench-test", "list-type=2&prefix=dir/&delimiter=/&max-keys=10"); err != nil {
		t.Fatalf("listObjects: %v", err)
	}
	if got := detectFS(t.TempDir()); got == "" {
		t.Fatal("detectFS returned empty")
	}
	pid := os.Getpid()
	if _, err := readProcCPUTicks(pid); err != nil {
		t.Fatalf("readProcCPUTicks: %v", err)
	}
	if rss, err := readProcRSSKB(pid); err != nil || rss <= 0 {
		t.Fatalf("readProcRSSKB = (%d, %v), want >0", rss, err)
	}
	if fds, err := countProcFDs(pid); err != nil || fds <= 0 {
		t.Fatalf("countProcFDs = (%d, %v), want >0", fds, err)
	}
}

var _ = http.StatusOK
