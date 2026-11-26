//go:build linux

package storage

import (
	"context"
	"crypto/md5"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/ashu-wolf/gostore/internal/blob"
	"github.com/ashu-wolf/gostore/internal/domain"
	"github.com/ashu-wolf/gostore/internal/meta"
)

func newUploadID() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", fmt.Errorf("create upload ID: %w", err)
	}
	return hex.EncodeToString(b[:]), nil
}

// InitiateMultipartUpload starts a new multipart upload for (bucket, key).
func (e *Engine) InitiateMultipartUpload(bucket, key, contentType string, metadata map[string]string) (domain.Upload, error) {
	if err := domain.ValidateKey(key); err != nil {
		return domain.Upload{}, err
	}
	if err := domain.ValidateMetadata(metadata); err != nil {
		return domain.Upload{}, err
	}
	id, err := newUploadID()
	if err != nil {
		return domain.Upload{}, err
	}
	now := e.now().UTC()
	u := domain.Upload{
		ID:           id,
		Bucket:       bucket,
		Key:          key,
		ContentType:  contentType,
		Metadata:     metadata,
		CreatedAt:    now,
		LastActiveAt: now,
	}
	if err := e.meta.CreateUpload(u); err != nil {
		return domain.Upload{}, err
	}
	return u, nil
}

// UploadPart stores one part of an active multipart upload.
func (e *Engine) UploadPart(ctx context.Context, bucket, key, uploadID string, partNumber int, checks blob.PutOptions, body io.Reader) (domain.Part, error) {
	if partNumber < 1 || partNumber > e.opts.MaxUploadParts {
		return domain.Part{}, fmt.Errorf("%w: part number %d out of range", domain.ErrInvalidPart, partNumber)
	}
	if checks.MaxSize <= 0 || checks.MaxSize > e.opts.MaxPartSize {
		checks.MaxSize = e.opts.MaxPartSize
	}

	e.pinUpload(uploadID)
	defer e.unpinUpload(uploadID)

	if err := e.meta.HasUpload(bucket, key, uploadID); err != nil {
		return domain.Part{}, err
	}

	info, err := e.blobs.Put(ctx, body, checks)
	if err != nil {
		return domain.Part{}, err
	}
	p := domain.Part{
		UploadID:   uploadID,
		Number:     partNumber,
		BlobID:     info.ID,
		Size:       info.Size,
		ETag:       info.MD5,
		ModifiedAt: e.now().UTC(),
	}
	old, err := e.meta.PutPart(bucket, key, p)
	if err != nil {
		if !errors.Is(err, meta.ErrCommitUnknown) {
			e.deleteBlob(info.ID)
		}
		return domain.Part{}, err
	}
	if old != "" {
		e.deleteBlob(old)
	}
	return p, nil
}

// ListParts returns a page of uploaded parts for an active multipart upload.
func (e *Engine) ListParts(opts domain.ListPartsOptions) (domain.ListPartsResult, error) {
	if opts.PartNumberMarker < 0 {
		return domain.ListPartsResult{}, fmt.Errorf("%w: part number marker must not be negative", domain.ErrInvalidArgument)
	}
	if opts.MaxParts < 0 {
		return domain.ListPartsResult{}, fmt.Errorf("%w: max parts must not be negative", domain.ErrInvalidArgument)
	}
	if opts.MaxParts == 0 || opts.MaxParts > domain.DefaultMaxKeys {
		opts.MaxParts = domain.DefaultMaxKeys
	}
	e.pinUpload(opts.UploadID)
	defer e.unpinUpload(opts.UploadID)
	return e.meta.ListParts(opts)
}

// AbortMultipartUpload cancels an active upload and deletes its part blobs.
func (e *Engine) AbortMultipartUpload(bucket, key, uploadID string) error {
	blobIDs, err := e.meta.AbortUpload(bucket, key, uploadID)
	if err != nil {
		return err
	}
	e.deleteBlobs(blobIDs)
	return nil
}

