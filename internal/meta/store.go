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
	"strings"
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
	keys    []string // sorted in UTF-8 byte order
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
			if _, exists := b.objects[r.Object.Key]; !exists {
				i, _ := slices.BinarySearch(b.keys, r.Object.Key)
				b.keys = slices.Insert(b.keys, i, r.Object.Key)
			}
			b.objects[r.Object.Key] = *r.Object
		} else {
			if _, exists := b.objects[r.Object.Key]; exists {
				if i, found := slices.BinarySearch(b.keys, r.Object.Key); found {
					b.keys = slices.Delete(b.keys, i, i+1)
				}
				delete(b.objects, r.Object.Key)
			}
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
		for _, key := range b.keys {
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

// ListBuckets returns all buckets sorted by name.
func (s *Store) ListBuckets() []domain.Bucket {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]domain.Bucket, 0, len(s.buckets))
	for _, b := range s.buckets {
		out = append(out, b.info)
	}
	slices.SortFunc(out, func(a, b domain.Bucket) int {
		return strings.Compare(a.Name, b.Name)
	})
	return out
}

// ListPage holds one page of a bucket listing from metadata.
type ListPage struct {
	Objects        []domain.Object
	CommonPrefixes []string
	LastItem       string
	Truncated      bool
}

// ListObjects lists up to maxKeys entries (objects and common prefixes) in
// UTF-8 byte order by seeking into the sorted key index.
func (s *Store) ListObjects(bucket, prefix, delimiter, after string, afterIsPrefix bool, maxKeys int) (ListPage, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	b := s.buckets[bucket]
	if b == nil {
		return ListPage{}, domain.ErrNoSuchBucket
	}
	if maxKeys <= 0 || len(b.keys) == 0 {
		return ListPage{}, nil
	}

	i, ok := startAt(b.keys, prefix, after, afterIsPrefix)
	if !ok {
		return ListPage{}, nil
	}

	var page ListPage
	count := 0
	for i < len(b.keys) {
		key := b.keys[i]
		if prefix != "" && !strings.HasPrefix(key, prefix) {
			break
		}

		if delimiter != "" {
			rest := key[len(prefix):]
			if sep := strings.Index(rest, delimiter); sep >= 0 {
				cp := key[:len(prefix)+sep+len(delimiter)]
				if count == maxKeys {
					page.Truncated = true
					break
				}
				page.CommonPrefixes = append(page.CommonPrefixes, cp)
				page.LastItem = cp
				count++

				end, hasEnd := nextPrefix(cp)
				if !hasEnd {
					break
				}
				nextIdx, _ := slices.BinarySearch(b.keys[i+1:], end)
				i = i + 1 + nextIdx
				continue
			}
		}

		if count == maxKeys {
			page.Truncated = true
			break
		}
		obj := b.objects[key]
		obj.Metadata = maps.Clone(obj.Metadata)
		page.Objects = append(page.Objects, obj)
		page.LastItem = key
		count++
		i++
	}
	if !page.Truncated {
		page.LastItem = ""
	}
	return page, nil
}

func startAt(keys []string, prefix, after string, afterIsPrefix bool) (int, bool) {
	if after == "" || after < prefix {
		i, _ := slices.BinarySearch(keys, prefix)
		return i, true
	}
	if afterIsPrefix {
		end, ok := nextPrefix(after)
		if !ok {
			return 0, false
		}
		i, _ := slices.BinarySearch(keys, end)
		return i, true
	}
	i, found := slices.BinarySearch(keys, after)
	if found {
		i++
	}
	return i, true
}

// nextPrefix returns the smallest string strictly greater than every string
// starting with p, or ("", false) when p consists only of 0xff bytes.
func nextPrefix(p string) (string, bool) {
	b := []byte(p)
	for i := len(b) - 1; i >= 0; i-- {
		if b[i] < 0xff {
			b[i]++
			return string(b[:i+1]), true
		}
	}
	return "", false
}
