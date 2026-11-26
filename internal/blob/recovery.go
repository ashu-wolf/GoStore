//go:build linux

package blob

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
)

// The methods in this file are for startup recovery. Call them only before the
// store is serving requests.

// RemoveTemp deletes unfinished uploads and returns how many it removed.
func (s *Store) RemoveTemp() (int, error) {
	entries, err := os.ReadDir(s.tmpDir)
	if err != nil {
		return 0, err
	}
	removed := 0
	for _, e := range entries {
		if err := os.Remove(filepath.Join(s.tmpDir, e.Name())); err != nil {
			return removed, err
		}
		removed++
	}
	return removed, nil
}

// IDs returns the ID of every published blob. Files that the store did not
// create are ignored.
func (s *Store) IDs() ([]string, error) {
	var ids []string
	err := filepath.WalkDir(s.blobDir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.Type().IsRegular() && validID(d.Name()) && s.path(d.Name()) == path {
			ids = append(ids, d.Name())
		}
		return nil
	})
	return ids, err
}

// Size returns the size of a blob in bytes.
func (s *Store) Size(id string) (int64, error) {
	if !validID(id) {
		return 0, ErrInvalidID
	}
	info, err := os.Stat(s.path(id))
	if errors.Is(err, fs.ErrNotExist) {
		return 0, ErrNotFound
	}
	if err != nil {
		return 0, err
	}
	return info.Size(), nil
}
