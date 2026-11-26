//go:build linux

// Package blob stores object data as immutable files on a Linux filesystem.
//
// It knows nothing about buckets or keys. Every blob has a random ID created
// here, so user input never becomes part of a file path.
package blob

import (
	"context"
	"crypto/md5"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"syscall"

	"github.com/ashu-wolf/gostore/internal/crashpoint"
)

const (
	bufferSize = 64 * 1024
	idBytes    = 16
	dirMode    = 0o700
)

// Errors returned by the store. Check for them with errors.Is.
var (
	ErrLocked           = errors.New("data directory is in use by another process")
	ErrInvalidID        = errors.New("invalid blob ID")
	ErrNotFound         = errors.New("blob not found")
	ErrTooLarge         = errors.New("blob is larger than the limit")
	ErrSizeMismatch     = errors.New("size does not match")
	ErrChecksumMismatch = errors.New("checksum does not match")
)

// PutOptions holds the checks applied to an upload before it is published.
type PutOptions struct {
	MaxSize    int64  // 0 means no limit
	WantSize   *int64 // nil means the size is not checked
	WantMD5    string // hex; empty means not checked
	WantSHA256 string // hex; empty means not checked
}

// Info describes a stored blob.
type Info struct {
	ID     string
	Size   int64
	MD5    string
	SHA256 string
}

type tempFile interface {
	io.Writer
	Name() string
	Sync() error
	Close() error
}

// fileOps holds the operations that tests replace to simulate disk failures.
type fileOps struct {
	createTemp func(dir string) (tempFile, error)
	rename     func(oldPath, newPath string) error
	syncDir    func(dir string) error
}

func osOps() fileOps {
	return fileOps{
		createTemp: func(dir string) (tempFile, error) {
			f, err := os.CreateTemp(dir, "upload-*") // created with mode 0600
			if err != nil {
				return nil, err
			}
			return f, nil
		},
		rename:  os.Rename,
		syncDir: syncDir,
	}
}

// Store is safe for concurrent use. Call Close only after all calls to Put,
// Open and Delete have returned.
type Store struct {
	tmpDir   string
	blobDir  string
	lockFile *os.File
	ops      fileOps
}

// Open prepares the data directory and takes an exclusive lock on it.
func Open(root string) (_ *Store, err error) {
	root, err = filepath.Abs(root)
	if err != nil {
		return nil, err
	}
	if err := mkdirAllSynced(root); err != nil {
		return nil, fmt.Errorf("create data directory: %w", err)
	}

	lock, err := lockDir(root)
	if err != nil {
		return nil, err
	}
	defer func() {
		if err != nil {
			_ = lock.Close()
		}
	}()

	s := &Store{
		tmpDir:   filepath.Join(root, "tmp"),
		blobDir:  filepath.Join(root, "blobs"),
		lockFile: lock,
		ops:      osOps(),
	}

	// All 256 shard directories are created up front, so uploads never have to
	// create (and sync) a directory.
	dirs := []string{s.tmpDir, s.blobDir}
	for i := 0; i < 256; i++ {
		dirs = append(dirs, filepath.Join(s.blobDir, fmt.Sprintf("%02x", i)))
	}
	for _, d := range dirs {
		if err := os.Mkdir(d, dirMode); err != nil && !errors.Is(err, fs.ErrExist) {
			return nil, fmt.Errorf("create %s: %w", d, err)
		}
	}
	// Sync even if the directories already existed: an earlier run may have
	// crashed between creating and syncing them.
	for _, d := range []string{s.blobDir, root, filepath.Dir(root)} {
		if err := syncDir(d); err != nil {
			return nil, err
		}
	}
	return s, nil
}

// Close releases the lock on the data directory.
func (s *Store) Close() error {
	return s.lockFile.Close()
}

// Put streams r into a new blob. On success, the data and its directory entry
// have been synced to disk. On error, Put tries to remove what it wrote.
//
// Cancelling ctx is noticed between reads. It cannot interrupt a Read that is
// already blocked; the reader (such as an HTTP request body) must do that.
func (s *Store) Put(ctx context.Context, r io.Reader, opts PutOptions) (_ Info, err error) {
	id, err := newID()
	if err != nil {
		return Info{}, err
	}

	// 1. Stream to a temporary file while counting and hashing.
	f, err := s.ops.createTemp(s.tmpDir)
	if err != nil {
		return Info{}, fmt.Errorf("create temporary file: %w", err)
	}
	closed, published := false, false
	defer func() {
		if !closed {
			_ = f.Close()
		}
		if !published {
			_ = os.Remove(f.Name())
		}
	}()

	md5Hash, shaHash := md5.New(), sha256.New()
	size, err := copyStream(ctx, io.MultiWriter(f, md5Hash, shaHash), r, opts.MaxSize)
	if err != nil {
		return Info{}, err
	}
	info := Info{
		ID:     id,
		Size:   size,
		MD5:    hex.EncodeToString(md5Hash.Sum(nil)),
		SHA256: hex.EncodeToString(shaHash.Sum(nil)),
	}

	// 2. Verify what the client claimed.
	if err := verify(info, opts); err != nil {
		return Info{}, err
	}

	// 3. Make the file contents durable.
	if err := f.Sync(); err != nil {
		return Info{}, fmt.Errorf("sync blob: %w", err)
	}
	closed = true
	if err := f.Close(); err != nil {
		return Info{}, fmt.Errorf("close blob: %w", err)
	}

	crashpoint.Hit("blob.before-rename")

	// 4. Publish with an atomic rename, then make the rename durable.
	final := s.path(id)
	if err := s.ops.rename(f.Name(), final); err != nil {
		return Info{}, fmt.Errorf("publish blob: %w", err)
	}
	published = true
	crashpoint.Hit("blob.after-rename")
	if err := s.ops.syncDir(filepath.Dir(final)); err != nil {
		_ = os.Remove(final) // best effort; startup cleanup handles leftovers
		return Info{}, fmt.Errorf("sync blob directory: %w", err)
	}
	return info, nil
}

