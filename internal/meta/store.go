//go:build linux

// Package meta stores bucket and object records.
//
// The current state is held in memory. Every change is appended to a journal and
// synced before it becomes visible. The journal is replayed at startup and
// compacted when most of its records are out of date.
//
// Limits of this design: all records must fit in memory, and writes are synced
// one at a time.
package meta

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"maps"
	"os"
	"slices"
	"sync"

	"github.com/ashu-wolf/gostore/internal/crashpoint"
	"github.com/ashu-wolf/gostore/internal/domain"
)

// Errors specific to the metadata store.
var (
	ErrCorrupt        = errors.New("metadata journal is corrupt")
	ErrMissing        = errors.New("metadata journal is missing")
	ErrRecordTooLarge = errors.New("metadata record is too large")
	// ErrCommitUnknown means a write or sync failed, so the change may or may
	// not survive a restart. The store rejects further changes.
	ErrCommitUnknown = errors.New("commit result unknown; restart required")
)

// compactSlack is how many outdated records are tolerated before compaction.
const compactSlack = 1000

type bucketState struct {
	info    domain.Bucket
	objects map[string]domain.Object
}

type journalFile interface {
	io.Writer
	Sync() error
	Close() error
}

// OpenReport describes what Open had to repair.
type OpenReport struct {
	DiscardedBytes int
	Compacted      bool
}

// Store is safe for concurrent use.
type Store struct {
	writeMu sync.Mutex // orders changes; held while syncing
	journal journalFile
	failed  error

	mu      sync.RWMutex // protects buckets; never held while syncing
	buckets map[string]*bucketState
}

// Open loads the journal at path. If the file does not exist, it is created only
// when allowCreate is true.
func Open(path string, allowCreate bool) (*Store, OpenReport, error) {
	var report OpenReport
	data, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		if !allowCreate {
			return nil, report, ErrMissing
		}
		if err := writeJournal(path, nil); err != nil {
			return nil, report, err
		}
		data = []byte(fileHeader)
	} else if err != nil {
		return nil, report, err
	}

	s := &Store{buckets: map[string]*bucketState{}}
	good, count, err := replay(data, s.apply)
	if err != nil {
		return nil, report, err
	}
	report.DiscardedBytes = len(data) - good

	live := s.snapshot()
	if count > 2*len(live)+compactSlack {
		if err := writeJournal(path, live); err != nil {
			return nil, report, fmt.Errorf("compact journal: %w", err)
		}
		report.Compacted = true
	}

	f, err := os.OpenFile(path, os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return nil, report, err
	}
	if report.DiscardedBytes > 0 && !report.Compacted {
		// Remove the unfinished record so new records do not follow it.
		if err := errors.Join(f.Truncate(int64(good)), f.Sync()); err != nil {
			_ = f.Close()
			return nil, report, fmt.Errorf("truncate journal: %w", err)
		}
	}
	s.journal = f
	return s, report, nil
}

// Close closes the journal.
func (s *Store) Close() error {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	return s.journal.Close()
}

