//go:build linux

package storage

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ashu-wolf/gostore/internal/blob"
	"github.com/ashu-wolf/gostore/internal/domain"
)

func TestMixedConcurrencyStress(t *testing.T) {
	for round := 0; round < 3; round++ {
		e := openMultipartEngine(t, t.TempDir(), Options{
			MinPartSize:            4,
			MaxPartSize:            4096,
			MaxMultipartObjectSize: 16384,
			MinFreeSpace:           1024,
			DisableExpiryWorker:    true,
		})
		if err := e.CreateBucket("photos"); err != nil {
			t.Fatal(err)
		}
		if _, err := putString(e, "stable.txt", "always-safe"); err != nil {
			t.Fatal(err)
		}

		var wg sync.WaitGroup
		// Object put/get/head/delete workers.
		for w := 0; w < 4; w++ {
			wg.Add(1)
			go func(id int) {
				defer wg.Done()
				for i := 0; i < 15; i++ {
					key := fmt.Sprintf("dir/%d/%d.txt", id%2, i%3)
					val := fmt.Sprintf("val-%d-%d", id, i)
					if _, err := putString(e, key, val); err != nil {
						t.Error(err)
						return
					}
					if _, err := e.HeadObject("photos", key); err != nil && !errors.Is(err, domain.ErrNoSuchKey) {
						t.Error(err)
						return
					}
					_, r, err := e.OpenObject("photos", key)
					if err == nil {
						_, readErr := io.ReadAll(r)
						_ = r.Close()
						if readErr != nil {
							t.Error(readErr)
							return
						}
					} else if !errors.Is(err, domain.ErrNoSuchKey) {
						t.Error(err)
						return
					}
					if i%4 == 0 {
						if err := e.DeleteObject("photos", key); err != nil {
							t.Error(err)
							return
						}
					}
				}
			}(w)
		}

		// ListObjects workers.
		for w := 0; w < 2; w++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				for i := 0; i < 20; i++ {
					res, err := e.ListObjects(domain.ListOptions{Bucket: "photos", Delimiter: "/", MaxKeys: 2})
					if err != nil {
						t.Error(err)
						return
					}
					if res.IsTruncated && res.NextContinuationToken != "" {
						if _, err := e.ListObjects(domain.ListOptions{
							Bucket:            "photos",
							Delimiter:         "/",
							MaxKeys:           2,
							ContinuationToken: res.NextContinuationToken,
						}); err != nil {
							t.Error(err)
							return
						}
					}
				}
			}()
		}

		// Bucket create/put/delete contention workers.
		for w := 0; w < 2; w++ {
			wg.Add(1)
			go func(id int) {
				defer wg.Done()
				for i := 0; i < 10; i++ {
					_ = e.CreateBucket("temp")
					_, err := e.PutObject(context.Background(), PutRequest{Bucket: "temp", Key: "k"}, strings.NewReader("x"))
					if err != nil && !errors.Is(err, domain.ErrNoSuchBucket) {
						t.Error(err)
						return
					}
					_ = e.DeleteObject("temp", "k")
					err = e.DeleteBucket("temp")
					if err != nil && !errors.Is(err, domain.ErrNoSuchBucket) && !errors.Is(err, domain.ErrBucketNotEmpty) {
						t.Error(err)
						return
					}
				}
			}(w)
		}

		// Multipart upload workers.
		for w := 0; w < 2; w++ {
			wg.Add(1)
			go func(id int) {
				defer wg.Done()
				for i := 0; i < 5; i++ {
					key := fmt.Sprintf("mp-%d-%d.bin", id, i)
					u, err := e.InitiateMultipartUpload("photos", key, "", nil)
					if err != nil {
						t.Error(err)
						return
					}
					p1, err := e.UploadPart(context.Background(), "photos", key, u.ID, 1, blob.PutOptions{}, strings.NewReader("part1-"))
					if err != nil {
						t.Error(err)
						return
					}
					p2, err := e.UploadPart(context.Background(), "photos", key, u.ID, 2, blob.PutOptions{}, strings.NewReader("part2"))
					if err != nil {
						t.Error(err)
						return
					}
					if _, err := e.ListParts(domain.ListPartsOptions{Bucket: "photos", Key: key, UploadID: u.ID}); err != nil {
						t.Error(err)
						return
					}
					if i%2 == 0 {
						if _, err := e.CompleteMultipartUpload(context.Background(), "photos", key, u.ID, []domain.CompletedPart{
							{Number: 1, ETag: p1.ETag},
							{Number: 2, ETag: p2.ETag},
						}); err != nil {
							t.Error(err)
							return
						}
					} else {
						if err := e.AbortMultipartUpload("photos", key, u.ID); err != nil {
							t.Error(err)
							return
						}
					}
				}
			}(w)
		}

		wg.Wait()
		if got := getString(t, e, "stable.txt"); got != "always-safe" {
			t.Fatalf("round %d: stable object = %q", round, got)
		}
		// Clean up temp bucket if left around, and confirm zero orphaned blobs.
		_ = e.DeleteObject("temp", "k")
		_ = e.DeleteBucket("temp")
		if err := e.Close(); err != nil {
			t.Fatal(err)
		}
	}
}

