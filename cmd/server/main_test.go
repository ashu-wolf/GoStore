//go:build linux

package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"log/slog"
	"net"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	v4 "github.com/aws/aws-sdk-go-v2/aws/signer/v4"

	"github.com/ashu-wolf/gostore/internal/config"
	"github.com/ashu-wolf/gostore/internal/storage"
)

func freeLocalAddr(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	_ = ln.Close()
	return addr
}

func waitReady(t *testing.T, addr string) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		resp, err := http.Get("http://" + addr + "/-/readyz")
		if err == nil {
			resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				return
			}
		}
		time.Sleep(15 * time.Millisecond)
	}
	t.Fatalf("server at %s never became ready", addr)
}

func waitActiveRequest(t *testing.T, addr string) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		resp, err := http.Get("http://" + addr + "/-/metrics")
		if err == nil {
			body, _ := io.ReadAll(resp.Body)
			resp.Body.Close()
			if strings.Contains(string(body), "gostore_active_requests 1") {
				return
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("server at %s never observed active request", addr)
}

func signReq(t *testing.T, req *http.Request, payloadHash, access, secret string) {
	t.Helper()
	req.Header.Set("X-Amz-Content-Sha256", payloadHash)
	signer := v4.NewSigner()
	creds := aws.Credentials{AccessKeyID: access, SecretAccessKey: secret}
	if err := signer.SignHTTP(context.Background(), creds, req, payloadHash, "s3", "us-east-1", time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
}

func TestRunServerGracefulDrainAndPprof(t *testing.T) {
	addr := freeLocalAddr(t)
	pprofAddr := freeLocalAddr(t)
	dataDir := t.TempDir()

	cfg := config.Config{
		Addr:                  addr,
		PprofAddr:             pprofAddr,
		DataDir:               dataDir,
		ShutdownTimeout:       5 * time.Second,
		IdleTransferTimeout:   5 * time.Second,
		MaxConcurrentRequests: 16,
		MaxObjectSize:         1 << 20,
		MaxMultipartSize:      2 << 20,
		MinPartSize:           1024,
		MaxUploadParts:        100,
		UploadExpiry:          time.Hour,
		MinFreeSpace:          0,
		ListPageSize:          100,
		AccessKey:             "test-ak",
		SecretKey:             "test-sk",
		Region:                "us-east-1",
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	errCh := make(chan error, 1)
	go func() {
		errCh <- runServer(ctx, cfg, slog.New(slog.NewTextHandler(io.Discard, nil)))
	}()

	waitReady(t, addr)

	// Verify pprof responds on localhost port.
	pResp, err := http.Get("http://" + pprofAddr + "/debug/pprof/")
	if err != nil {
		t.Fatal(err)
	}
	pResp.Body.Close()
	if pResp.StatusCode != http.StatusOK {
		t.Fatalf("pprof status=%d", pResp.StatusCode)
	}

	// Create bucket.
	emptyHash := "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855"
	bReq, _ := http.NewRequest(http.MethodPut, "http://"+addr+"/photos", nil)
	signReq(t, bReq, emptyHash, cfg.AccessKey, cfg.SecretKey)
	bResp, err := http.DefaultClient.Do(bReq)
	if err != nil || bResp.StatusCode != http.StatusOK {
		t.Fatalf("create bucket: %v, resp=%v", err, bResp)
	}
	bResp.Body.Close()

	// Start an in-flight upload, trigger shutdown, then complete the upload before deadline.
	pr, pw := io.Pipe()
	payload := []byte("drained-ok")
	sum := sha256.Sum256(payload)
	uReq, _ := http.NewRequest(http.MethodPut, "http://"+addr+"/photos/drained.txt", pr)
	uReq.ContentLength = int64(len(payload))
	signReq(t, uReq, hex.EncodeToString(sum[:]), cfg.AccessKey, cfg.SecretKey)

	type uploadRes struct {
		code int
		err  error
	}
	uResCh := make(chan uploadRes, 1)
	go func() {
		resp, err := http.DefaultClient.Do(uReq)
		if err != nil {
			uResCh <- uploadRes{err: err}
			return
		}
		resp.Body.Close()
		uResCh <- uploadRes{code: resp.StatusCode}
	}()

	// Write first half and wait until the server is actively inside PutObject.
	if _, err := pw.Write(payload[:4]); err != nil {
		t.Fatal(err)
	}
	waitActiveRequest(t, addr)
	cancel()

	// Complete the in-flight upload; it must drain and commit cleanly.
	if _, err := pw.Write(payload[4:]); err != nil {
		t.Fatal(err)
	}
	_ = pw.Close()

	res := <-uResCh
	if res.err != nil || res.code != http.StatusOK {
		t.Fatalf("drained upload failed: status=%d err=%v", res.code, res.err)
	}
	if err := <-errCh; err != nil {
		t.Fatalf("runServer returned error: %v", err)
	}

	// Verify lock was released and data survived shutdown.
	e, _, err := storage.Open(dataDir)
	if err != nil {
		t.Fatalf("reopen after shutdown: %v", err)
	}
	defer e.Close()
	if _, err := e.HeadObject("photos", "drained.txt"); err != nil {
		t.Fatalf("drained object missing: %v", err)
	}
}

func TestRunServerDeadlineCancelsStalledRequests(t *testing.T) {
	addr := freeLocalAddr(t)
	dataDir := t.TempDir()

	cfg := config.Config{
		Addr:                  addr,
		DataDir:               dataDir,
		ShutdownTimeout:       120 * time.Millisecond,
		IdleTransferTimeout:   5 * time.Second,
		MaxConcurrentRequests: 16,
		MaxObjectSize:         1 << 20,
		MaxMultipartSize:      2 << 20,
		MinPartSize:           1024,
		MaxUploadParts:        100,
		UploadExpiry:          time.Hour,
		MinFreeSpace:          0,
		ListPageSize:          100,
		AccessKey:             "test-ak",
		SecretKey:             "test-sk",
		Region:                "us-east-1",
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	errCh := make(chan error, 1)
	go func() {
		errCh <- runServer(ctx, cfg, slog.New(slog.NewTextHandler(io.Discard, nil)))
	}()
	waitReady(t, addr)

	emptyHash := "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855"
	bReq, _ := http.NewRequest(http.MethodPut, "http://"+addr+"/photos", nil)
	signReq(t, bReq, emptyHash, cfg.AccessKey, cfg.SecretKey)
	bResp, err := http.DefaultClient.Do(bReq)
	if err != nil {
		t.Fatal(err)
	}
	bResp.Body.Close()

	// Start a stalled upload that never completes.
	pr, pw := io.Pipe()
	defer pw.Close()
	uReq, _ := http.NewRequest(http.MethodPut, "http://"+addr+"/photos/stalled.txt", pr)
	uReq.ContentLength = 100
	signReq(t, uReq, "0000000000000000000000000000000000000000000000000000000000000000", cfg.AccessKey, cfg.SecretKey)

	clientDone := make(chan struct{})
	go func() {
		defer close(clientDone)
		resp, err := http.DefaultClient.Do(uReq)
		if err == nil {
			resp.Body.Close()
		}
	}()
	_, _ = pw.Write([]byte("abc"))
	waitActiveRequest(t, addr)

	start := time.Now()
	cancel()

	select {
	case err := <-errCh:
		if err != nil {
			t.Fatalf("runServer returned error: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("runServer did not exit within shutdown timeout + margin")
	}
	if elapsed := time.Since(start); elapsed > 1500*time.Millisecond {
		t.Fatalf("shutdown took %v, expected close to %v", elapsed, cfg.ShutdownTimeout)
	}

	_ = pw.Close()
	<-clientDone

	// Storage lock must be released cleanly.
	e, _, err := storage.Open(dataDir)
	if err != nil {
		t.Fatalf("reopen after forced shutdown: %v", err)
	}
	_ = e.Close()
}
