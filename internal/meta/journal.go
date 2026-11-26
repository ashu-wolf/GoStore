//go:build linux

package meta

import (
	"bytes"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"hash/crc32"
	"io"
	"os"
	"path/filepath"

	"github.com/ashu-wolf/gostore/internal/domain"
)

// The journal is a file header followed by records. Each record is:
//
//	4 bytes  payload length
//	4 bytes  CRC-32C of the length
//	4 bytes  CRC-32C of the payload
//	n bytes  payload (JSON)
//
// The length has its own checksum so that a damaged length is not mistaken for
// an unfinished record, which would silently drop every record after it.
const (
	fileHeader   = "GOSTORE-JOURNAL-1\n"
	recordHeader = 12
	maxRecord    = 64 * 1024
)

var crcTable = crc32.MakeTable(crc32.Castagnoli)

const (
	opCreateBucket    = "create_bucket"
	opDeleteBucket    = "delete_bucket"
	opPutObject       = "put_object"
	opDeleteObject    = "delete_object"
	opCreateUpload    = "create_upload"
	opPutPart         = "put_part"
	opAbortUpload     = "abort_upload"
	opCompleteUpload  = "complete_upload"
	opSaveCompletion  = "save_completion"
	opPurgeCompletion = "purge_completion"
)

type record struct {
	Op        string                  `json:"op"`
	Bucket    *domain.Bucket          `json:"bucket,omitempty"`
	Object    *domain.Object          `json:"object,omitempty"`
	Upload    *domain.Upload          `json:"upload,omitempty"`
	Part      *domain.Part            `json:"part,omitempty"`
	Completed *domain.CompletedUpload `json:"completed,omitempty"`
}

func encode(r record) ([]byte, error) {
	payload, err := json.Marshal(r)
	if err != nil {
		return nil, err
	}
	if len(payload) > maxRecord {
		return nil, ErrRecordTooLarge
	}
	buf := make([]byte, recordHeader+len(payload))
	binary.BigEndian.PutUint32(buf[0:4], uint32(len(payload)))
	binary.BigEndian.PutUint32(buf[4:8], crc32.Checksum(buf[0:4], crcTable))
	binary.BigEndian.PutUint32(buf[8:12], crc32.Checksum(payload, crcTable))
	copy(buf[recordHeader:], payload)
	return buf, nil
}

// replay applies every complete record in data. It returns the offset just
// after the last complete record. An unfinished record at the end is normal
// after a crash. A complete record that fails its checks is an error.
func replay(data []byte, apply func(record) error) (good int, count int, err error) {
	if !bytes.HasPrefix(data, []byte(fileHeader)) {
		return 0, 0, fmt.Errorf("%w: bad file header", ErrCorrupt)
	}
	off := len(fileHeader)
	for off < len(data) {
		rest := data[off:]
		if len(rest) < recordHeader {
			break // unfinished header
		}
		if crc32.Checksum(rest[0:4], crcTable) != binary.BigEndian.Uint32(rest[4:8]) {
			if allZero(rest) {
				break // space that was allocated but never written
			}
			return off, count, fmt.Errorf("%w: bad record length at byte %d", ErrCorrupt, off)
		}
		n := int(binary.BigEndian.Uint32(rest[0:4]))
		if n > maxRecord {
			return off, count, fmt.Errorf("%w: record too large at byte %d", ErrCorrupt, off)
		}
		if len(rest) < recordHeader+n {
			break // unfinished payload
		}
		payload := rest[recordHeader : recordHeader+n]
		if crc32.Checksum(payload, crcTable) != binary.BigEndian.Uint32(rest[8:12]) {
			return off, count, fmt.Errorf("%w: bad checksum at byte %d", ErrCorrupt, off)
		}
		var r record
		if err := json.Unmarshal(payload, &r); err != nil {
			return off, count, fmt.Errorf("%w: bad record at byte %d: %v", ErrCorrupt, off, err)
		}
		if err := apply(r); err != nil {
			return off, count, fmt.Errorf("%w: record at byte %d: %v", ErrCorrupt, off, err)
		}
		off += recordHeader + n
		count++
	}
	return off, count, nil
}

func allZero(b []byte) bool {
	for _, c := range b {
		if c != 0 {
			return false
		}
	}
	return true
}

// writeJournal atomically replaces the journal at path with the given records.
func writeJournal(path string, records []record) (err error) {
	tmp := path + ".tmp"
	f, err := os.OpenFile(tmp, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	defer func() {
		if err != nil {
			_ = f.Close()
			_ = os.Remove(tmp)
		}
	}()
	if _, err = io.WriteString(f, fileHeader); err != nil {
		return err
	}
	for _, r := range records {
		buf, encodeErr := encode(r)
		if encodeErr != nil {
			return encodeErr
		}
		if _, err = f.Write(buf); err != nil {
			return err
		}
	}
	if err = f.Sync(); err != nil {
		return err
	}
	if err = f.Close(); err != nil {
		return err
	}
	if err = os.Rename(tmp, path); err != nil {
		return err
	}
	return syncDir(filepath.Dir(path))
}

func syncDir(dir string) error {
	d, err := os.Open(dir)
	if err != nil {
		return err
	}
	return errors.Join(d.Sync(), d.Close())
}