// commit appends, syncs and then applies a record. The caller holds writeMu.
func (s *Store) commit(r record) error {
	if s.failed != nil {
		return s.failed
	}
	buf, err := encode(r)
	if err != nil {
		return err
	}
	if _, err := s.journal.Write(buf); err != nil {
		s.failed = fmt.Errorf("%w: %w", ErrCommitUnknown, err)
		return s.failed
	}
	crashpoint.Hit("meta.after-write")
	if err := s.journal.Sync(); err != nil {
		s.failed = fmt.Errorf("%w: %w", ErrCommitUnknown, err)
		return s.failed
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.apply(r)
}

// apply changes the in-memory state. It rejects changes that do not fit it.
func (s *Store) apply(r record) error {
	switch {
	case r.Op == opCreateBucket && r.Bucket != nil:
		if s.buckets[r.Bucket.Name] != nil {
			return domain.ErrBucketExists
		}
		s.buckets[r.Bucket.Name] = &bucketState{info: *r.Bucket, objects: map[string]domain.Object{}}
	case r.Op == opDeleteBucket && r.Bucket != nil:
		b := s.buckets[r.Bucket.Name]
		if b == nil {
			return domain.ErrNoSuchBucket
		}
		if len(b.objects) > 0 {
			return domain.ErrBucketNotEmpty
		}
		delete(s.buckets, r.Bucket.Name)
	case (r.Op == opPutObject || r.Op == opDeleteObject) && r.Object != nil:
		b := s.buckets[r.Object.Bucket]
		if b == nil {
			return domain.ErrNoSuchBucket
		}
		if r.Op == opPutObject {
			b.objects[r.Object.Key] = *r.Object
		} else {
			delete(b.objects, r.Object.Key)
		}
	default:
		return fmt.Errorf("unknown record %q", r.Op)
	}
	return nil
}

// snapshot returns the records needed to rebuild the current state.
func (s *Store) snapshot() []record {
	var out []record
	for _, name := range slices.Sorted(maps.Keys(s.buckets)) {
		b := s.buckets[name]
		out = append(out, record{Op: opCreateBucket, Bucket: &b.info})
		for _, key := range slices.Sorted(maps.Keys(b.objects)) {
			o := b.objects[key]
			out = append(out, record{Op: opPutObject, Object: &o})
		}
	}
	return out
}

// Only writers change the maps, and they all hold writeMu. A writer may
// therefore read the maps without taking mu.

// CreateBucket adds a bucket.
func (s *Store) CreateBucket(b domain.Bucket) error {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	if s.buckets[b.Name] != nil {
		return domain.ErrBucketExists
	}
	return s.commit(record{Op: opCreateBucket, Bucket: &b})
}

// DeleteBucket removes an empty bucket. The check and removal are atomic.
func (s *Store) DeleteBucket(name string) error {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	b := s.buckets[name]
	if b == nil {
		return domain.ErrNoSuchBucket
	}
	if len(b.objects) > 0 {
		return domain.ErrBucketNotEmpty
	}
	return s.commit(record{Op: opDeleteBucket, Bucket: &domain.Bucket{Name: name}})
}

// PutObject saves an object record. It returns the blob ID that it replaced,
// or an empty string.
func (s *Store) PutObject(o domain.Object) (oldBlobID string, err error) {
	o.Metadata = maps.Clone(o.Metadata)
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	b := s.buckets[o.Bucket]
	if b == nil {
		return "", domain.ErrNoSuchBucket
	}
	old := b.objects[o.Key].BlobID
	if err := s.commit(record{Op: opPutObject, Object: &o}); err != nil {
		return "", err
	}
	return old, nil
}

// DeleteObject removes an object record and returns its blob ID. Deleting a
// missing object succeeds and returns an empty string.
func (s *Store) DeleteObject(bucket, key string) (blobID string, err error) {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	b := s.buckets[bucket]
	if b == nil {
		return "", domain.ErrNoSuchBucket
	}
	o, ok := b.objects[key]
	if !ok {
		return "", nil
	}
	err = s.commit(record{Op: opDeleteObject, Object: &domain.Object{Bucket: bucket, Key: key}})
	if err != nil {
		return "", err
	}
	return o.BlobID, nil
}

// HasBucket reports whether a bucket exists.
func (s *Store) HasBucket(name string) bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.buckets[name] != nil
}

// Object returns a copy of an object record.
func (s *Store) Object(bucket, key string) (domain.Object, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	b := s.buckets[bucket]
	if b == nil {
		return domain.Object{}, domain.ErrNoSuchBucket
	}
	o, ok := b.objects[key]
	if !ok {
		return domain.Object{}, domain.ErrNoSuchKey
	}
	o.Metadata = maps.Clone(o.Metadata)
	return o, nil
}

// AllObjects returns every object record, for startup recovery.
func (s *Store) AllObjects() []domain.Object {
	s.mu.RLock()
	defer s.mu.RUnlock()
	var out []domain.Object
	for _, b := range s.buckets {
		for _, o := range b.objects {
			o.Metadata = maps.Clone(o.Metadata)
			out = append(out, o)
		}
	}
	return out
}
