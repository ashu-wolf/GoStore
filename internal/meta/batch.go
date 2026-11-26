//go:build linux

package meta

import (
	"fmt"
	"maps"

	"github.com/ashu-wolf/gostore/internal/crashpoint"
	"github.com/ashu-wolf/gostore/internal/domain"
)

type writeReq struct {
	prepare      func(ov *batchOverlay) ([]record, error)
	done         chan error
	becomeLeader chan struct{}
	err          error
	committed    bool
}

// submit enqueues a metadata mutation for group commit and blocks until either
// its validation error returns or its journal write and Sync complete.
func (s *Store) submit(prepare func(ov *batchOverlay) ([]record, error)) error {
	req := &writeReq{
		prepare:      prepare,
		done:         make(chan error, 1),
		becomeLeader: make(chan struct{}),
	}

	s.queueMu.Lock()
	s.pending = append(s.pending, req)
	leader := !s.flushing
	if leader {
		s.flushing = true
	}
	s.queueMu.Unlock()

	if !leader {
		select {
		case err := <-req.done:
			return err
		case <-req.becomeLeader:
		}
	}

	for {
		s.queueMu.Lock()
		batch := s.pending
		s.pending = nil
		s.queueMu.Unlock()

		s.flushBatch(batch)

		s.queueMu.Lock()
		if len(s.pending) == 0 {
			s.flushing = false
			s.queueMu.Unlock()
			break
		}
		nextLeader := s.pending[0]
		s.queueMu.Unlock()
		close(nextLeader.becomeLeader)
		break
	}
	return <-req.done
}

func (s *Store) flushBatch(batch []*writeReq) {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()

	if s.failed != nil {
		for _, req := range batch {
			req.done <- s.failed
		}
		return
	}

	ov := newBatchOverlay(s)
	var batchBuf []byte
	var records []record

	for _, req := range batch {
		recs, err := req.prepare(ov)
		if err != nil {
			req.err = err
			continue
		}
		if len(recs) == 0 {
			continue
		}
		encoded := make([][]byte, 0, len(recs))
		var encodeErr error
		for _, r := range recs {
			buf, err := encode(r)
			if err != nil {
				encodeErr = err
				break
			}
			encoded = append(encoded, buf)
		}
		if encodeErr != nil {
			req.err = encodeErr
			continue
		}
		for i, r := range recs {
			ov.stage(r)
			batchBuf = append(batchBuf, encoded[i]...)
			records = append(records, r)
		}
		req.committed = true
	}

	if len(batchBuf) > 0 {
		if _, err := s.journal.Write(batchBuf); err != nil {
			s.failed = fmt.Errorf("%w: %w", ErrCommitUnknown, err)
			s.finishBatchFailed(batch, s.failed)
			return
		}
		crashpoint.Hit("meta.after-write")
		if err := s.journal.Sync(); err != nil {
			s.failed = fmt.Errorf("%w: %w", ErrCommitUnknown, err)
			s.finishBatchFailed(batch, s.failed)
			return
		}
		s.mu.Lock()
		for _, r := range records {
			if err := s.apply(r); err != nil {
				s.mu.Unlock()
				s.failed = fmt.Errorf("%w: %w", ErrCommitUnknown, err)
				s.finishBatchFailed(batch, s.failed)
				return
			}
		}
		s.mu.Unlock()
	}

	for _, req := range batch {
		req.done <- req.err
	}
}

func (s *Store) finishBatchFailed(batch []*writeReq, diskErr error) {
	for _, req := range batch {
		if req.err != nil {
			req.done <- req.err
			continue
		}
		if req.committed {
			req.done <- diskErr
			continue
		}
		req.done <- nil
	}
}

type objKey struct {
	bucket string
	key    string
}

type objEdit struct {
	exists bool
	blobID string
}

// batchOverlay presents a sequential view of in-memory state across records
// queued in one group-commit batch without exposing uncommitted writes to
// concurrent readers.
type batchOverlay struct {
	store           *Store
	bucketExists    map[string]bool
	bucketReset     map[string]bool
	bucketDelta     map[string]int
	objEdits        map[objKey]objEdit
	uploadEdits     map[string]*uploadState
	completionEdits map[string]*domain.CompletedUpload
}

func newBatchOverlay(s *Store) *batchOverlay {
	return &batchOverlay{
		store:           s,
		bucketExists:    map[string]bool{},
		bucketReset:     map[string]bool{},
		bucketDelta:     map[string]int{},
		objEdits:        map[objKey]objEdit{},
		uploadEdits:     map[string]*uploadState{},
		completionEdits: map[string]*domain.CompletedUpload{},
	}
}

func (ov *batchOverlay) hasBucket(name string) bool {
	if exists, ok := ov.bucketExists[name]; ok {
		return exists
	}
	return ov.store.buckets[name] != nil
}

func (ov *batchOverlay) bucketObjCount(name string) int {
	base := 0
	if !ov.bucketReset[name] {
		if b := ov.store.buckets[name]; b != nil {
			base = len(b.objects)
		}
	}
	return base + ov.bucketDelta[name]
}

