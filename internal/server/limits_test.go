//go:build linux

package server

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ashu-wolf/gostore/internal/storage"
)

type slowBodyReader struct {
	started chan struct{}
	release chan struct{}
	once    sync.Once
	done    bool
}

func (s *slowBodyReader) Read(p []byte) (int, error) {
	s.once.Do(func() { close(s.started) })
	<-s.release
	if s.done {
		return 0, io.EOF
	}
	s.done = true
	return copy(p, "ok"), nil
}

func TestSemaphoreOverloadReturns503(t *testing.T) {
	ts, engine, _ := newTestServer(t, Options{
		MaxConcurrentRequests: 2,
		AllowUnauthenticated:  true,
	})
	if err := engine.CreateBucket("photos"); err != nil {
		t.Fatal(err)
	}

	r1 := &slowBodyReader{started: make(chan struct{}), release: make(chan struct{})}
	r2 := &slowBodyReader{started: make(chan struct{}), release: make(chan struct{})}

	var wg sync.WaitGroup
	wg.Add(2)
	for i, rdr := range []*slowBodyReader{r1, r2} {
		key := string(rune('a' + i))
		go func(k string, r io.Reader) {
			defer wg.Done()
			req, _ := http.NewRequest(http.MethodPut, ts.URL+"/photos/"+k, r)
			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				t.Error(err)
				return
			}
			resp.Body.Close()
			if resp.StatusCode != http.StatusOK {
				t.Errorf("upload %s status=%d", k, resp.StatusCode)
			}
		}(key, rdr)
	}
	<-r1.started
	<-r2.started

	// Wait until both uploads hold the server's semaphore slots (metrics bypasses the semaphore).
	deadline := time.Now().Add(3 * time.Second)
	settled := false
	for time.Now().Before(deadline) {
		_, mBody := doReq(t, http.MethodGet, ts.URL+"/-/metrics", nil, nil)
		if strings.Contains(string(mBody), "gostore_active_requests 2") {
			settled = true
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	if !settled {
		close(r1.release)
		close(r2.release)
		wg.Wait()
		t.Fatal("uploads never acquired both semaphore slots")
	}

	resp, body := doReq(t, http.MethodGet, ts.URL+"/photos?list-type=2", nil, nil)
	if resp.StatusCode != http.StatusServiceUnavailable ||
		resp.Header.Get("Retry-After") != "1" ||
		decodeErrorXML(t, body).Code != "SlowDown" {
		close(r1.release)
		close(r2.release)
		wg.Wait()
		t.Fatalf("expected 503 SlowDown with Retry-After, got status=%d headers=%v body=%s", resp.StatusCode, resp.Header, body)
	}

	// Health check bypasses data semaphore.
	hzResp, hzBody := doReq(t, http.MethodGet, ts.URL+"/-/healthz", nil, nil)
	if hzResp.StatusCode != http.StatusOK || string(hzBody) != "ok\n" {
		close(r1.release)
		close(r2.release)
		wg.Wait()
		t.Fatalf("healthz blocked under load: status=%d body=%q", hzResp.StatusCode, hzBody)
	}

	close(r1.release)
	close(r2.release)
	wg.Wait()

	// Normal requests succeed again once slots drain.
	resp, _ = doReq(t, http.MethodGet, ts.URL+"/photos?list-type=2", nil, nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("post-overload list status=%d", resp.StatusCode)
	}
}

func TestIdleTransferTimeoutAndCancellation(t *testing.T) {
	dir := t.TempDir()
	engine, _, err := storage.OpenWithOptions(dir, storage.Options{
		DisableSpaceCheck:   true,
		DisableExpiryWorker: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer engine.Close()

	ts := httptest.NewServer(New(slog.New(slog.NewTextHandler(io.Discard, nil)), engine, Options{
		IdleTransferTimeout:  80 * time.Millisecond,
		AllowUnauthenticated: true,
	}))
	defer ts.Close()

	doReq(t, http.MethodPut, ts.URL+"/photos", nil, nil)
	doReq(t, http.MethodPut, ts.URL+"/photos/keep.txt", strings.NewReader("safe"), nil)

	// Stalled upload body triggers idle-transfer timeout.
	pr, pw := io.Pipe()
	req, _ := http.NewRequest(http.MethodPut, ts.URL+"/photos/stalled.txt", pr)
	req.ContentLength = 100
	go func() {
		_, _ = pw.Write([]byte("abc"))
		// Keep pipe open without writing remaining 97 bytes.
	}()
	resp, err := http.DefaultClient.Do(req)
	_ = pw.Close()
	if err == nil {
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if resp.StatusCode != http.StatusRequestTimeout {
			t.Fatalf("expected 408 RequestTimeout, got status=%d body=%s", resp.StatusCode, body)
		}
	}

	// Client cancellation aborts cleanly.
	ctx, cancel := context.WithCancel(context.Background())
	pr2, pw2 := io.Pipe()
	req2, _ := http.NewRequestWithContext(ctx, http.MethodPut, ts.URL+"/photos/canceled.txt", pr2)
	req2.ContentLength = 100
	go func() {
		_, _ = pw2.Write([]byte("abc"))
		time.Sleep(15 * time.Millisecond)
		cancel()
		_ = pw2.Close()
	}()
	if _, err := http.DefaultClient.Do(req2); err == nil {
		t.Fatal("expected client error on canceled upload")
	}

	// Committed object remains intact.
	getResp, getBody := doReq(t, http.MethodGet, ts.URL+"/photos/keep.txt", nil, nil)
	if getResp.StatusCode != http.StatusOK || string(getBody) != "safe" {
		t.Fatalf("keep.txt damaged: status=%d body=%q", getResp.StatusCode, getBody)
	}
}
