//go:build linux

package storage

import (
	"bytes"
	"context"
	"crypto/md5"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ashu-wolf/gostore/internal/blob"
	"github.com/ashu-wolf/gostore/internal/domain"
)

func openMultipartEngine(t *testing.T, root string, opts Options) *Engine {
	t.Helper()
	e, _, err := OpenWithOptions(root, opts)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = e.Close() })
	return e
}

func TestMultipartLifecycleReplacementAndETag(t *testing.T) {
	e := openMultipartEngine(t, t.TempDir(), Options{
		MinPartSize:            4,
		MaxPartSize:            64,
		MaxMultipartObjectSize: 200,
		DisableExpiryWorker:    true,
	})
	if err := e.CreateBucket("photos"); err != nil {
		t.Fatal(err)
	}

	u, err := e.InitiateMultipartUpload("photos", "movie.bin", "video/mp4", map[string]string{"tag": "v1"})
	if err != nil {
		t.Fatal(err)
	}

	// Upload part 1, then replace part 1.
	p1Old, err := e.UploadPart(context.Background(), "photos", "movie.bin", u.ID, 1, blob.PutOptions{}, strings.NewReader("old-p1"))
	if err != nil {
		t.Fatal(err)
	}
	p1Data := "part-one-"
	p1, err := e.UploadPart(context.Background(), "photos", "movie.bin", u.ID, 1, blob.PutOptions{}, strings.NewReader(p1Data))
	if err != nil {
		t.Fatal(err)
	}
	if p1.BlobID == p1Old.BlobID {
		t.Fatal("expected new blob for replaced part")
	}
	// Replaced part blob must be deleted immediately.
	if _, err := e.blobs.Size(p1Old.BlobID); !errors.Is(err, blob.ErrNotFound) {
		t.Fatalf("old part blob still exists: %v", err)
	}

	p2Data := "end" // last part may be smaller than MinPartSize (4)
	p2, err := e.UploadPart(context.Background(), "photos", "movie.bin", u.ID, 2, blob.PutOptions{}, strings.NewReader(p2Data))
	if err != nil {
		t.Fatal(err)
	}

	// List parts.
	lp, err := e.ListParts(domain.ListPartsOptions{Bucket: "photos", Key: "movie.bin", UploadID: u.ID})
	if err != nil || len(lp.Parts) != 2 || lp.Parts[0].ETag != p1.ETag || lp.Parts[1].ETag != p2.ETag {
		t.Fatalf("ListParts: %+v, err=%v", lp, err)
	}

	// Complete and verify multipart ETag = MD5(bin(p1.MD5) || bin(p2.MD5)) + "-2".
	comp, err := e.CompleteMultipartUpload(context.Background(), "photos", "movie.bin", u.ID, []domain.CompletedPart{
		{Number: 1, ETag: `"` + p1.ETag + `"`},
		{Number: 2, ETag: p2.ETag},
	})
	if err != nil {
		t.Fatal(err)
	}
	d1, _ := hex.DecodeString(p1.ETag)
	d2, _ := hex.DecodeString(p2.ETag)
	h := md5.New()
	_, _ = h.Write(d1)
	_, _ = h.Write(d2)
	wantETag := fmt.Sprintf("%s-2", hex.EncodeToString(h.Sum(nil)))
	if comp.ETag != wantETag {
		t.Fatalf("ETag = %q, want %q", comp.ETag, wantETag)
	}

	// Repeated completion returns the exact same result.
	comp2, err := e.CompleteMultipartUpload(context.Background(), "photos", "movie.bin", u.ID, []domain.CompletedPart{
		{Number: 1, ETag: p1.ETag},
		{Number: 2, ETag: p2.ETag},
	})
	if err != nil || comp2 != comp {
		t.Fatalf("repeated completion got %+v, err=%v", comp2, err)
	}

	// Part blobs should be deleted; only the assembled object blob remains.
	if blobCount(t, e) != 1 {
		t.Fatalf("expected 1 blob, got %d", blobCount(t, e))
	}

	obj, r, err := e.OpenObject("photos", "movie.bin")
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	body, _ := io.ReadAll(r)
	if string(body) != p1Data+p2Data || obj.ContentType != "video/mp4" || obj.Metadata["tag"] != "v1" || obj.ETag != wantETag {
		t.Fatalf("got obj=%+v body=%q", obj, body)
	}
}

