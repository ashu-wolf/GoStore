//go:build linux

package meta

import (
	"fmt"
	"maps"
	"slices"
	"time"

	"github.com/ashu-wolf/gostore/internal/domain"
)

type uploadState struct {
	info  domain.Upload
	parts map[int]domain.Part
}

func (s *Store) applyMultipart(r record) error {
	if s.uploads == nil {
		s.uploads = map[string]*uploadState{}
	}
	if s.completions == nil {
		s.completions = map[string]domain.CompletedUpload{}
	}
	switch {
	case r.Op == opCreateUpload && r.Upload != nil:
		if s.buckets[r.Upload.Bucket] == nil {
			return domain.ErrNoSuchBucket
		}
		if s.uploads[r.Upload.ID] != nil {
			return fmt.Errorf("duplicate upload %q", r.Upload.ID)
		}
		u := *r.Upload
		u.Metadata = maps.Clone(u.Metadata)
		s.uploads[u.ID] = &uploadState{info: u, parts: map[int]domain.Part{}}

	case r.Op == opPutPart && r.Part != nil:
		u := s.uploads[r.Part.UploadID]
		if u == nil {
			return domain.ErrNoSuchUpload
		}
		u.parts[r.Part.Number] = *r.Part
		if r.Part.ModifiedAt.After(u.info.LastActiveAt) {
			u.info.LastActiveAt = r.Part.ModifiedAt
		}

	case r.Op == opAbortUpload && r.Upload != nil:
		if s.uploads[r.Upload.ID] == nil {
			return domain.ErrNoSuchUpload
		}
		delete(s.uploads, r.Upload.ID)

	case r.Op == opCompleteUpload && r.Upload != nil && r.Object != nil && r.Completed != nil:
		b := s.buckets[r.Object.Bucket]
		if b == nil {
			return domain.ErrNoSuchBucket
		}
		if s.uploads[r.Upload.ID] == nil {
			return domain.ErrNoSuchUpload
		}
		obj := *r.Object
		obj.Metadata = maps.Clone(obj.Metadata)
		b.putObject(obj)
		delete(s.uploads, r.Upload.ID)
		s.completions[r.Completed.UploadID] = *r.Completed

	case r.Op == opSaveCompletion && r.Completed != nil:
		s.completions[r.Completed.UploadID] = *r.Completed

	case r.Op == opPurgeCompletion && r.Completed != nil:
		delete(s.completions, r.Completed.UploadID)

	default:
		return fmt.Errorf("unknown multipart record %q", r.Op)
	}
	return nil
}

func (s *Store) snapshotMultipart() []record {
	var out []record
	for _, id := range slices.Sorted(maps.Keys(s.uploads)) {
		u := s.uploads[id]
		info := u.info
		info.Metadata = maps.Clone(info.Metadata)
		out = append(out, record{Op: opCreateUpload, Upload: &info})
		for _, num := range slices.Sorted(maps.Keys(u.parts)) {
			p := u.parts[num]
			out = append(out, record{Op: opPutPart, Part: &p})
		}
	}
	for _, id := range slices.Sorted(maps.Keys(s.completions)) {
		c := s.completions[id]
		out = append(out, record{Op: opSaveCompletion, Completed: &c})
	}
	return out
}

// CreateUpload records a new active multipart upload.
func (s *Store) CreateUpload(u domain.Upload) error {
	u.Metadata = maps.Clone(u.Metadata)
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	if s.buckets[u.Bucket] == nil {
		return domain.ErrNoSuchBucket
	}
	return s.commit(record{Op: opCreateUpload, Upload: &u})
}

// HasUpload verifies that the bucket and active upload exist for (bucket, key).
func (s *Store) HasUpload(bucket, key, uploadID string) error {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.buckets[bucket] == nil {
		return domain.ErrNoSuchBucket
	}
	u := s.uploads[uploadID]
	if u == nil || u.info.Bucket != bucket || u.info.Key != key {
		return domain.ErrNoSuchUpload
	}
	return nil
}