// Open returns a blob for reading and seeking. The caller must close it.
// On Linux, an open blob stays readable even if it is deleted.
func (s *Store) Open(id string) (io.ReadSeekCloser, error) {
	if !validID(id) {
		return nil, ErrInvalidID
	}
	f, err := os.Open(s.path(id))
	if errors.Is(err, fs.ErrNotExist) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	return f, nil
}

// Delete removes a blob. Deleting a missing blob succeeds. The removal is not
// synced: after a crash the file may reappear as unused data, which startup
// cleanup will remove.
func (s *Store) Delete(id string) error {
	if !validID(id) {
		return ErrInvalidID
	}
	if err := os.Remove(s.path(id)); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	return nil
}

func (s *Store) path(id string) string {
	return filepath.Join(s.blobDir, id[:2], id)
}

// copyStream copies src to dst through one fixed-size buffer.
func copyStream(ctx context.Context, dst io.Writer, src io.Reader, maxSize int64) (int64, error) {
	buf := make([]byte, bufferSize)
	var total int64
	for {
		if err := ctx.Err(); err != nil {
			return total, err
		}
		n, readErr := src.Read(buf)
		if n > 0 {
			if maxSize > 0 && int64(n) > maxSize-total {
				return total, ErrTooLarge
			}
			written, err := dst.Write(buf[:n])
			total += int64(written)
			if err != nil {
				return total, fmt.Errorf("write blob: %w", err)
			}
		}
		if readErr == io.EOF {
			return total, nil
		}
		if readErr != nil {
			return total, fmt.Errorf("read upload: %w", readErr)
		}
	}
}

func verify(info Info, opts PutOptions) error {
	if opts.WantSize != nil && *opts.WantSize != info.Size {
		return fmt.Errorf("%w: want %d bytes, got %d", ErrSizeMismatch, *opts.WantSize, info.Size)
	}
	if opts.WantMD5 != "" && !strings.EqualFold(opts.WantMD5, info.MD5) {
		return fmt.Errorf("%w: MD5", ErrChecksumMismatch)
	}
	if opts.WantSHA256 != "" && !strings.EqualFold(opts.WantSHA256, info.SHA256) {
		return fmt.Errorf("%w: SHA-256", ErrChecksumMismatch)
	}
	return nil
}

func newID() (string, error) {
	var b [idBytes]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", fmt.Errorf("create blob ID: %w", err)
	}
	return hex.EncodeToString(b[:]), nil
}

// validID accepts only the format made by newID, which blocks path traversal.
func validID(id string) bool {
	if len(id) != idBytes*2 {
		return false
	}
	for i := 0; i < len(id); i++ {
		c := id[i]
		if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'f') {
			return false
		}
	}
	return true
}

// lockDir uses an advisory flock, which the kernel releases if the process dies.
func lockDir(root string) (*os.File, error) {
	f, err := os.OpenFile(filepath.Join(root, "LOCK"), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, fmt.Errorf("open lock file: %w", err)
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		_ = f.Close()
		if errors.Is(err, syscall.EWOULDBLOCK) {
			return nil, ErrLocked
		}
		return nil, fmt.Errorf("lock data directory: %w", err)
	}
	return f, nil
}

// mkdirAllSynced is like os.MkdirAll, but syncs the parent of each new directory.
func mkdirAllSynced(dir string) error {
	info, err := os.Stat(dir)
	if err == nil {
		if !info.IsDir() {
			return fmt.Errorf("%s is not a directory", dir)
		}
		return nil
	}
	if !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	parent := filepath.Dir(dir)
	if err := mkdirAllSynced(parent); err != nil {
		return err
	}
	if err := os.Mkdir(dir, dirMode); err != nil && !errors.Is(err, fs.ErrExist) {
		return err
	}
	return syncDir(parent)
}

// syncDir flushes a directory, so that new names in it survive a power loss.
func syncDir(dir string) error {
	d, err := os.Open(dir)
	if err != nil {
		return fmt.Errorf("open directory for sync: %w", err)
	}
	syncErr := d.Sync()
	closeErr := d.Close()
	if err := errors.Join(syncErr, closeErr); err != nil {
		return fmt.Errorf("sync directory %s: %w", dir, err)
	}
	return nil
}
