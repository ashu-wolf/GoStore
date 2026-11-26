//go:build linux

package storage

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"slices"
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

func TestBucketService(t *testing.T) {
	e, _ := openEngine(t, t.TempDir())
	if err := e.CreateBucket("Bad_Name"); !errors.Is(err, domain.ErrInvalidBucketName) {
		t.Fatalf("got %v", err)
	}
	for _, name := range []string{"videos", "archive", "photos"} {
		if err := e.CreateBucket(name); err != nil {
			t.Fatal(err)
		}
	}
	if err := e.CreateBucket("photos"); !errors.Is(err, domain.ErrBucketExists) {
		t.Fatalf("got %v", err)
	}
	buckets := e.ListBuckets()
	var names []string
	for _, b := range buckets {
		names = append(names, b.Name)
	}
	if !slices.Equal(names, []string{"archive", "photos", "videos"}) {
		t.Fatalf("got %v", names)
	}

	if err := e.DeleteBucket("missing"); !errors.Is(err, domain.ErrNoSuchBucket) {
		t.Fatalf("got %v", err)
	}
	if _, err := putString(e, "a.jpg", "data"); err != nil {
		t.Fatal(err)
	}
	if err := e.DeleteBucket("photos"); !errors.Is(err, domain.ErrBucketNotEmpty) {
		t.Fatalf("got %v", err)
	}
	if err := e.DeleteObject("photos", "a.jpg"); err != nil {
		t.Fatal(err)
	}
	if err := e.DeleteBucket("photos"); err != nil {
		t.Fatal(err)
	}
}

func TestObjectHeadOpenDelete(t *testing.T) {
	e, _ := openEngine(t, t.TempDir())
	if err := e.CreateBucket("photos"); err != nil {
		t.Fatal(err)
	}

	if _, err := e.HeadObject("missing", "a"); !errors.Is(err, domain.ErrNoSuchBucket) {
		t.Fatalf("got %v", err)
	}
	if _, err := e.HeadObject("photos", "missing"); !errors.Is(err, domain.ErrNoSuchKey) {
		t.Fatalf("got %v", err)
	}
	if _, _, err := e.OpenObject("photos", "missing"); !errors.Is(err, domain.ErrNoSuchKey) {
		t.Fatalf("got %v", err)
	}
	if err := e.DeleteObject("missing", "a"); !errors.Is(err, domain.ErrNoSuchBucket) {
		t.Fatalf("got %v", err)
	}
	// Deleting a missing object in an existing bucket must succeed.
	if err := e.DeleteObject("photos", "missing"); err != nil {
		t.Fatalf("got %v", err)
	}

	obj, err := e.PutObject(context.Background(), PutRequest{
		Bucket:      "photos",
		Key:         "a.txt",
		ContentType: "text/plain",
		Metadata:    map[string]string{"owner": "ashu"},
	}, strings.NewReader("hello"))
	if err != nil {
		t.Fatal(err)
	}
	head, err := e.HeadObject("photos", "a.txt")
	if err != nil {
		t.Fatal(err)
	}
	if head.Size != 5 || head.ETag != obj.ETag || head.SHA256 != obj.SHA256 ||
		head.ContentType != "text/plain" || head.Metadata["owner"] != "ashu" {
		t.Fatalf("unexpected head: %+v", head)
	}
	// Mutating the returned metadata map must not change stored metadata.
	head.Metadata["owner"] = "tampered"
	head2, err := e.HeadObject("photos", "a.txt")
	if err != nil || head2.Metadata["owner"] != "ashu" {
		t.Fatalf("metadata mutated: %+v, %v", head2, err)
	}

	// Overwrite updates metadata and content.
	if _, err := e.PutObject(context.Background(), PutRequest{
		Bucket:      "photos",
		Key:         "a.txt",
		ContentType: "application/octet-stream",
		Metadata:    map[string]string{"version": "2"},
	}, strings.NewReader("world!")); err != nil {
		t.Fatal(err)
	}
	head3, r, err := e.OpenObject("photos", "a.txt")
	if err != nil {
		t.Fatal(err)
	}
	body, err := io.ReadAll(r)
	_ = r.Close()
	if err != nil || string(body) != "world!" || head3.Size != 6 ||
		head3.ContentType != "application/octet-stream" || head3.Metadata["version"] != "2" {
		t.Fatalf("got %q, %+v, %v", body, head3, err)
	}

	// Missing blob makes HeadObject return ErrDamaged.
	if err := e.blobs.Delete(head3.BlobID); err != nil {
		t.Fatal(err)
	}
	if _, err := e.HeadObject("photos", "a.txt"); !errors.Is(err, ErrDamaged) {
		t.Fatalf("got %v", err)
	}
}