// CompleteMultipartUpload validates the submitted part list, assembles a new
// synced blob, atomically publishes the object, and saves the completion result.
func (e *Engine) CompleteMultipartUpload(ctx context.Context, bucket, key, uploadID string, submitted []domain.CompletedPart) (domain.CompletedUpload, error) {
	now := e.now().UTC()
	if c, ok := e.meta.Completion(bucket, key, uploadID, now, e.opts.CompletionRetention); ok {
		return c, nil
	}
	if len(submitted) == 0 {
		return domain.CompletedUpload{}, fmt.Errorf("%w: no parts provided", domain.ErrInvalidPart)
	}
	if len(submitted) > e.opts.MaxUploadParts {
		return domain.CompletedUpload{}, fmt.Errorf("%w: too many parts", domain.ErrInvalidPart)
	}
	for i, sp := range submitted {
		if sp.Number < 1 || sp.Number > e.opts.MaxUploadParts {
			return domain.CompletedUpload{}, fmt.Errorf("%w: part number %d out of range", domain.ErrInvalidPart, sp.Number)
		}
		if i > 0 && sp.Number <= submitted[i-1].Number {
			return domain.CompletedUpload{}, domain.ErrInvalidOrder
		}
	}

	e.pinUpload(uploadID)
	defer e.unpinUpload(uploadID)

	e.pinMu.Lock()
	info, storedParts, snapErr := e.meta.UploadSnapshot(bucket, key, uploadID, func(parts []domain.Part) {
		for _, p := range parts {
			e.blobPins[p.BlobID]++
		}
	})
	e.pinMu.Unlock()

	if snapErr != nil {
		if errors.Is(snapErr, domain.ErrNoSuchUpload) {
			if c, ok := e.meta.Completion(bucket, key, uploadID, e.now().UTC(), e.opts.CompletionRetention); ok {
				return c, nil
			}
		}
		return domain.CompletedUpload{}, snapErr
	}

	pinnedIDs := make([]string, len(storedParts))
	partByNum := make(map[int]domain.Part, len(storedParts))
	for i, p := range storedParts {
		pinnedIDs[i] = p.BlobID
		partByNum[p.Number] = p
	}
	defer e.unpinBlobs(pinnedIDs)

	selected := make([]domain.Part, len(submitted))
	var totalSize int64
	md5OfMD5s := md5.New()

	for i, sp := range submitted {
		p, ok := partByNum[sp.Number]
		if !ok {
			return domain.CompletedUpload{}, fmt.Errorf("%w: part %d does not exist", domain.ErrInvalidPart, sp.Number)
		}
		wantETag := strings.Trim(sp.ETag, "\"")
		if !strings.EqualFold(wantETag, p.ETag) {
			return domain.CompletedUpload{}, fmt.Errorf("%w: part %d ETag mismatch", domain.ErrInvalidPart, sp.Number)
		}
		if i < len(submitted)-1 && p.Size < e.opts.MinPartSize {
			return domain.CompletedUpload{}, fmt.Errorf("%w: part %d size %d below minimum %d", domain.ErrEntityTooSmall, sp.Number, p.Size, e.opts.MinPartSize)
		}
		if p.Size > e.opts.MaxPartSize {
			return domain.CompletedUpload{}, fmt.Errorf("%w: part %d exceeds max part size", domain.ErrEntityTooLarge, sp.Number)
		}
		if totalSize > e.opts.MaxMultipartObjectSize-p.Size {
			return domain.CompletedUpload{}, fmt.Errorf("%w: completed object exceeds max multipart size", domain.ErrEntityTooLarge)
		}
		totalSize += p.Size

		rawDigest, err := hex.DecodeString(p.ETag)
		if err != nil {
			return domain.CompletedUpload{}, fmt.Errorf("decode part %d ETag: %w", p.Number, err)
		}
		_, _ = md5OfMD5s.Write(rawDigest)
		selected[i] = p
	}

	mbr := &multiBlobReader{blobs: e.blobs, parts: selected}
	defer mbr.Close()

	assembled, err := e.blobs.Put(ctx, mbr, blob.PutOptions{WantSize: &totalSize})
	if err != nil {
		return domain.CompletedUpload{}, err
	}

	mpETag := fmt.Sprintf("%s-%d", hex.EncodeToString(md5OfMD5s.Sum(nil)), len(selected))
	nowDone := e.now().UTC()
	obj := domain.Object{
		Bucket:      bucket,
		Key:         key,
		BlobID:      assembled.ID,
		Size:        assembled.Size,
		ETag:        mpETag,
		SHA256:      assembled.SHA256,
		ContentType: info.ContentType,
		Metadata:    info.Metadata,
		ModifiedAt:  nowDone,
	}
	completed := domain.CompletedUpload{
		UploadID:    uploadID,
		Bucket:      bucket,
		Key:         key,
		ETag:        mpETag,
		CompletedAt: nowDone,
	}

	already, res, oldObjBlob, partBlobs, err := e.meta.CompleteUpload(obj, uploadID, selected, completed, nowDone, e.opts.CompletionRetention)
	if err != nil {
		if !errors.Is(err, meta.ErrCommitUnknown) {
			e.deleteBlob(assembled.ID)
		}
		return domain.CompletedUpload{}, err
	}
	if already {
		e.deleteBlob(assembled.ID)
		return res, nil
	}
	if oldObjBlob != "" {
		e.deleteBlob(oldObjBlob)
	}
	e.deleteBlobs(partBlobs)
	return res, nil
}

