//go:build linux

package meta

import (
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/ashu-wolf/gostore/internal/domain"
)

func TestMultipartStateSurvivesReopenAndCompaction(t *testing.T) {
	path := filepath.Join(t.TempDir(), "meta.journal")
	s, _ := open(t, path)
	must(t, s.CreateBucket(domain.Bucket{Name: "photos"}))

	t0 := time.Date(2026, 11, 26, 10, 0, 0, 0, time.UTC)
	must(t, s.CreateUpload(domain.Upload{
		ID:           "u1",
		Bucket:       "photos",
		Key:          "big.bin",
		ContentType:  "application/octet-stream",
		Metadata:     map[string]string{"owner": "ashu"},
		CreatedAt:    t0,
		LastActiveAt: t0,
	}))
	must(t, s.CreateUpload(domain.Upload{
		ID:           "u2",
		Bucket:       "photos",
		Key:          "done.bin",
		CreatedAt:    t0,
		LastActiveAt: t0,
	}))

	if old, err := s.PutPart("photos", "big.bin", domain.Part{
		UploadID: "u1", Number: 2, BlobID: "b2", Size: 100, ETag: "e2", ModifiedAt: t0.Add(2 * time.Minute),
	}); err != nil || old != "" {
		t.Fatalf("old=%q err=%v", old, err)
	}
	if old, err := s.PutPart("photos", "big.bin", domain.Part{
		UploadID: "u1", Number: 1, BlobID: "b1a", Size: 100, ETag: "e1a", ModifiedAt: t0.Add(time.Minute),
	}); err != nil || old != "" {
		t.Fatalf("old=%q err=%v", old, err)
	}
	// Replace part 1.
	if old, err := s.PutPart("photos", "big.bin", domain.Part{
		UploadID: "u1", Number: 1, BlobID: "b1b", Size: 105, ETag: "e1b", ModifiedAt: t0.Add(3 * time.Minute),
	}); err != nil || old != "b1a" {
		t.Fatalf("replace old=%q err=%v", old, err)
	}

	// Complete u2.
	if _, err := s.PutPart("photos", "done.bin", domain.Part{
		UploadID: "u2", Number: 1, BlobID: "bd1", Size: 50, ETag: "ed1", ModifiedAt: t0,
	}); err != nil {
		t.Fatal(err)
	}
	donePart := []domain.Part{{UploadID: "u2", Number: 1, BlobID: "bd1", Size: 50, ETag: "ed1"}}
	already, comp, _, partBlobs, err := s.CompleteUpload(
		domain.Object{Bucket: "photos", Key: "done.bin", BlobID: "bfinal", Size: 50, ETag: "ed1-1"},
		"u2", donePart,
		domain.CompletedUpload{UploadID: "u2", Bucket: "photos", Key: "done.bin", ETag: "ed1-1", CompletedAt: t0.Add(5 * time.Minute)},
		t0.Add(5*time.Minute), 24*time.Hour,
	)
	if err != nil || already || comp.ETag != "ed1-1" || len(partBlobs) != 1 || partBlobs[0] != "bd1" {
		t.Fatalf("complete failed: already=%v comp=%+v blobs=%v err=%v", already, comp, partBlobs, err)
	}
	must(t, s.Close())

	// Reopen and verify upload u1, parts, and u2 completion survive.
	s, _ = open(t, path)
	res, err := s.ListParts(domain.ListPartsOptions{Bucket: "photos", Key: "big.bin", UploadID: "u1", MaxParts: 1})
	if err != nil || !res.IsTruncated || len(res.Parts) != 1 || res.Parts[0].BlobID != "b1b" || res.NextPartNumberMarker != 1 {
		t.Fatalf("list parts page 1: %+v, err=%v", res, err)
	}
	res2, err := s.ListParts(domain.ListPartsOptions{Bucket: "photos", Key: "big.bin", UploadID: "u1", PartNumberMarker: res.NextPartNumberMarker, MaxParts: 10})
	if err != nil || res2.IsTruncated || len(res2.Parts) != 1 || res2.Parts[0].BlobID != "b2" {
		t.Fatalf("list parts page 2: %+v, err=%v", res2, err)
	}
	if c, ok := s.Completion("photos", "done.bin", "u2", t0.Add(10*time.Hour), 24*time.Hour); !ok || c.ETag != "ed1-1" {
		t.Fatalf("completion missing after reopen: %+v, %v", c, ok)
	}
	// LastActiveAt must reflect the newest part (t0+3m), not out-of-order t0+2m.
	if res.Upload.LastActiveAt != t0.Add(3*time.Minute) {
		t.Fatalf("LastActiveAt=%v, want %v", res.Upload.LastActiveAt, t0.Add(3*time.Minute))
	}

	// Expire u1 only when inactive past cutoff and not active.
	blobs, count, err := s.ExpireUploads(t0.Add(4*time.Minute), t0, func(id string) bool { return id == "u1" })
	if err != nil || count != 0 || len(blobs) != 0 {
		t.Fatalf("active upload was expired: count=%d blobs=%v err=%v", count, blobs, err)
	}
	blobs, count, err = s.ExpireUploads(t0.Add(4*time.Minute), t0.Add(6*time.Minute), nil)
	if err != nil || count != 1 || len(blobs) != 2 {
		t.Fatalf("expected u1 expiry: count=%d blobs=%v err=%v", count, blobs, err)
	}
	if err := s.HasUpload("photos", "big.bin", "u1"); !errors.Is(err, domain.ErrNoSuchUpload) {
		t.Fatalf("got %v", err)
	}
	if _, ok := s.Completion("photos", "done.bin", "u2", t0.Add(10*time.Hour), 24*time.Hour); ok {
		t.Fatal("expected completion to be purged")
	}
}
