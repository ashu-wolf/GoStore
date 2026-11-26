//go:build linux

package meta

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/ashu-wolf/gostore/internal/domain"
)

func open(t *testing.T, path string) (*Store, OpenReport) {
	t.Helper()
	s, report, err := Open(path, true)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s, report
}

func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

func put(t *testing.T, s *Store, key, blobID string) string {
	t.Helper()
	old, err := s.PutObject(domain.Object{Bucket: "photos", Key: key, BlobID: blobID})
	must(t, err)
	return old
}

// seed creates a journal with one bucket and two objects.
func seed(t *testing.T) (path string, sizeBeforeLast int) {
	t.Helper()
	path = filepath.Join(t.TempDir(), "meta.journal")
	s, _ := open(t, path)
	must(t, s.CreateBucket(domain.Bucket{Name: "photos"}))
	put(t, s, "a", "1")
	info, err := os.Stat(path)
	must(t, err)
	put(t, s, "b", "2")
	must(t, s.Close())
	return path, int(info.Size())
}

func TestStateSurvivesReopen(t *testing.T) {
	path := filepath.Join(t.TempDir(), "meta.journal")
	s, _ := open(t, path)
	must(t, s.CreateBucket(domain.Bucket{Name: "photos"}))
	if err := s.CreateBucket(domain.Bucket{Name: "photos"}); !errors.Is(err, domain.ErrBucketExists) {
		t.Fatalf("got %v", err)
	}
	put(t, s, "a", "1")
	if old := put(t, s, "a", "2"); old != "1" {
		t.Fatalf("old blob = %q", old)
	}
	put(t, s, "gone", "3")
	_, err := s.DeleteObject("photos", "gone")
	must(t, err)
	if err := s.DeleteBucket("photos"); !errors.Is(err, domain.ErrBucketNotEmpty) {
		t.Fatalf("got %v", err)
	}
	must(t, s.Close())

	s, _ = open(t, path)
	if o, err := s.Object("photos", "a"); err != nil || o.BlobID != "2" {
		t.Fatalf("got %+v, %v", o, err)
	}
	if _, err := s.Object("photos", "gone"); !errors.Is(err, domain.ErrNoSuchKey) {
		t.Fatalf("got %v", err)
	}
	_, err = s.DeleteObject("photos", "a")
	must(t, err)
	must(t, s.DeleteBucket("photos"))
	if _, err := s.PutObject(domain.Object{Bucket: "photos", Key: "a"}); !errors.Is(err, domain.ErrNoSuchBucket) {
		t.Fatalf("got %v", err)
	}
}

// Every possible cut inside the last record must drop only that record.
func TestUnfinishedTailAtEveryByte(t *testing.T) {
	path, keep := seed(t)
	data, err := os.ReadFile(path)
	must(t, err)

	for cut := keep; cut < len(data); cut++ {
		must(t, os.WriteFile(path, data[:cut], 0o600))
		s, report := open(t, path)
		if report.DiscardedBytes != cut-keep {
			t.Fatalf("cut %d: discarded %d", cut, report.DiscardedBytes)
		}
		if _, err := s.Object("photos", "a"); err != nil {
			t.Fatalf("cut %d: lost earlier record: %v", cut, err)
		}
		if _, err := s.Object("photos", "b"); !errors.Is(err, domain.ErrNoSuchKey) {
			t.Fatalf("cut %d: unfinished record is visible", cut)
		}
		// New writes after the repair must be readable.
		put(t, s, "c", "3")
		must(t, s.Close())
		s, _ = open(t, path)
		if _, err := s.Object("photos", "c"); err != nil {
			t.Fatalf("cut %d: %v", cut, err)
		}
		must(t, s.Close())
	}
}

func TestZeroFilledTail(t *testing.T) {
	path, _ := seed(t)
	data, err := os.ReadFile(path)
	must(t, err)
	must(t, os.WriteFile(path, append(data, make([]byte, 100)...), 0o600))
	_, report := open(t, path)
	if report.DiscardedBytes != 100 {
		t.Fatalf("discarded %d", report.DiscardedBytes)
	}
}

