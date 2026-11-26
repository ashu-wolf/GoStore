//go:build linux

package storage

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/ashu-wolf/gostore/internal/domain"
	"github.com/ashu-wolf/gostore/internal/meta"
)

func openEngine(t *testing.T, root string) (*Engine, Report) {
	t.Helper()
	e, report, err := Open(root)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = e.Close() })
	return e, report
}

func putString(e *Engine, key, value string) (domain.Object, error) {
	return e.PutObject(context.Background(), PutRequest{Bucket: "photos", Key: key}, strings.NewReader(value))
}

func getString(t *testing.T, e *Engine, key string) string {
	t.Helper()
	_, r, err := e.OpenObject("photos", key)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	data, err := io.ReadAll(r)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

func blobCount(t *testing.T, e *Engine) int {
	t.Helper()
	ids, err := e.blobs.IDs()
	if err != nil {
		t.Fatal(err)
	}
	return len(ids)
}

func TestObjectLifecycle(t *testing.T) {
	root := t.TempDir()
	e, _ := openEngine(t, root)
	if err := e.CreateBucket("photos"); err != nil {
		t.Fatal(err)
	}
	for _, v := range []string{"first", "second"} {
		if _, err := putString(e, "../../etc/passwd", v); err != nil {
			t.Fatal(err)
		}
	}
	if blobCount(t, e) != 1 {
		t.Fatal("old blob was not removed")
	}
	if err := e.Close(); err != nil {
		t.Fatal(err)
	}
	e, _ = openEngine(t, root)
	if got := getString(t, e, "../../etc/passwd"); got != "second" {
		t.Fatalf("got %q", got)
	}
	for i := 0; i < 2; i++ {
		if err := e.DeleteObject("photos", "../../etc/passwd"); err != nil {
			t.Fatal(err)
		}
	}
	if blobCount(t, e) != 0 {
		t.Fatal("blob was not removed")
	}
	if err := e.DeleteBucket("photos"); err != nil {
		t.Fatal(err)
	}
}

func TestRejectedPutLeavesNothing(t *testing.T) {
	e, _ := openEngine(t, t.TempDir())
	if _, err := putString(e, "a", "data"); !errors.Is(err, domain.ErrNoSuchBucket) {
		t.Fatalf("got %v", err)
	}
	if blobCount(t, e) != 0 {
		t.Fatal("left a blob")
	}
}

// deleteBucketOnEOF deletes the bucket while an upload is streaming.
type deleteBucketOnEOF struct {
	e *Engine
	t *testing.T
}

func (d deleteBucketOnEOF) Read([]byte) (int, error) {
	if err := d.e.DeleteBucket("photos"); err != nil {
		d.t.Error(err)
	}
	return 0, io.EOF
}

func TestBucketDeletedDuringUpload(t *testing.T) {
	e, _ := openEngine(t, t.TempDir())
	if err := e.CreateBucket("photos"); err != nil {
		t.Fatal(err)
	}
	_, err := e.PutObject(context.Background(), PutRequest{Bucket: "photos", Key: "a"}, deleteBucketOnEOF{e, t})
	if !errors.Is(err, domain.ErrNoSuchBucket) {
		t.Fatalf("got %v", err)
	}
	if blobCount(t, e) != 0 {
		t.Fatal("left a blob")
	}
}

func TestDamagedAndMissingJournal(t *testing.T) {
	root := t.TempDir()
	e, _ := openEngine(t, root)
	if err := e.CreateBucket("photos"); err != nil {
		t.Fatal(err)
	}
	obj, err := putString(e, "a", "data")
	if err != nil {
		t.Fatal(err)
	}
	if err := e.blobs.Delete(obj.BlobID); err != nil {
		t.Fatal(err)
	}
	if _, _, err := e.OpenObject("photos", "a"); !errors.Is(err, ErrDamaged) {
		t.Fatalf("got %v", err)
	}
	if _, err := putString(e, "b", "keep"); err != nil {
		t.Fatal(err)
	}
	if err := e.Close(); err != nil {
		t.Fatal(err)
	}

	e, report := openEngine(t, root)
	if len(report.Damaged) != 1 || report.Damaged[0] != "photos/a" {
		t.Fatalf("report %+v", report)
	}
	if err := e.Close(); err != nil {
		t.Fatal(err)
	}

	// Losing the journal must stop startup and keep the blobs.
	journal := filepath.Join(root, "meta.journal")
	if err := os.Rename(journal, journal+".bak"); err != nil {
		t.Fatal(err)
	}
	if _, _, err := Open(root); !errors.Is(err, meta.ErrMissing) {
		t.Fatalf("got %v", err)
	}
	if err := os.Rename(journal+".bak", journal); err != nil {
		t.Fatal(err)
	}
	e, _ = openEngine(t, root)
	if got := getString(t, e, "b"); got != "keep" {
		t.Fatalf("got %q", got)
	}
}

func TestReadsDuringOverwrites(t *testing.T) {
	e, _ := openEngine(t, t.TempDir())
	if err := e.CreateBucket("photos"); err != nil {
		t.Fatal(err)
	}
	if _, err := putString(e, "a", "value"); err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	for w := 0; w < 4; w++ {
		wg.Add(2)
		go func() {
			defer wg.Done()
			for i := 0; i < 25; i++ {
				if _, err := putString(e, "a", "value"); err != nil {
					t.Error(err)
					return
				}
			}
		}()
		go func() {
			defer wg.Done()
			for i := 0; i < 100; i++ {
				_, r, err := e.OpenObject("photos", "a")
				if err != nil {
					t.Error(err)
					return
				}
				data, err := io.ReadAll(r)
				_ = r.Close()
				if err != nil || string(data) != "value" {
					t.Errorf("got %q, %v", data, err)
					return
				}
			}
		}()
	}
	wg.Wait()
	if blobCount(t, e) != 1 {
		t.Fatal("unused blobs remain")
	}
}