func (ov *batchOverlay) getObject(bucket, key string) (bool, string) {
	if ed, ok := ov.objEdits[objKey{bucket, key}]; ok {
		return ed.exists, ed.blobID
	}
	if ov.bucketReset[bucket] {
		return false, ""
	}
	b := ov.store.buckets[bucket]
	if b == nil {
		return false, ""
	}
	o, ok := b.objects[key]
	if !ok {
		return false, ""
	}
	return true, o.BlobID
}

func (ov *batchOverlay) getUpload(id string) *uploadState {
	if u, ok := ov.uploadEdits[id]; ok {
		return u
	}
	orig := ov.store.uploads[id]
	if orig == nil {
		return nil
	}
	cp := &uploadState{
		info:  orig.info,
		parts: maps.Clone(orig.parts),
	}
	cp.info.Metadata = maps.Clone(orig.info.Metadata)
	ov.uploadEdits[id] = cp
	return cp
}

func (ov *batchOverlay) getCompletion(id string) (domain.CompletedUpload, bool) {
	if c, ok := ov.completionEdits[id]; ok {
		if c == nil {
			return domain.CompletedUpload{}, false
		}
		return *c, true
	}
	c, ok := ov.store.completions[id]
	return c, ok
}

func (ov *batchOverlay) allUploadIDs() []string {
	seen := make(map[string]struct{}, len(ov.store.uploads)+len(ov.uploadEdits))
	for id := range ov.store.uploads {
		seen[id] = struct{}{}
	}
	for id := range ov.uploadEdits {
		seen[id] = struct{}{}
	}
	out := make([]string, 0, len(seen))
	for id := range seen {
		out = append(out, id)
	}
	return out
}

func (ov *batchOverlay) allCompletionIDs() []string {
	seen := make(map[string]struct{}, len(ov.store.completions)+len(ov.completionEdits))
	for id := range ov.store.completions {
		seen[id] = struct{}{}
	}
	for id := range ov.completionEdits {
		seen[id] = struct{}{}
	}
	out := make([]string, 0, len(seen))
	for id := range seen {
		out = append(out, id)
	}
	return out
}

func (ov *batchOverlay) stage(r record) {
	switch {
	case r.Op == opCreateBucket && r.Bucket != nil:
		ov.bucketExists[r.Bucket.Name] = true
		ov.bucketReset[r.Bucket.Name] = true
		ov.bucketDelta[r.Bucket.Name] = 0
	case r.Op == opDeleteBucket && r.Bucket != nil:
		ov.bucketExists[r.Bucket.Name] = false
		ov.bucketReset[r.Bucket.Name] = true
		ov.bucketDelta[r.Bucket.Name] = 0
	case r.Op == opPutObject && r.Object != nil:
		exists, _ := ov.getObject(r.Object.Bucket, r.Object.Key)
		if !exists {
			ov.bucketDelta[r.Object.Bucket]++
		}
		ov.objEdits[objKey{r.Object.Bucket, r.Object.Key}] = objEdit{exists: true, blobID: r.Object.BlobID}
	case r.Op == opDeleteObject && r.Object != nil:
		exists, _ := ov.getObject(r.Object.Bucket, r.Object.Key)
		if exists {
			ov.bucketDelta[r.Object.Bucket]--
		}
		ov.objEdits[objKey{r.Object.Bucket, r.Object.Key}] = objEdit{exists: false}
	case r.Op == opCreateUpload && r.Upload != nil:
		u := *r.Upload
		u.Metadata = maps.Clone(u.Metadata)
		ov.uploadEdits[u.ID] = &uploadState{info: u, parts: map[int]domain.Part{}}
	case r.Op == opPutPart && r.Part != nil:
		u := ov.getUpload(r.Part.UploadID)
		if u != nil {
			u.parts[r.Part.Number] = *r.Part
			if r.Part.ModifiedAt.After(u.info.LastActiveAt) {
				u.info.LastActiveAt = r.Part.ModifiedAt
			}
		}
	case r.Op == opAbortUpload && r.Upload != nil:
		ov.uploadEdits[r.Upload.ID] = nil
	case r.Op == opCompleteUpload && r.Upload != nil && r.Object != nil && r.Completed != nil:
		exists, _ := ov.getObject(r.Object.Bucket, r.Object.Key)
		if !exists {
			ov.bucketDelta[r.Object.Bucket]++
		}
		ov.objEdits[objKey{r.Object.Bucket, r.Object.Key}] = objEdit{exists: true, blobID: r.Object.BlobID}
		ov.uploadEdits[r.Upload.ID] = nil
		c := *r.Completed
		ov.completionEdits[c.UploadID] = &c
	case r.Op == opSaveCompletion && r.Completed != nil:
		c := *r.Completed
		ov.completionEdits[c.UploadID] = &c
	case r.Op == opPurgeCompletion && r.Completed != nil:
		ov.completionEdits[r.Completed.UploadID] = nil
	}
}
