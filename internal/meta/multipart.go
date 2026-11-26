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
	return s.submit(func(ov *batchOverlay) ([]record, error) {
		if !ov.hasBucket(u.Bucket) {
			return nil, domain.ErrNoSuchBucket
		}
		if ov.getUpload(u.ID) != nil {
			return nil, fmt.Errorf("duplicate upload %q", u.ID)
		}
		return []record{{Op: opCreateUpload, Upload: &u}}, nil
	})
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
	err = s.submit(func(ov *batchOverlay) ([]record, error) {
		if !ov.hasBucket(bucket) {
			return nil, domain.ErrNoSuchBucket
		}
		u := ov.getUpload(p.UploadID)
		if u == nil || u.info.Bucket != bucket || u.info.Key != key {
			return nil, domain.ErrNoSuchUpload
		}
		oldBlobID = u.parts[p.Number].BlobID
		return []record{{Op: opPutPart, Part: &p}}, nil
	})
	if err != nil {
		return "", err
	}
	return oldBlobID, nil
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
	var blobIDs []string
	err := s.submit(func(ov *batchOverlay) ([]record, error) {
		if !ov.hasBucket(bucket) {
			return nil, domain.ErrNoSuchBucket
		}
		u := ov.getUpload(uploadID)
		if u == nil || u.info.Bucket != bucket || u.info.Key != key {
			return nil, domain.ErrNoSuchUpload
		}
		blobIDs = make([]string, 0, len(u.parts))
		for _, p := range u.parts {
			blobIDs = append(blobIDs, p.BlobID)
		}
		return []record{{Op: opAbortUpload, Upload: &domain.Upload{ID: uploadID}}}, nil
	})
	if err != nil {
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
	err = s.submit(func(ov *batchOverlay) ([]record, error) {
		if !ov.hasBucket(obj.Bucket) {
			return nil, domain.ErrNoSuchBucket
		}
		if c, ok := ov.getCompletion(uploadID); ok && c.Bucket == obj.Bucket && c.Key == obj.Key && now.Sub(c.CompletedAt) < retention {
			alreadyCompleted = true
			res = c
			return nil, nil
		}
		u := ov.getUpload(uploadID)
		if u == nil || u.info.Bucket != obj.Bucket || u.info.Key != obj.Key {
			return nil, domain.ErrNoSuchUpload
		}
		for _, want := range expected {
			got, ok := u.parts[want.Number]
			if !ok || got.BlobID != want.BlobID {
				return nil, domain.ErrInvalidPart
			}
		}

		merged := obj
		if merged.ContentType == "" {
			merged.ContentType = u.info.ContentType
		}
		if merged.Metadata == nil && u.info.Metadata != nil {
			merged.Metadata = maps.Clone(u.info.Metadata)
		}
		_, oldObjBlobID = ov.getObject(merged.Bucket, merged.Key)
		partBlobIDs = make([]string, 0, len(u.parts))
		for _, p := range u.parts {
			partBlobIDs = append(partBlobIDs, p.BlobID)
		}
		res = completed
		return []record{{
			Op:        opCompleteUpload,
			Upload:    &domain.Upload{ID: uploadID},
			Object:    &merged,
			Completed: &completed,
		}}, nil
	})
	if err != nil {
		return false, domain.CompletedUpload{}, "", nil, err
	}
	if alreadyCompleted {
		return true, res, "", nil, nil
	}
	return false, res, oldObjBlobID, partBlobIDs, nil
}

// ExpireUploads aborts inactive uploads older than uploadCutoff and purges
// completion records older than completionCutoff.
func (s *Store) ExpireUploads(uploadCutoff, completionCutoff time.Time, isActive func(string) bool) (expiredBlobs []string, expiredCount int, err error) {
	err = s.submit(func(ov *batchOverlay) ([]record, error) {
		var toAbort []string
		for _, id := range ov.allUploadIDs() {
			u := ov.getUpload(id)
			if u == nil || !u.info.LastActiveAt.Before(uploadCutoff) {
				continue
			}
			if isActive != nil && isActive(id) {
				continue
			}
			toAbort = append(toAbort, id)
		}
		slices.Sort(toAbort)
		const maxExpirePerRun = 100
		if len(toAbort) > maxExpirePerRun {
			toAbort = toAbort[:maxExpirePerRun]
		}

		var toPurge []string
		for _, id := range ov.allCompletionIDs() {
			c, ok := ov.getCompletion(id)
			if ok && c.CompletedAt.Before(completionCutoff) {
				toPurge = append(toPurge, id)
			}
		}
		slices.Sort(toPurge)

		recs := make([]record, 0, len(toAbort)+len(toPurge))
		expiredBlobs = nil
		for _, id := range toAbort {
			u := ov.getUpload(id)
			for _, p := range u.parts {
				expiredBlobs = append(expiredBlobs, p.BlobID)
			}
			recs = append(recs, record{Op: opAbortUpload, Upload: &domain.Upload{ID: id}})
		}
		expiredCount = len(toAbort)
		for _, id := range toPurge {
			recs = append(recs, record{Op: opPurgeCompletion, Completed: &domain.CompletedUpload{UploadID: id}})
		}
		return recs, nil
	})
	if err != nil {
		return nil, 0, err
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