// PutPart stores or replaces a part record and returns any replaced blob ID.
func (s *Store) PutPart(bucket, key string, p domain.Part) (oldBlobID string, err error) {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	if s.buckets[bucket] == nil {
		return "", domain.ErrNoSuchBucket
	}
	u := s.uploads[p.UploadID]
	if u == nil || u.info.Bucket != bucket || u.info.Key != key {
		return "", domain.ErrNoSuchUpload
	}
	old := u.parts[p.Number].BlobID
	if err := s.commit(record{Op: opPutPart, Part: &p}); err != nil {
		return "", err
	}
	return old, nil
}

// UploadSnapshot returns the upload info and sorted parts, invoking pinFn
// while holding RLock so callers can pin part blobs before another writer
// deletes them.
func (s *Store) UploadSnapshot(bucket, key, uploadID string, pinFn func([]domain.Part)) (domain.Upload, []domain.Part, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.buckets[bucket] == nil {
		return domain.Upload{}, nil, domain.ErrNoSuchBucket
	}
	u := s.uploads[uploadID]
	if u == nil || u.info.Bucket != bucket || u.info.Key != key {
		return domain.Upload{}, nil, domain.ErrNoSuchUpload
	}
	info := u.info
	info.Metadata = maps.Clone(info.Metadata)
	parts := make([]domain.Part, 0, len(u.parts))
	for _, num := range slices.Sorted(maps.Keys(u.parts)) {
		parts = append(parts, u.parts[num])
	}
	if pinFn != nil {
		pinFn(parts)
	}
	return info, parts, nil
}

// ListParts returns a page of parts for an active multipart upload.
func (s *Store) ListParts(opts domain.ListPartsOptions) (domain.ListPartsResult, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.buckets[opts.Bucket] == nil {
		return domain.ListPartsResult{}, domain.ErrNoSuchBucket
	}
	u := s.uploads[opts.UploadID]
	if u == nil || u.info.Bucket != opts.Bucket || u.info.Key != opts.Key {
		return domain.ListPartsResult{}, domain.ErrNoSuchUpload
	}

	info := u.info
	info.Metadata = maps.Clone(info.Metadata)
	res := domain.ListPartsResult{
		Upload:           info,
		PartNumberMarker: opts.PartNumberMarker,
		MaxParts:         opts.MaxParts,
	}
	if opts.MaxParts <= 0 {
		return res, nil
	}

	nums := slices.Sorted(maps.Keys(u.parts))
	idx, found := slices.BinarySearch(nums, opts.PartNumberMarker)
	if found {
		idx++
	}
	for ; idx < len(nums); idx++ {
		if len(res.Parts) == opts.MaxParts {
			res.IsTruncated = true
			break
		}
		p := u.parts[nums[idx]]
		res.Parts = append(res.Parts, p)
		res.NextPartNumberMarker = p.Number
	}
	if !res.IsTruncated {
		res.NextPartNumberMarker = 0
	}
	return res, nil
}

// AbortUpload removes an active upload and returns its part blob IDs.
func (s *Store) AbortUpload(bucket, key, uploadID string) ([]string, error) {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	if s.buckets[bucket] == nil {
		return nil, domain.ErrNoSuchBucket
	}
	u := s.uploads[uploadID]
	if u == nil || u.info.Bucket != bucket || u.info.Key != key {
		return nil, domain.ErrNoSuchUpload
	}
	blobIDs := make([]string, 0, len(u.parts))
	for _, p := range u.parts {
		blobIDs = append(blobIDs, p.BlobID)
	}
	if err := s.commit(record{Op: opAbortUpload, Upload: &domain.Upload{ID: uploadID}}); err != nil {
		return nil, err
	}
	return blobIDs, nil
}

// Completion returns a saved completion result if it is still within retention.
func (s *Store) Completion(bucket, key, uploadID string, now time.Time, retention time.Duration) (domain.CompletedUpload, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	c, ok := s.completions[uploadID]
	if !ok || c.Bucket != bucket || c.Key != key || now.Sub(c.CompletedAt) >= retention {
		return domain.CompletedUpload{}, false
	}
	return c, true
}

