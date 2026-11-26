//go:build linux

// Package storage combines blob data and metadata into crash-safe operations.
package storage

import (
	"context"
	"errors"
	"fmt"
	"io"
	"path/filepath"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/ashu-wolf/gostore/internal/blob"
	"github.com/ashu-wolf/gostore/internal/crashpoint"
	"github.com/ashu-wolf/gostore/internal/domain"
	"github.com/ashu-wolf/gostore/internal/meta"
)

// ErrDamaged means an object's data is missing or has the wrong size.
var ErrDamaged = errors.New("object data is damaged")

// Options configures a storage Engine. Zero fields use defaults.
type Options struct {
	MaxPartSize            int64
	MaxMultipartObjectSize int64
	MinPartSize            int64
	MaxUploadParts         int
	UploadExpiry           time.Duration
	CompletionRetention    time.Duration
	ExpiryInterval         time.Duration
	DisableExpiryWorker    bool
	Now                    func() time.Time
}

// Engine is safe for concurrent use. Close it only after all calls return.
type Engine struct {
	blobs    *blob.Store
	meta     *meta.Store
	opts     Options
	now      func() time.Time
	onLookup func() // test hook called after meta lookup in lookupObject

	pinMu         sync.Mutex
	uploadPins    map[string]int
	blobPins      map[string]int
	pendingDelete map[string]bool

	stopCh    chan struct{}
	closeOnce sync.Once
	wg        sync.WaitGroup
}

// Report describes what startup recovery found.
type Report struct {
	TempRemoved    int
	OrphansRemoved int
	DiscardedBytes int
	Compacted      bool
	Damaged        []string // "bucket/key" of objects with bad data
}

// Open loads the data directory with default options and runs recovery.
func Open(root string) (*Engine, Report, error) {
	return OpenWithOptions(root, Options{})
}

// OpenWithOptions loads the data directory and runs recovery. It must finish
// before the server accepts requests.
func OpenWithOptions(root string, opts Options) (_ *Engine, report Report, err error) {
	if opts.MaxPartSize <= 0 {
		opts.MaxPartSize = domain.DefaultMaxPartSize
	}
	if opts.MaxMultipartObjectSize <= 0 {
		opts.MaxMultipartObjectSize = domain.DefaultMaxMultipartObjectSize
	}
	if opts.MinPartSize <= 0 {
		opts.MinPartSize = domain.DefaultMinPartSize
	}
	if opts.MaxUploadParts <= 0 {
		opts.MaxUploadParts = domain.DefaultMaxUploadParts
	}
	if opts.UploadExpiry <= 0 {
		opts.UploadExpiry = domain.DefaultUploadExpiry
	}
	if opts.CompletionRetention <= 0 {
		opts.CompletionRetention = domain.DefaultUploadExpiry
	}
	if opts.ExpiryInterval <= 0 {
		opts.ExpiryInterval = time.Minute
	}
	if opts.Now == nil {
		opts.Now = time.Now
	}

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
	for _, partBlobID := range m.AllPartBlobIDs() {
		used[partBlobID] = true
	}
	for _, id := range ids {
		if !used[id] {
			if err := blobs.Delete(id); err != nil {
				return nil, report, fmt.Errorf("remove unused blob: %w", err)
			}
			report.OrphansRemoved++
		}
	}
	e := &Engine{
		blobs:         blobs,
		meta:          m,
		opts:          opts,
		now:           opts.Now,
		uploadPins:    map[string]int{},
		blobPins:      map[string]int{},
		pendingDelete: map[string]bool{},
		stopCh:        make(chan struct{}),
	}
	if !opts.DisableExpiryWorker {
		e.startExpiryWorker()
	}
	return e, report, nil
}

// Close stops background workers, closes the metadata journal, and releases
// the directory lock.
func (e *Engine) Close() error {
	var err error
	e.closeOnce.Do(func() {
		close(e.stopCh)
		e.wg.Wait()
		err = errors.Join(e.meta.Close(), e.blobs.Close())
	})
	return err
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

// ListBuckets returns all buckets sorted by name.
func (e *Engine) ListBuckets() []domain.Bucket {
	return e.meta.ListBuckets()
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
			e.deleteBlob(info.ID)
		}
		return domain.Object{}, err
	}
	crashpoint.Hit("storage.after-commit")

	if old != "" {
		e.deleteBlob(old) // recovery retries if this fails
	}
	return obj, nil
}