func TestCorruptionStopsStartup(t *testing.T) {
	path, keep := seed(t)
	data, err := os.ReadFile(path)
	must(t, err)

	tests := map[string]int{
		"file header":         0,
		"length mid-file":     len(fileHeader),
		"payload mid-file":    len(fileHeader) + recordHeader + 2,
		"length of last":      keep + 1,
		"payload of last":     len(data) - 2,
		"checksum of payload": keep + 9,
	}
	for name, offset := range tests {
		t.Run(name, func(t *testing.T) {
			bad := append([]byte(nil), data...)
			bad[offset] ^= 0xff
			must(t, os.WriteFile(path, bad, 0o600))
			if _, _, err := Open(path, true); !errors.Is(err, ErrCorrupt) {
				t.Fatalf("want ErrCorrupt, got %v", err)
			}
			after, err := os.ReadFile(path)
			must(t, err)
			if string(after) != string(bad) {
				t.Fatal("a corrupt journal must not be modified")
			}
		})
	}
}

func TestMissingJournal(t *testing.T) {
	if _, _, err := Open(filepath.Join(t.TempDir(), "meta.journal"), false); !errors.Is(err, ErrMissing) {
		t.Fatalf("got %v", err)
	}
}

func TestCompaction(t *testing.T) {
	path := filepath.Join(t.TempDir(), "meta.journal")
	must(t, writeJournal(path, history(compactSlack+10)))
	before, err := os.Stat(path)
	must(t, err)

	s, report := open(t, path)
	if !report.Compacted {
		t.Fatal("expected compaction")
	}
	put(t, s, "after", "x")
	must(t, s.Close())

	s, report = open(t, path)
	after, err := os.Stat(path)
	must(t, err)
	if report.Compacted || after.Size() >= before.Size() {
		t.Fatalf("sizes %d -> %d", before.Size(), after.Size())
	}
	for _, key := range []string{"same", "after"} {
		if _, err := s.Object("photos", key); err != nil {
			t.Fatal(err)
		}
	}
}

// history builds a journal in which one object is overwritten many times.
func history(n int) []record {
	out := []record{{Op: opCreateBucket, Bucket: &domain.Bucket{Name: "photos"}}}
	for i := 0; i < n; i++ {
		out = append(out, record{Op: opPutObject, Object: &domain.Object{Bucket: "photos", Key: "same"}})
	}
	return out
}

type failingJournal struct {
	journalFile
}

func (failingJournal) Sync() error { return errors.New("injected sync failure") }

func TestSyncFailureStopsFurtherWrites(t *testing.T) {
	s, _ := open(t, filepath.Join(t.TempDir(), "meta.journal"))
	must(t, s.CreateBucket(domain.Bucket{Name: "photos"}))
	real := s.journal
	s.journal = failingJournal{real}

	_, err := s.PutObject(domain.Object{Bucket: "photos", Key: "a"})
	if !errors.Is(err, ErrCommitUnknown) {
		t.Fatalf("got %v", err)
	}
	if _, err := s.Object("photos", "a"); !errors.Is(err, domain.ErrNoSuchKey) {
		t.Fatal("unconfirmed change is visible")
	}
	s.journal = real
	if err := s.CreateBucket(domain.Bucket{Name: "videos"}); !errors.Is(err, ErrCommitUnknown) {
		t.Fatalf("got %v", err)
	}
}

func FuzzReplay(f *testing.F) {
	valid := []byte(fileHeader)
	for _, r := range history(2) {
		buf, err := encode(r)
		if err != nil {
			f.Fatal(err)
		}
		valid = append(valid, buf...)
	}
	f.Add(valid)
	f.Add([]byte(fileHeader))
	f.Fuzz(func(t *testing.T, data []byte) {
		s := &Store{buckets: map[string]*bucketState{}}
		good, _, err := replay(data, s.apply)
		if err == nil && (good < len(fileHeader) || good > len(data)) {
			t.Fatalf("bad offset %d for %d bytes", good, len(data))
		}
	})
}