// CompleteUpload atomically checks whether the upload is still active (or already
// completed), publishes obj, removes upload state, and saves completed.
func (s *Store) CompleteUpload(
	obj domain.Object,
	uploadID string,
	expected []domain.Part,
	completed domain.CompletedUpload,
	now time.Time,
	retention time.Duration,
) (alreadyCompleted bool, res domain.CompletedUpload, oldObjBlobID string, partBlobIDs []string, err error) {
	obj.Metadata = maps.Clone(obj.Metadata)
	s.writeMu.Lock()
	defer s.writeMu.Unlock()

	b := s.buckets[obj.Bucket]
	if b == nil {
		return false, domain.CompletedUpload{}, "", nil, domain.ErrNoSuchBucket
	}
	if c, ok := s.completions[uploadID]; ok && c.Bucket == obj.Bucket && c.Key == obj.Key && now.Sub(c.CompletedAt) < retention {
		return true, c, "", nil, nil
	}
	u := s.uploads[uploadID]
	if u == nil || u.info.Bucket != obj.Bucket || u.info.Key != obj.Key {
		return false, domain.CompletedUpload{}, "", nil, domain.ErrNoSuchUpload
	}
	for _, want := range expected {
		got, ok := u.parts[want.Number]
		if !ok || got.BlobID != want.BlobID {
			return false, domain.CompletedUpload{}, "", nil, domain.ErrInvalidPart
		}
	}

	if obj.ContentType == "" {
		obj.ContentType = u.info.ContentType
	}
	if obj.Metadata == nil && u.info.Metadata != nil {
		obj.Metadata = maps.Clone(u.info.Metadata)
	}
	oldObjBlobID = b.objects[obj.Key].BlobID
	for _, p := range u.parts {
		partBlobIDs = append(partBlobIDs, p.BlobID)
	}

	err = s.commit(record{
		Op:        opCompleteUpload,
		Upload:    &domain.Upload{ID: uploadID},
		Object:    &obj,
		Completed: &completed,
	})
	if err != nil {
		return false, domain.CompletedUpload{}, "", nil, err
	}
	return false, completed, oldObjBlobID, partBlobIDs, nil
}

// ExpireUploads aborts inactive uploads older than uploadCutoff and purges
// completion records older than completionCutoff.
func (s *Store) ExpireUploads(uploadCutoff, completionCutoff time.Time, isActive func(string) bool) (expiredBlobs []string, expiredCount int, err error) {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()

	var toAbort []string
	for id, u := range s.uploads {
		if !u.info.LastActiveAt.Before(uploadCutoff) {
			continue
		}
		if isActive != nil && isActive(id) {
			continue
		}
		toAbort = append(toAbort, id)
	}
	slices.Sort(toAbort)

	for _, id := range toAbort {
		u := s.uploads[id]
		for _, p := range u.parts {
			expiredBlobs = append(expiredBlobs, p.BlobID)
		}
		if err := s.commit(record{Op: opAbortUpload, Upload: &domain.Upload{ID: id}}); err != nil {
			return expiredBlobs, expiredCount, err
		}
		expiredCount++
	}

	var toPurge []string
	for id, c := range s.completions {
		if c.CompletedAt.Before(completionCutoff) {
			toPurge = append(toPurge, id)
		}
	}
	slices.Sort(toPurge)
	for _, id := range toPurge {
		if err := s.commit(record{Op: opPurgeCompletion, Completed: &domain.CompletedUpload{UploadID: id}}); err != nil {
			return expiredBlobs, expiredCount, err
		}
	}

	return expiredBlobs, expiredCount, nil
}

// AllPartBlobIDs returns every blob ID referenced by active multipart uploads.
func (s *Store) AllPartBlobIDs() []string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	var ids []string
	for _, u := range s.uploads {
		for _, p := range u.parts {
			ids = append(ids, p.BlobID)
		}
	}
	return ids
}