// HeadObject returns an object's metadata after verifying that its blob
// exists and has the expected size.
func (e *Engine) HeadObject(bucket, key string) (domain.Object, error) {
	obj, _, err := lookupObject(e, bucket, key, func(obj domain.Object) (struct{}, error) {
		size, err := e.blobs.Size(obj.BlobID)
		if err != nil {
			return struct{}{}, err
		}
		if size != obj.Size {
			return struct{}{}, ErrDamaged
		}
		return struct{}{}, nil
	})
	return obj, err
}

// OpenObject returns an object's record and data. The caller must close the
// reader. The data is not checked against its SHA-256 here.
func (e *Engine) OpenObject(bucket, key string) (domain.Object, io.ReadSeekCloser, error) {
	return lookupObject(e, bucket, key, func(obj domain.Object) (io.ReadSeekCloser, error) {
		r, err := e.blobs.Open(obj.BlobID)
		if err != nil {
			return nil, err
		}
		if err := checkSize(r, obj.Size); err != nil {
			_ = r.Close()
			return nil, err
		}
		return r, nil
	})
}

func lookupObject[T any](e *Engine, bucket, key string, access func(domain.Object) (T, error)) (domain.Object, T, error) {
	tried := ""
	for {
		obj, err := e.meta.Object(bucket, key)
		if err != nil {
			var zero T
			return domain.Object{}, zero, err
		}
		if obj.BlobID == tried {
			var zero T
			return domain.Object{}, zero, ErrDamaged
		}
		if e.onLookup != nil {
			e.onLookup()
		}
		val, err := access(obj)
		if errors.Is(err, blob.ErrNotFound) {
			// The object may have been replaced after the lookup. Look again.
			tried = obj.BlobID
			continue
		}
		if err != nil {
			var zero T
			return domain.Object{}, zero, err
		}
		return obj, val, nil
	}
}

// ListObjects returns a page of objects and common prefixes in UTF-8 byte order.
func (e *Engine) ListObjects(opts domain.ListOptions) (domain.ListResult, error) {
	if !utf8.ValidString(opts.Prefix) {
		return domain.ListResult{}, fmt.Errorf("%w: prefix is not valid UTF-8", domain.ErrInvalidArgument)
	}
	if !utf8.ValidString(opts.Delimiter) {
		return domain.ListResult{}, fmt.Errorf("%w: delimiter is not valid UTF-8", domain.ErrInvalidArgument)
	}
	if opts.MaxKeys < 0 {
		return domain.ListResult{}, fmt.Errorf("%w: max keys must not be negative", domain.ErrInvalidArgument)
	}
	maxKeys := opts.MaxKeys
	if maxKeys == 0 || maxKeys > domain.DefaultMaxKeys {
		maxKeys = domain.DefaultMaxKeys
	}

	var after string
	var afterIsPrefix bool
	if opts.ContinuationToken != "" {
		tok, err := domain.DecodeListToken(opts.ContinuationToken, opts.Bucket, opts.Prefix, opts.Delimiter)
		if err != nil {
			return domain.ListResult{}, err
		}
		after = tok.After
		afterIsPrefix = opts.Delimiter != "" && strings.Contains(tok.After[len(opts.Prefix):], opts.Delimiter)
	} else if opts.StartAfter != "" {
		after = opts.StartAfter
	}

	page, err := e.meta.ListObjects(opts.Bucket, opts.Prefix, opts.Delimiter, after, afterIsPrefix, maxKeys)
	if err != nil {
		return domain.ListResult{}, err
	}
	res := domain.ListResult{
		Objects:        page.Objects,
		CommonPrefixes: page.CommonPrefixes,
		IsTruncated:    page.Truncated,
	}
	if page.Truncated {
		tok, err := (domain.ListToken{
			Bucket:    opts.Bucket,
			Prefix:    opts.Prefix,
			Delimiter: opts.Delimiter,
			After:     page.LastItem,
		}).Encode()
		if err != nil {
			return domain.ListResult{}, fmt.Errorf("encode continuation token: %w", err)
		}
		res.NextContinuationToken = tok
	}
	return res, nil
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
		e.deleteBlob(old)
	}
	return nil
}