func TestOpenReaderSurvivesDeleteAndOverwrite(t *testing.T) {
	e, _ := openEngine(t, t.TempDir())
	if err := e.CreateBucket("photos"); err != nil {
		t.Fatal(err)
	}
	if _, err := putString(e, "a", "original-data"); err != nil {
		t.Fatal(err)
	}
	_, r, err := e.OpenObject("photos", "a")
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()

	// Overwrite and then delete while the file descriptor is open.
	if _, err := putString(e, "a", "new-data"); err != nil {
		t.Fatal(err)
	}
	if err := e.DeleteObject("photos", "a"); err != nil {
		t.Fatal(err)
	}

	data, err := io.ReadAll(r)
	if err != nil || string(data) != "original-data" {
		t.Fatalf("got %q, %v", data, err)
	}
}

func TestLookupRetriesWhenBlobRemoved(t *testing.T) {
	t.Run("open sees overwrite", func(t *testing.T) {
		e, _ := openEngine(t, t.TempDir())
		if err := e.CreateBucket("photos"); err != nil {
			t.Fatal(err)
		}
		if _, err := putString(e, "a", "first"); err != nil {
			t.Fatal(err)
		}
		calls := 0
		e.onLookup = func() {
			calls++
			if calls == 1 {
				if _, err := putString(e, "a", "second"); err != nil {
					t.Error(err)
				}
			}
		}
		_, r, err := e.OpenObject("photos", "a")
		if err != nil {
			t.Fatal(err)
		}
		defer r.Close()
		data, err := io.ReadAll(r)
		if err != nil || string(data) != "second" || calls != 2 {
			t.Fatalf("got %q, calls=%d, err=%v", data, calls, err)
		}
	})

	t.Run("open sees delete", func(t *testing.T) {
		e, _ := openEngine(t, t.TempDir())
		if err := e.CreateBucket("photos"); err != nil {
			t.Fatal(err)
		}
		if _, err := putString(e, "a", "first"); err != nil {
			t.Fatal(err)
		}
		calls := 0
		e.onLookup = func() {
			calls++
			if calls == 1 {
				if err := e.DeleteObject("photos", "a"); err != nil {
					t.Error(err)
				}
			}
		}
		if _, _, err := e.OpenObject("photos", "a"); !errors.Is(err, domain.ErrNoSuchKey) {
			t.Fatalf("got %v, want ErrNoSuchKey", err)
		}
	})

	t.Run("head sees overwrite", func(t *testing.T) {
		e, _ := openEngine(t, t.TempDir())
		if err := e.CreateBucket("photos"); err != nil {
			t.Fatal(err)
		}
		if _, err := putString(e, "a", "first"); err != nil {
			t.Fatal(err)
		}
		calls := 0
		e.onLookup = func() {
			calls++
			if calls == 1 {
				if _, err := putString(e, "a", "second-v"); err != nil {
					t.Error(err)
				}
			}
		}
		obj, err := e.HeadObject("photos", "a")
		if err != nil || obj.Size != int64(len("second-v")) || calls != 2 {
			t.Fatalf("got %+v, calls=%d, err=%v", obj, calls, err)
		}
	})
}