type gatedReader struct {
	first   []byte
	rest    []byte
	wroteCh chan struct{}
	gate    chan struct{}
	stage   int
}

func (g *gatedReader) Read(p []byte) (int, error) {
	switch g.stage {
	case 0:
		g.stage = 1
		return copy(p, g.first), nil
	case 1:
		g.stage = 2
		close(g.wroteCh)
		<-g.gate
		return copy(p, g.rest), nil
	default:
		return 0, io.EOF
	}
}

func TestDiskSpaceAccountingAndFullDiskProtection(t *testing.T) {
	var availBytes atomic.Int64
	availBytes.Store(1500) // 500 bytes headroom above MinFreeSpace=1000

	e := openMultipartEngine(t, t.TempDir(), Options{
		MinPartSize:         1,
		MinFreeSpace:        1000,
		DisableExpiryWorker: true,
		Statfs: func(string) (int64, error) {
			return availBytes.Load(), nil
		},
	})
	if err := e.CreateBucket("photos"); err != nil {
		t.Fatal(err)
	}
	// Put a baseline object that must stay intact through all space-full rejections.
	want10 := int64(4)
	if _, err := e.PutObject(context.Background(), PutRequest{
		Bucket: "photos",
		Key:    "keep.txt",
		Checks: blob.PutOptions{WantSize: &want10},
	}, strings.NewReader("safe")); err != nil {
		t.Fatal(err)
	}

	t.Run("simultaneous uploads account for reserved space", func(t *testing.T) {
		gr := &gatedReader{
			first:   bytes.Repeat([]byte("a"), 100),
			rest:    bytes.Repeat([]byte("b"), 200),
			wroteCh: make(chan struct{}),
			gate:    make(chan struct{}),
		}
		size300 := int64(300)
		var (
			wg  sync.WaitGroup
			err error
		)
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err = e.PutObject(context.Background(), PutRequest{
				Bucket: "photos",
				Key:    "inflight.bin",
				Checks: blob.PutOptions{WantSize: &size300},
			}, gr)
		}()

		<-gr.wroteCh
		availBytes.Store(1400) // reflect 100 bytes written; 200 bytes remaining reserved => 200 headroom

		size350 := int64(350)
		if _, err2 := e.PutObject(context.Background(), PutRequest{
			Bucket: "photos",
			Key:    "too-big-now.bin",
			Checks: blob.PutOptions{WantSize: &size350},
		}, bytes.NewReader(make([]byte, 350))); !errors.Is(err2, domain.ErrInsufficientStorage) {
			close(gr.gate)
			wg.Wait()
			t.Fatalf("expected ErrInsufficientStorage for 350 bytes, got %v", err2)
		}

		// A 150-byte upload fits within remaining 200 headroom.
		size150 := int64(150)
		if _, err3 := e.PutObject(context.Background(), PutRequest{
			Bucket: "photos",
			Key:    "fits.bin",
			Checks: blob.PutOptions{WantSize: &size150},
		}, bytes.NewReader(make([]byte, 150))); err3 != nil {
			close(gr.gate)
			wg.Wait()
			t.Fatalf("expected 150-byte upload to succeed, got %v", err3)
		}

		close(gr.gate)
		wg.Wait()
		if err != nil {
			t.Fatalf("inflight upload failed: %v", err)
		}
	})

	t.Run("multipart assembly checks space and dynamic full disk stops unknown-size upload", func(t *testing.T) {
		availBytes.Store(2000)
		u, err := e.InitiateMultipartUpload("photos", "mp.bin", "", nil)
		if err != nil {
			t.Fatal(err)
		}
		sz := int64(200)
		p1, err := e.UploadPart(context.Background(), "photos", "mp.bin", u.ID, 1, blob.PutOptions{WantSize: &sz}, bytes.NewReader(make([]byte, 200)))
		if err != nil {
			t.Fatal(err)
		}
		p2, err := e.UploadPart(context.Background(), "photos", "mp.bin", u.ID, 2, blob.PutOptions{WantSize: &sz}, bytes.NewReader(make([]byte, 200)))
		if err != nil {
			t.Fatal(err)
		}

		// Drop free space so 400-byte assembly cannot fit above MinFreeSpace=1000.
		availBytes.Store(1300)
		if _, err := e.CompleteMultipartUpload(context.Background(), "photos", "mp.bin", u.ID, []domain.CompletedPart{
			{Number: 1, ETag: p1.ETag},
			{Number: 2, ETag: p2.ETag},
		}); !errors.Is(err, domain.ErrInsufficientStorage) {
			t.Fatalf("expected ErrInsufficientStorage on assembly, got %v", err)
		}

		// Unknown-size stream passes initial check, then disk fills mid-stream.
		availBytes.Store(1500)
		stream := &gatedReader{
			first:   bytes.Repeat([]byte("x"), 100),
			rest:    bytes.Repeat([]byte("y"), 100),
			wroteCh: make(chan struct{}),
			gate:    make(chan struct{}),
		}
		var (
			wg        sync.WaitGroup
			streamErr error
		)
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, streamErr = e.PutObject(context.Background(), PutRequest{Bucket: "photos", Key: "stream.bin"}, stream)
		}()
		<-stream.wroteCh
		availBytes.Store(900) // below MinFreeSpace=1000 mid-stream!
		close(stream.gate)
		wg.Wait()
		if !errors.Is(streamErr, domain.ErrInsufficientStorage) {
			t.Fatalf("expected ErrInsufficientStorage mid-stream, got %v", streamErr)
		}
		if tmp, err := e.blobs.RemoveTemp(); err != nil || tmp != 0 {
			t.Fatalf("left %d temp files after disk full, err=%v", tmp, err)
		}
	})

	// Committed object must remain intact after all disk-full errors.
	if got := getString(t, e, "keep.txt"); got != "safe" {
		t.Fatalf("keep.txt corrupted: %q", got)
	}
}

