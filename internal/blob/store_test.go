//go:build linux

package blob

import (
	"bytes"
	"context"
	"crypto/md5"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"syscall"
	"testing"
	"testing/iotest"
)

func newStore(t *testing.T) *Store {
	t.Helper()
	s, err := Open(filepath.Join(t.TempDir(), "data"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

// fileCount returns the number of temporary and published files.
func fileCount(t *testing.T, s *Store) int {
	t.Helper()
	count := 0
	for _, dir := range []string{s.tmpDir, s.blobDir} {
		err := filepath.WalkDir(dir, func(_ string, d fs.DirEntry, err error) error {
			if err == nil && !d.IsDir() {
				count++
			}
			return err
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	return count
}

func readBlob(t *testing.T, s *Store, id string) []byte {
	t.Helper()
	r, err := s.Open(id)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	data, err := io.ReadAll(r)
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func TestPutAndOpen(t *testing.T) {
	for _, size := range []int{0, 1, bufferSize, bufferSize + 1, 5 << 20} {
		s := newStore(t)
		data := bytes.Repeat([]byte("abcdefg"), size/7+1)[:size]
		sha := sha256.Sum256(data)
		sum := md5.Sum(data)
		want := int64(size)

		info, err := s.Put(context.Background(), bytes.NewReader(data), PutOptions{
			MaxSize:    max(want, 1),
			WantSize:   &want,
			WantMD5:    hex.EncodeToString(sum[:]),
			WantSHA256: strings.ToUpper(hex.EncodeToString(sha[:])),
		})
		if err != nil {
			t.Fatalf("size %d: %v", size, err)
		}
		if info.Size != want || info.SHA256 != hex.EncodeToString(sha[:]) || info.MD5 != hex.EncodeToString(sum[:]) {
			t.Fatalf("size %d: wrong info %+v", size, info)
		}
		if !bytes.Equal(readBlob(t, s, info.ID), data) {
			t.Fatalf("size %d: data differs", size)
		}
		if n := fileCount(t, s); n != 1 {
			t.Fatalf("size %d: want 1 file, got %d", size, n)
		}
	}
}

func TestPutRejectsBadUploads(t *testing.T) {
	disconnected := io.MultiReader(strings.NewReader("partial"), iotest.ErrReader(io.ErrUnexpectedEOF))
	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	wantSize := int64(10)

	tests := []struct {
		name string
		ctx  context.Context
		body io.Reader
		opts PutOptions
		want error
	}{
		{"client disconnects", context.Background(), disconnected, PutOptions{}, io.ErrUnexpectedEOF},
		{"cancelled", cancelled, strings.NewReader("data"), PutOptions{}, context.Canceled},
		{"too large", context.Background(), strings.NewReader("12345"), PutOptions{MaxSize: 4}, ErrTooLarge},
		{"short body", context.Background(), strings.NewReader("12345"), PutOptions{WantSize: &wantSize}, ErrSizeMismatch},
		{"wrong MD5", context.Background(), strings.NewReader("data"), PutOptions{WantMD5: "00"}, ErrChecksumMismatch},
		{"wrong SHA-256", context.Background(), strings.NewReader("data"), PutOptions{WantSHA256: "00"}, ErrChecksumMismatch},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := newStore(t)
			if _, err := s.Put(tt.ctx, tt.body, tt.opts); !errors.Is(err, tt.want) {
				t.Fatalf("want %v, got %v", tt.want, err)
			}
			if n := fileCount(t, s); n != 0 {
				t.Fatalf("left %d files behind", n)
			}
		})
	}
}

var errInjected = errors.New("injected disk failure")

type faultyFile struct {
	tempFile
	failOn string
}

func (f faultyFile) Write(p []byte) (int, error) {
	if f.failOn == "write" {
		n, _ := f.tempFile.Write(p[:len(p)/2]) // partial write
		return n, errInjected
	}
	return f.tempFile.Write(p)
}

func (f faultyFile) Sync() error {
	if f.failOn == "sync" {
		return errInjected
	}
	return f.tempFile.Sync()
}

func (f faultyFile) Close() error {
	err := f.tempFile.Close()
	if f.failOn == "close" {
		return errInjected
	}
	return err
}

func TestPutDiskFailures(t *testing.T) {
	for _, step := range []string{"create", "write", "sync", "close", "rename", "syncdir"} {
		t.Run(step, func(t *testing.T) {
			s := newStore(t)
			realCreate := s.ops.createTemp
			s.ops.createTemp = func(dir string) (tempFile, error) {
				if step == "create" {
					return nil, errInjected
				}
				f, err := realCreate(dir)
				if err != nil {
					return nil, err
				}
				return faultyFile{f, step}, nil
			}
			if step == "rename" {
				s.ops.rename = func(string, string) error { return errInjected }
			}
			if step == "syncdir" {
				s.ops.syncDir = func(string) error { return errInjected }
			}

			_, err := s.Put(context.Background(), strings.NewReader("some data"), PutOptions{})
			if !errors.Is(err, errInjected) {
				t.Fatalf("want injected error, got %v", err)
			}
			if n := fileCount(t, s); n != 0 {
				t.Fatalf("left %d files behind", n)
			}
		})
	}
}

// zeros produces data without allocating memory.
type zeros struct{}

func (zeros) Read(p []byte) (int, error) {
	clear(p)
	return len(p), nil
}

func TestPutMemoryDoesNotGrowWithSize(t *testing.T) {
	s := newStore(t)
	const size = 64 << 20

	var before, after runtime.MemStats
	runtime.ReadMemStats(&before)
	info, err := s.Put(context.Background(), io.LimitReader(zeros{}, size), PutOptions{})
	runtime.ReadMemStats(&after)

	if err != nil || info.Size != size {
		t.Fatalf("size %d, error %v", info.Size, err)
	}
	if allocated := after.TotalAlloc - before.TotalAlloc; allocated > 1<<20 {
		t.Fatalf("allocated %d bytes for a %d byte upload", allocated, size)
	}
}

func TestSeekAndReadAfterDelete(t *testing.T) {
	s := newStore(t)
	info, err := s.Put(context.Background(), strings.NewReader("0123456789"), PutOptions{})
	if err != nil {
		t.Fatal(err)
	}
	r, err := s.Open(info.ID)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()

	for i := 0; i < 2; i++ { // the second delete checks idempotency
		if err := s.Delete(info.ID); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := r.Seek(4, io.SeekStart); err != nil {
		t.Fatal(err)
	}
	got, err := io.ReadAll(r)
	if err != nil || string(got) != "456789" {
		t.Fatalf("got %q, error %v", got, err)
	}
	if _, err := s.Open(info.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("want ErrNotFound, got %v", err)
	}
}

func TestRejectsUnsafeIDs(t *testing.T) {
	s := newStore(t)
	for _, id := range []string{"", "abc", "../../etc/passwd", strings.Repeat("A", 32), "../" + strings.Repeat("a", 29)} {
		if _, err := s.Open(id); !errors.Is(err, ErrInvalidID) {
			t.Errorf("Open(%q): got %v", id, err)
		}
		if err := s.Delete(id); !errors.Is(err, ErrInvalidID) {
			t.Errorf("Delete(%q): got %v", id, err)
		}
	}
}

func TestLockAndReopen(t *testing.T) {
	root := filepath.Join(t.TempDir(), "nested", "data")
	first, err := Open(root)
	if err != nil {
		t.Fatal(err)
	}
	info, err := first.Put(context.Background(), strings.NewReader("keep me"), PutOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Open(root); !errors.Is(err, ErrLocked) {
		t.Fatalf("want ErrLocked, got %v", err)
	}
	if err := first.Close(); err != nil {
		t.Fatal(err)
	}

	second, err := Open(root)
	if err != nil {
		t.Fatal(err)
	}
	defer second.Close()
	if got := readBlob(t, second, info.ID); string(got) != "keep me" {
		t.Fatalf("got %q", got)
	}
}

func TestPermissions(t *testing.T) {
	s := newStore(t)
	info, err := s.Put(context.Background(), strings.NewReader("secret"), PutOptions{})
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{s.tmpDir, s.blobDir, s.path(info.ID)} {
		st, err := os.Stat(p)
		if err != nil {
			t.Fatal(err)
		}
		if st.Mode().Perm()&0o077 != 0 {
			t.Errorf("%s is accessible to other users: %v", p, st.Mode().Perm())
		}
	}
}

func TestConcurrentPuts(t *testing.T) {
	s := newStore(t)
	var wg sync.WaitGroup
	for i := 0; i < 32; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			want := bytes.Repeat([]byte{byte(i)}, 100_000)
			info, err := s.Put(context.Background(), bytes.NewReader(want), PutOptions{})
			if err != nil {
				t.Error(err)
				return
			}
			r, err := s.Open(info.ID)
			if err != nil {
				t.Error(err)
				return
			}
			defer r.Close()
			if got, err := io.ReadAll(r); err != nil || !bytes.Equal(got, want) {
				t.Errorf("upload %d: data differs, error %v", i, err)
			}
		}(i)
	}
	wg.Wait()
	if n := fileCount(t, s); n != 32 {
		t.Fatalf("want 32 files, got %d", n)
	}
}

type enospcFile struct {
	tempFile
}

func (f enospcFile) Write(p []byte) (int, error) {
	return 0, syscall.ENOSPC
}

func TestPutENOSPCLeavesNoTempFiles(t *testing.T) {
	s := newStore(t)
	keep, err := s.Put(context.Background(), strings.NewReader("committed"), PutOptions{})
	if err != nil {
		t.Fatal(err)
	}

	realCreate := s.ops.createTemp
	s.ops.createTemp = func(dir string) (tempFile, error) {
		f, err := realCreate(dir)
		if err != nil {
			return nil, err
		}
		return enospcFile{f}, nil
	}
	if _, err := s.Put(context.Background(), strings.NewReader("fails"), PutOptions{}); !errors.Is(err, ErrNoSpace) || !errors.Is(err, syscall.ENOSPC) {
		t.Fatalf("want ErrNoSpace/ENOSPC, got %v", err)
	}
	if n := fileCount(t, s); n != 1 {
		t.Fatalf("want only 1 committed file, got %d", n)
	}
	if got := readBlob(t, s, keep.ID); string(got) != "committed" {
		t.Fatalf("committed blob corrupted: %q", got)
	}
}
