//go:build linux

// Package storage combines blob data and metadata into crash-safe operations.
package storage

import (
	"context"
	"errors"
	"fmt"
	"io"
	"path/filepath"
	"time"

	"github.com/ashu-wolf/gostore/internal/blob"
	"github.com/ashu-wolf/gostore/internal/crashpoint"
	"github.com/ashu-wolf/gostore/internal/domain"
	"github.com/ashu-wolf/gostore/internal/meta"
)

// ErrDamaged means an object's data is missing or has the wrong size.
var ErrDamaged = errors.New("object data is damaged")

// Engine is safe for concurrent use. Close it only after all calls return.
type Engine struct {
	blobs *blob.Store
	meta  *meta.Store
}

// Report describes what startup recovery found.
type Report struct {
	TempRemoved    int
	OrphansRemoved int
	DiscardedBytes int
	Compacted      bool
	Damaged        []string // "bucket/key" of objects with bad data
}

// Open loads the data directory and runs recovery. It must finish before the
// server accepts requests.
func Open(root string) (_ *Engine, report Report, err error) {
	blobs, err := blob.Open(root) // also locks the directory
	if err != nil {
		return nil, report, err
	}
	var m *meta.Store
	defer func() {
		if err != nil {
			if m != nil {
				_ = m.Close()
			}
			_ = blobs.Close()
		}
	}()

	ids, err := blobs.IDs()
	if err != nil {
		return nil, report, fmt.Errorf("list blobs: %w", err)
	}
	// A new journal is allowed only for an empty store. Otherwise a lost
	// journal would make every blob look unused, and cleanup would delete it.
	m, opened, err := meta.Open(filepath.Join(root, "meta.journal"), len(ids) == 0)
	if err != nil {
		return nil, report, fmt.Errorf("open metadata: %w", err)
	}
	report.DiscardedBytes, report.Compacted = opened.DiscardedBytes, opened.Compacted

	if report.TempRemoved, err = blobs.RemoveTemp(); err != nil {
		return nil, report, fmt.Errorf("remove temporary files: %w", err)
	}

	used := map[string]bool{}
	for _, o := range m.AllObjects() {
		used[o.BlobID] = true
		size, sizeErr := blobs.Size(o.BlobID)
		switch {
		case errors.Is(sizeErr, blob.ErrNotFound), errors.Is(sizeErr, blob.ErrInvalidID),
			sizeErr == nil && size != o.Size:
			report.Damaged = append(report.Damaged, o.Bucket+"/"+o.Key)
		case sizeErr != nil:
			return nil, report, fmt.Errorf("check blob: %w", sizeErr)
		}
	}
	for _, id := range ids {
		if !used[id] {
			if err := blobs.Delete(id); err != nil {
				return nil, report, fmt.Errorf("remove unused blob: %w", err)
			}
			report.OrphansRemoved++
		}
	}
	return &Engine{blobs: blobs, meta: m}, report, nil
}

// Close closes the metadata journal and releases the directory lock.
func (e *Engine) Close() error {
	return errors.Join(e.meta.Close(), e.blobs.Close())
}

// CreateBucket validates the name and creates a bucket.
func (e *Engine) CreateBucket(name string) error {
	if err := domain.ValidateBucketName(name); err != nil {
		return err
	}
	return e.meta.CreateBucket(domain.Bucket{Name: name, CreatedAt: time.Now().UTC()})
}

// DeleteBucket removes an empty bucket.
func (e *Engine) DeleteBucket(name string) error {
	return e.meta.DeleteBucket(name)
}

// PutRequest describes an object upload.
type PutRequest struct {
	Bucket      string
	Key         string
	ContentType string
	Metadata    map[string]string
	Checks      blob.PutOptions
}

// PutObject stores an object. The new version becomes visible only when the
// metadata commit succeeds.
func (e *Engine) PutObject(ctx context.Context, req PutRequest, body io.Reader) (domain.Object, error) {
	if err := domain.ValidateKey(req.Key); err != nil {
		return domain.Object{}, err
	}
	if err := domain.ValidateMetadata(req.Metadata); err != nil {
		return domain.Object{}, err
	}
	// Fail early. The commit checks again, in case the bucket is deleted while
	// the data is streaming.
	if !e.meta.HasBucket(req.Bucket) {
		return domain.Object{}, domain.ErrNoSuchBucket
	}

	info, err := e.blobs.Put(ctx, body, req.Checks)
	if err != nil {
		return domain.Object{}, err
	}
	obj := domain.Object{
		Bucket:      req.Bucket,
		Key:         req.Key,
		BlobID:      info.ID,
		Size:        info.Size,
		ETag:        info.MD5,
		SHA256:      info.SHA256,
		ContentType: req.ContentType,
		Metadata:    req.Metadata,
		ModifiedAt:  time.Now().UTC(),
	}
	crashpoint.Hit("storage.before-commit")

	old, err := e.meta.PutObject(obj)
	if err != nil {
		// If the result is unknown, the record may be on disk, so the blob
		// must be kept. Recovery removes it if it is unused.
		if !errors.Is(err, meta.ErrCommitUnknown) {
			_ = e.blobs.Delete(info.ID)
		}
		return domain.Object{}, err
	}
	crashpoint.Hit("storage.after-commit")

	if old != "" {
		_ = e.blobs.Delete(old) // recovery retries if this fails
	}
	return obj, nil
}

// OpenObject returns an object's record and data. The caller must close the
// reader. The data is not checked against its SHA-256 here.
func (e *Engine) OpenObject(bucket, key string) (domain.Object, io.ReadSeekCloser, error) {
	tried := ""
	for {
		obj, err := e.meta.Object(bucket, key)
		if err != nil {
			return domain.Object{}, nil, err
		}
		if obj.BlobID == tried {
			return domain.Object{}, nil, ErrDamaged
		}
		r, err := e.blobs.Open(obj.BlobID)
		if errors.Is(err, blob.ErrNotFound) {
			// The object may have been replaced after the lookup. Look again.
			tried = obj.BlobID
			continue
		}
		if err != nil {
			return domain.Object{}, nil, err
		}
		if err := checkSize(r, obj.Size); err != nil {
			_ = r.Close()
			return domain.Object{}, nil, err
		}
		return obj, r, nil
	}
}

func checkSize(r io.Seeker, want int64) error {
	size, err := r.Seek(0, io.SeekEnd)
	if err != nil {
		return err
	}
	if size != want {
		return ErrDamaged
	}
	_, err = r.Seek(0, io.SeekStart)
	return err
}

// DeleteObject removes an object. Deleting a missing object succeeds.
func (e *Engine) DeleteObject(bucket, key string) error {
	old, err := e.meta.DeleteObject(bucket, key)
	if err != nil {
		return err
	}
	if old != "" {
		_ = e.blobs.Delete(old)
	}
	return nil
}