func TestMultipartValidationErrors(t *testing.T) {
	e := openMultipartEngine(t, t.TempDir(), Options{
		MinPartSize:            5,
		MaxPartSize:            10,
		MaxMultipartObjectSize: 12,
		MaxUploadParts:         3,
		DisableExpiryWorker:    true,
	})
	if err := e.CreateBucket("photos"); err != nil {
		t.Fatal(err)
	}
	u, err := e.InitiateMultipartUpload("photos", "k", "", nil)
	if err != nil {
		t.Fatal(err)
	}

	if _, err := e.UploadPart(context.Background(), "photos", "k", u.ID, 4, blob.PutOptions{}, strings.NewReader("a")); !errors.Is(err, domain.ErrInvalidPart) {
		t.Fatalf("part > MaxUploadParts: got %v", err)
	}
	if _, err := e.UploadPart(context.Background(), "photos", "k", u.ID, 1, blob.PutOptions{}, strings.NewReader(strings.Repeat("x", 11))); !errors.Is(err, blob.ErrTooLarge) {
		t.Fatalf("part > MaxPartSize: got %v", err)
	}

	p1, err := e.UploadPart(context.Background(), "photos", "k", u.ID, 1, blob.PutOptions{}, strings.NewReader("1234"))
	if err != nil {
		t.Fatal(err)
	}
	p2, err := e.UploadPart(context.Background(), "photos", "k", u.ID, 2, blob.PutOptions{}, strings.NewReader("567890123"))
	if err != nil {
		t.Fatal(err)
	}

	// Out of order.
	if _, err := e.CompleteMultipartUpload(context.Background(), "photos", "k", u.ID, []domain.CompletedPart{
		{Number: 2, ETag: p2.ETag},
		{Number: 1, ETag: p1.ETag},
	}); !errors.Is(err, domain.ErrInvalidOrder) {
		t.Fatalf("order: got %v", err)
	}
	// Wrong ETag.
	if _, err := e.CompleteMultipartUpload(context.Background(), "photos", "k", u.ID, []domain.CompletedPart{
		{Number: 1, ETag: "0000"},
	}); !errors.Is(err, domain.ErrInvalidPart) {
		t.Fatalf("etag: got %v", err)
	}
	// Non-last part smaller than MinPartSize (5).
	if _, err := e.CompleteMultipartUpload(context.Background(), "photos", "k", u.ID, []domain.CompletedPart{
		{Number: 1, ETag: p1.ETag},
		{Number: 2, ETag: p2.ETag},
	}); !errors.Is(err, domain.ErrEntityTooSmall) {
		t.Fatalf("too small: got %v", err)
	}
	// Replace part 1 with 5 bytes so total 5+9=14 > MaxMultipartObjectSize (12).
	p1Big, err := e.UploadPart(context.Background(), "photos", "k", u.ID, 1, blob.PutOptions{}, strings.NewReader("12345"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.CompleteMultipartUpload(context.Background(), "photos", "k", u.ID, []domain.CompletedPart{
		{Number: 1, ETag: p1Big.ETag},
		{Number: 2, ETag: p2.ETag},
	}); !errors.Is(err, domain.ErrEntityTooLarge) {
		t.Fatalf("too large: got %v", err)
	}
}

func TestIncompleteUploadSurvivesRestartUntilExpiry(t *testing.T) {
	root := t.TempDir()
	now := time.Date(2026, 11, 26, 12, 0, 0, 0, time.UTC)
	var mu sync.Mutex
	clock := func() time.Time {
		mu.Lock()
		defer mu.Unlock()
		return now
	}
	setClock := func(t time.Time) {
		mu.Lock()
		now = t
		mu.Unlock()
	}

	e := openMultipartEngine(t, root, Options{
		MinPartSize:         1,
		UploadExpiry:        time.Hour,
		CompletionRetention: time.Hour,
		DisableExpiryWorker: true,
		Now:                 clock,
	})
	if err := e.CreateBucket("photos"); err != nil {
		t.Fatal(err)
	}
	u, err := e.InitiateMultipartUpload("photos", "resume.bin", "text/plain", nil)
	if err != nil {
		t.Fatal(err)
	}
	p1, err := e.UploadPart(context.Background(), "photos", "resume.bin", u.ID, 1, blob.PutOptions{}, strings.NewReader("part1-"))
	if err != nil {
		t.Fatal(err)
	}
	if err := e.Close(); err != nil {
		t.Fatal(err)
	}

	// Restart before expiry: startup recovery must keep incomplete upload part blobs.
	e, report, err := OpenWithOptions(root, Options{
		MinPartSize:         1,
		UploadExpiry:        time.Hour,
		CompletionRetention: time.Hour,
		DisableExpiryWorker: true,
		Now:                 clock,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer e.Close()
	if report.OrphansRemoved != 0 || blobCount(t, e) != 1 {
		t.Fatalf("incomplete upload blob lost on restart: report=%+v blobs=%d", report, blobCount(t, e))
	}

	// Upload second part after restart and complete.
	p2, err := e.UploadPart(context.Background(), "photos", "resume.bin", u.ID, 2, blob.PutOptions{}, strings.NewReader("part2"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.CompleteMultipartUpload(context.Background(), "photos", "resume.bin", u.ID, []domain.CompletedPart{
		{Number: 1, ETag: p1.ETag},
		{Number: 2, ETag: p2.ETag},
	}); err != nil {
		t.Fatal(err)
	}
	if got := getString(t, e, "resume.bin"); got != "part1-part2" {
		t.Fatalf("got %q", got)
	}

	// Now start another upload and verify active protection + expiry after UploadExpiry.
	u2, err := e.InitiateMultipartUpload("photos", "expire.bin", "", nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.UploadPart(context.Background(), "photos", "expire.bin", u2.ID, 1, blob.PutOptions{}, strings.NewReader("temp")); err != nil {
		t.Fatal(err)
	}

	setClock(now.Add(2 * time.Hour))
	e.pinUpload(u2.ID)
	n, err := e.ExpireUploads(clock())
	if err != nil || n != 0 {
		t.Fatalf("active upload expired: n=%d err=%v", n, err)
	}
	e.unpinUpload(u2.ID)

	n, err = e.ExpireUploads(clock())
	if err != nil || n != 1 {
		t.Fatalf("expected 1 expired upload, got n=%d err=%v", n, err)
	}
	if blobCount(t, e) != 1 { // only resume.bin remains
		t.Fatalf("blobs=%d, want 1", blobCount(t, e))
	}
	// Completion record for u must also be expired now.
	if _, err := e.CompleteMultipartUpload(context.Background(), "photos", "resume.bin", u.ID, []domain.CompletedPart{{Number: 1, ETag: p1.ETag}}); !errors.Is(err, domain.ErrNoSuchUpload) {
		t.Fatalf("expected expired completion to return ErrNoSuchUpload, got %v", err)
	}
}

func TestAbortRacingComplete(t *testing.T) {
	e := openMultipartEngine(t, t.TempDir(), Options{
		MinPartSize:         1,
		DisableExpiryWorker: true,
	})
	if err := e.CreateBucket("photos"); err != nil {
		t.Fatal(err)
	}

	partPayload := bytes.Repeat([]byte("z"), 64*1024)
	for iter := 0; iter < 20; iter++ {
		key := fmt.Sprintf("race-%d.bin", iter)
		u, err := e.InitiateMultipartUpload("photos", key, "", nil)
		if err != nil {
			t.Fatal(err)
		}
		var submitted []domain.CompletedPart
		for num := 1; num <= 4; num++ {
			p, err := e.UploadPart(context.Background(), "photos", key, u.ID, num, blob.PutOptions{}, bytes.NewReader(partPayload))
			if err != nil {
				t.Fatal(err)
			}
			submitted = append(submitted, domain.CompletedPart{Number: num, ETag: p.ETag})
		}

		var (
			wg          sync.WaitGroup
			completeErr error
			abortErr    error
		)
		wg.Add(2)
		go func() {
			defer wg.Done()
			_, completeErr = e.CompleteMultipartUpload(context.Background(), "photos", key, u.ID, submitted)
		}()
		go func() {
			defer wg.Done()
			abortErr = e.AbortMultipartUpload("photos", key, u.ID)
		}()
		wg.Wait()

		switch {
		case completeErr == nil && errors.Is(abortErr, domain.ErrNoSuchUpload):
			// Complete won: object must be readable and clean.
			obj, r, err := e.OpenObject("photos", key)
			if err != nil {
				t.Fatalf("iter %d: OpenObject failed: %v", iter, err)
			}
			data, err := io.ReadAll(r)
			_ = r.Close()
			if err != nil || obj.Size != int64(4*len(partPayload)) || len(data) != 4*len(partPayload) {
				t.Fatalf("iter %d: corrupt object size=%d len=%d err=%v", iter, obj.Size, len(data), err)
			}
			if err := e.DeleteObject("photos", key); err != nil {
				t.Fatal(err)
			}
		case abortErr == nil && errors.Is(completeErr, domain.ErrNoSuchUpload):
			// Abort won: object must not exist.
			if _, err := e.HeadObject("photos", key); !errors.Is(err, domain.ErrNoSuchKey) {
				t.Fatalf("iter %d: object exists after abort won: %v", iter, err)
			}
		default:
			t.Fatalf("iter %d: unexpected race outcome completeErr=%v abortErr=%v", iter, completeErr, abortErr)
		}

		if n := blobCount(t, e); n != 0 {
			t.Fatalf("iter %d: leaked %d blobs", iter, n)
		}
	}
}