// ExpireUploads aborts inactive uploads older than UploadExpiry and purges
// expired completion records.
func (e *Engine) ExpireUploads(now time.Time) (int, error) {
	utcNow := now.UTC()
	expiredBlobs, count, err := e.meta.ExpireUploads(
		utcNow.Add(-e.opts.UploadExpiry),
		utcNow.Add(-e.opts.CompletionRetention),
		e.isUploadActive,
	)
	e.deleteBlobs(expiredBlobs)
	return count, err
}

func (e *Engine) startExpiryWorker() {
	e.wg.Add(1)
	go func() {
		defer e.wg.Done()
		ticker := time.NewTicker(e.opts.ExpiryInterval)
		defer ticker.Stop()
		for {
			select {
			case <-e.stopCh:
				return
			case <-ticker.C:
				_, _ = e.ExpireUploads(e.now())
			}
		}
	}()
}

func (e *Engine) pinUpload(id string) {
	e.pinMu.Lock()
	e.uploadPins[id]++
	e.pinMu.Unlock()
}

func (e *Engine) unpinUpload(id string) {
	e.pinMu.Lock()
	if n := e.uploadPins[id]; n <= 1 {
		delete(e.uploadPins, id)
	} else {
		e.uploadPins[id] = n - 1
	}
	e.pinMu.Unlock()
}

func (e *Engine) isUploadActive(id string) bool {
	e.pinMu.Lock()
	active := e.uploadPins[id] > 0
	e.pinMu.Unlock()
	return active
}

func (e *Engine) unpinBlobs(ids []string) {
	var toDelete []string
	e.pinMu.Lock()
	for _, id := range ids {
		if n := e.blobPins[id]; n <= 1 {
			delete(e.blobPins, id)
			if e.pendingDelete[id] {
				delete(e.pendingDelete, id)
				toDelete = append(toDelete, id)
			}
		} else {
			e.blobPins[id] = n - 1
		}
	}
	e.pinMu.Unlock()
	for _, id := range toDelete {
		_ = e.blobs.Delete(id)
	}
}

func (e *Engine) deleteBlob(id string) {
	e.pinMu.Lock()
	if e.blobPins[id] > 0 {
		e.pendingDelete[id] = true
		e.pinMu.Unlock()
		return
	}
	e.pinMu.Unlock()
	_ = e.blobs.Delete(id)
}

func (e *Engine) deleteBlobs(ids []string) {
	for _, id := range ids {
		e.deleteBlob(id)
	}
}

// multiBlobReader streams part blobs sequentially using at most one open file
// descriptor at a time.
type multiBlobReader struct {
	blobs *blob.Store
	parts []domain.Part
	idx   int
	cur   io.ReadSeekCloser
	read  int64
}

func (m *multiBlobReader) Read(p []byte) (int, error) {
	for {
		if m.cur == nil {
			if m.idx >= len(m.parts) {
				return 0, io.EOF
			}
			part := m.parts[m.idx]
			r, err := m.blobs.Open(part.BlobID)
			if err != nil {
				return 0, fmt.Errorf("open part %d: %w", part.Number, err)
			}
			if err := checkSize(r, part.Size); err != nil {
				_ = r.Close()
				return 0, fmt.Errorf("part %d: %w", part.Number, err)
			}
			m.cur = r
			m.read = 0
		}

		n, err := m.cur.Read(p)
		m.read += int64(n)
		if err == io.EOF {
			want := m.parts[m.idx].Size
			_ = m.cur.Close()
			m.cur = nil
			m.idx++
			if m.read != want {
				return n, fmt.Errorf("%w: part size mismatch", ErrDamaged)
			}
			if n > 0 {
				return n, nil
			}
			continue
		}
		if err != nil {
			return n, err
		}
		return n, nil
	}
}

func (m *multiBlobReader) Close() error {
	if m.cur != nil {
		err := m.cur.Close()
		m.cur = nil
		return err
	}
	return nil
}