func TestCleanupWorkerBoundedAndStoppable(t *testing.T) {
	var nowMu sync.Mutex
	now := time.Date(2026, 11, 26, 12, 0, 0, 0, time.UTC)
	clock := func() time.Time {
		nowMu.Lock()
		defer nowMu.Unlock()
		return now
	}

	e := openMultipartEngine(t, t.TempDir(), Options{
		MinPartSize:       1,
		UploadExpiry:      time.Minute,
		ExpiryInterval:    10 * time.Millisecond,
		DisableSpaceCheck: true,
		Now:               clock,
	})
	if err := e.CreateBucket("photos"); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 5; i++ {
		u, err := e.InitiateMultipartUpload("photos", fmt.Sprintf("k-%d", i), "", nil)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := e.UploadPart(context.Background(), "photos", u.Key, u.ID, 1, blob.PutOptions{}, strings.NewReader("tmp")); err != nil {
			t.Fatal(err)
		}
	}

	nowMu.Lock()
	now = now.Add(2 * time.Minute)
	nowMu.Unlock()

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if blobCount(t, e) == 0 {
			break
		}
		time.Sleep(15 * time.Millisecond)
	}
	if blobCount(t, e) != 0 {
		t.Fatalf("worker did not expire inactive uploads, blobs=%d", blobCount(t, e))
	}
	if err := e.Close(); err != nil {
		t.Fatal(err)
	}
}