func TestListObjectsService(t *testing.T) {
	e, _ := openEngine(t, t.TempDir())
	if _, err := e.ListObjects(domain.ListOptions{Bucket: "missing"}); !errors.Is(err, domain.ErrNoSuchBucket) {
		t.Fatalf("got %v", err)
	}
	if err := e.CreateBucket("photos"); err != nil {
		t.Fatal(err)
	}

	// Empty bucket.
	empty, err := e.ListObjects(domain.ListOptions{Bucket: "photos"})
	if err != nil || len(empty.Objects) != 0 || len(empty.CommonPrefixes) != 0 || empty.IsTruncated || empty.NextContinuationToken != "" {
		t.Fatalf("unexpected empty listing: %+v, %v", empty, err)
	}

	// Validation and token errors.
	if _, err := e.ListObjects(domain.ListOptions{Bucket: "photos", MaxKeys: -1}); !errors.Is(err, domain.ErrInvalidArgument) {
		t.Fatalf("got %v", err)
	}
	if _, err := e.ListObjects(domain.ListOptions{Bucket: "photos", Prefix: "\xff"}); !errors.Is(err, domain.ErrInvalidArgument) {
		t.Fatalf("got %v", err)
	}
	if _, err := e.ListObjects(domain.ListOptions{Bucket: "photos", Delimiter: "\xff"}); !errors.Is(err, domain.ErrInvalidArgument) {
		t.Fatalf("got %v", err)
	}
	if _, err := e.ListObjects(domain.ListOptions{Bucket: "photos", ContinuationToken: "bad"}); !errors.Is(err, domain.ErrInvalidToken) {
		t.Fatalf("got %v", err)
	}

	keys := []string{
		"a.txt",
		"dir/",
		"dir/1.txt",
		"dir/2.txt",
		"dir/sub/x.txt",
		"dir/sub/y.txt",
		"dir0/z.txt",
		"temp.txt",
	}
	for _, k := range keys {
		if _, err := putString(e, k, "v-"+k); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := putString(e, "a.txt", "v2-a.txt"); err != nil {
		t.Fatal(err)
	}
	if err := e.DeleteObject("photos", "temp.txt"); err != nil {
		t.Fatal(err)
	}

	listAll := func(opts domain.ListOptions) ([]string, []string) {
		t.Helper()
		var gotObjs, gotPrefixes []string
		for pages := 0; pages < 50; pages++ {
			res, err := e.ListObjects(opts)
			if err != nil {
				t.Fatal(err)
			}
			for _, o := range res.Objects {
				gotObjs = append(gotObjs, o.Key)
			}
			gotPrefixes = append(gotPrefixes, res.CommonPrefixes...)
			if !res.IsTruncated {
				if res.NextContinuationToken != "" {
					t.Fatal("non-truncated response had continuation token")
				}
				return gotObjs, gotPrefixes
			}
			if res.NextContinuationToken == "" {
				t.Fatal("truncated response missing continuation token")
			}
			opts.ContinuationToken = res.NextContinuationToken
		}
		t.Fatal("pagination did not terminate")
		return nil, nil
	}

	for pageSize := 1; pageSize <= len(keys); pageSize++ {
		objs, prefixes := listAll(domain.ListOptions{
			Bucket:    "photos",
			Delimiter: "/",
			MaxKeys:   pageSize,
		})
		wantObjs := []string{"a.txt"}
		wantPrefixes := []string{"dir/", "dir0/"}
		if !slices.Equal(objs, wantObjs) || !slices.Equal(prefixes, wantPrefixes) {
			t.Fatalf("pageSize=%d: got objs=%q prefixes=%q, want objs=%q prefixes=%q",
				pageSize, objs, prefixes, wantObjs, wantPrefixes)
		}

		objs, prefixes = listAll(domain.ListOptions{
			Bucket:    "photos",
			Prefix:    "dir/",
			Delimiter: "/",
			MaxKeys:   pageSize,
		})
		wantObjs = []string{"dir/", "dir/1.txt", "dir/2.txt"}
		wantPrefixes = []string{"dir/sub/"}
		if !slices.Equal(objs, wantObjs) || !slices.Equal(prefixes, wantPrefixes) {
			t.Fatalf("prefix pageSize=%d: got objs=%q prefixes=%q, want objs=%q prefixes=%q",
				pageSize, objs, prefixes, wantObjs, wantPrefixes)
		}
	}

	// Token from one query must be rejected when reused with another query.
	p1, err := e.ListObjects(domain.ListOptions{Bucket: "photos", Delimiter: "/", MaxKeys: 1})
	if err != nil || !p1.IsTruncated {
		t.Fatalf("got %+v, %v", p1, err)
	}
	if _, err := e.ListObjects(domain.ListOptions{
		Bucket:            "photos",
		Prefix:            "dir/",
		Delimiter:         "/",
		ContinuationToken: p1.NextContinuationToken,
	}); !errors.Is(err, domain.ErrInvalidToken) {
		t.Fatalf("expected ErrInvalidToken, got %v", err)
	}
}

func TestConcurrentServiceOperations(t *testing.T) {
	e, _ := openEngine(t, t.TempDir())
	if err := e.CreateBucket("photos"); err != nil {
		t.Fatal(err)
	}
	for _, k := range []string{"k1", "k2", "dir/k3", "dir/k4"} {
		if _, err := putString(e, k, "init"); err != nil {
			t.Fatal(err)
		}
	}

	var wg sync.WaitGroup
	for w := 0; w < 4; w++ {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()
			for i := 0; i < 20; i++ {
				if _, err := putString(e, "k1", "val"); err != nil {
					t.Error(err)
					return
				}
				if _, err := e.HeadObject("photos", "k1"); err != nil {
					t.Error(err)
					return
				}
				if _, err := e.ListObjects(domain.ListOptions{Bucket: "photos", Delimiter: "/", MaxKeys: 2}); err != nil {
					t.Error(err)
					return
				}
				if len(e.ListBuckets()) != 1 {
					t.Error("unexpected bucket count")
					return
				}
			}
		}(w)
	}
	wg.Wait()
}
