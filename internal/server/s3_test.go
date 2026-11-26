//go:build linux

package server

import (
	"bytes"
	"crypto/md5"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/xml"
	"errors"
	"io"
	"io/fs"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ashu-wolf/gostore/internal/storage"
)

func doReq(t *testing.T, method, url string, body io.Reader, headers map[string]string) (*http.Response, []byte) {
	t.Helper()
	req, err := http.NewRequest(method, url, body)
	if err != nil {
		t.Fatal(err)
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	return resp, data
}

func decodeErrorXML(t *testing.T, data []byte) errorXML {
	t.Helper()
	var out errorXML
	if err := xml.Unmarshal(data, &out); err != nil {
		t.Fatalf("unmarshal error xml %q: %v", data, err)
	}
	return out
}

func TestBucketsAndObjectsHTTP(t *testing.T) {
	ts, _, _ := newTestServer(t)

	// Empty service listing.
	resp, body := doReq(t, http.MethodGet, ts.URL+"/", nil, nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status=%d body=%s", resp.StatusCode, body)
	}
	var lb listAllMyBucketsXML
	if err := xml.Unmarshal(body, &lb); err != nil {
		t.Fatal(err)
	}
	if len(lb.Buckets.Bucket) != 0 {
		t.Fatalf("expected 0 buckets, got %+v", lb.Buckets.Bucket)
	}

	// Create buckets, check invalid and duplicate names.
	resp, body = doReq(t, http.MethodPut, ts.URL+"/Bad_Name", nil, nil)
	if resp.StatusCode != http.StatusBadRequest || decodeErrorXML(t, body).Code != "InvalidBucketName" {
		t.Fatalf("status=%d body=%s", resp.StatusCode, body)
	}
	for _, name := range []string{"videos", "photos"} {
		resp, body = doReq(t, http.MethodPut, ts.URL+"/"+name, nil, nil)
		if resp.StatusCode != http.StatusOK || resp.Header.Get("Location") != "/"+name {
			t.Fatalf("create %s: status=%d location=%q body=%s", name, resp.StatusCode, resp.Header.Get("Location"), body)
		}
	}
	resp, body = doReq(t, http.MethodPut, ts.URL+"/photos", nil, nil)
	if resp.StatusCode != http.StatusConflict || decodeErrorXML(t, body).Code != "BucketAlreadyOwnedByYou" {
		t.Fatalf("duplicate create: status=%d body=%s", resp.StatusCode, body)
	}

	resp, body = doReq(t, http.MethodGet, ts.URL+"/", nil, nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status=%d", resp.StatusCode)
	}
	if err := xml.Unmarshal(body, &lb); err != nil {
		t.Fatal(err)
	}
	if len(lb.Buckets.Bucket) != 2 || lb.Buckets.Bucket[0].Name != "photos" || lb.Buckets.Bucket[1].Name != "videos" {
		t.Fatalf("buckets=%+v", lb.Buckets.Bucket)
	}

	// Put object with content-type, user metadata, Content-MD5, and X-Amz-Content-Sha256.
	payload := []byte("hello world!")
	md5Sum := md5.Sum(payload)
	shaSum := sha256.Sum256(payload)
	wantETag := `"` + hex.EncodeToString(md5Sum[:]) + `"`

	resp, body = doReq(t, http.MethodPut, ts.URL+"/photos/dir/../../etc/passwd", bytes.NewReader(payload), map[string]string{
		"Content-Type":         "text/plain; charset=utf-8",
		"Content-MD5":          base64.StdEncoding.EncodeToString(md5Sum[:]),
		"X-Amz-Content-Sha256": hex.EncodeToString(shaSum[:]),
		"X-Amz-Meta-Author":    "ashu",
		"X-Amz-Meta-Project":   "gostore",
	})
	if resp.StatusCode != http.StatusOK || resp.Header.Get("ETag") != wantETag {
		t.Fatalf("put status=%d etag=%q body=%s", resp.StatusCode, resp.Header.Get("ETag"), body)
	}

	// HEAD and GET preserve headers, metadata, ETag, length, and Last-Modified.
	for _, method := range []string{http.MethodHead, http.MethodGet} {
		resp, body = doReq(t, method, ts.URL+"/photos/dir/../../etc/passwd", nil, nil)
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("%s status=%d body=%s", method, resp.StatusCode, body)
		}
		if resp.Header.Get("Content-Type") != "text/plain; charset=utf-8" ||
			resp.Header.Get("Content-Length") != strconv.Itoa(len(payload)) ||
			resp.Header.Get("ETag") != wantETag ||
			resp.Header.Get("Accept-Ranges") != "bytes" ||
			resp.Header.Get("X-Amz-Meta-Author") != "ashu" ||
			resp.Header.Get("X-Amz-Meta-Project") != "gostore" {
			t.Fatalf("%s unexpected headers: %v", method, resp.Header)
		}
		if _, err := time.Parse(http.TimeFormat, resp.Header.Get("Last-Modified")); err != nil {
			t.Fatalf("%s invalid Last-Modified %q: %v", method, resp.Header.Get("Last-Modified"), err)
		}
		if method == http.MethodHead && len(body) != 0 {
			t.Fatalf("HEAD returned body %q", body)
		}
		if method == http.MethodGet && !bytes.Equal(body, payload) {
			t.Fatalf("GET returned %q, want %q", body, payload)
		}
	}

	// Non-empty bucket cannot be deleted.
	resp, body = doReq(t, http.MethodDelete, ts.URL+"/photos", nil, nil)
	if resp.StatusCode != http.StatusConflict || decodeErrorXML(t, body).Code != "BucketNotEmpty" {
		t.Fatalf("status=%d body=%s", resp.StatusCode, body)
	}

	// Delete existing and missing objects both return 204.
	for _, path := range []string{"/photos/dir/../../etc/passwd", "/photos/does-not-exist"} {
		resp, body = doReq(t, http.MethodDelete, ts.URL+path, nil, nil)
		if resp.StatusCode != http.StatusNoContent || len(body) != 0 {
			t.Fatalf("delete %s: status=%d body=%s", path, resp.StatusCode, body)
		}
	}

	// Delete empty bucket succeeds; missing bucket returns 404.
	resp, _ = doReq(t, http.MethodDelete, ts.URL+"/photos", nil, nil)
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("delete bucket status=%d", resp.StatusCode)
	}
	resp, body = doReq(t, http.MethodDelete, ts.URL+"/photos", nil, nil)
	if resp.StatusCode != http.StatusNotFound || decodeErrorXML(t, body).Code != "NoSuchBucket" {
		t.Fatalf("status=%d body=%s", resp.StatusCode, body)
	}
}

func TestPutValidationAndLimitsHTTP(t *testing.T) {
	ts, _, _ := newTestServer(t, Options{MaxObjectSize: 16, MaxListKeys: 2})
	resp, _ := doReq(t, http.MethodPut, ts.URL+"/photos", nil, nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("create bucket status=%d", resp.StatusCode)
	}

	tests := []struct {
		name       string
		path       string
		body       string
		headers    map[string]string
		wantStatus int
		wantCode   string
	}{
		{
			name:       "missing bucket",
			path:       "/missing/a.txt",
			body:       "hi",
			wantStatus: http.StatusNotFound,
			wantCode:   "NoSuchBucket",
		},
		{
			name:       "exceeds max object size",
			path:       "/photos/big.txt",
			body:       strings.Repeat("a", 17),
			wantStatus: http.StatusBadRequest,
			wantCode:   "EntityTooLarge",
		},
		{
			name:       "invalid Content-MD5 format",
			path:       "/photos/a.txt",
			body:       "hi",
			headers:    map[string]string{"Content-MD5": "not-base64!"},
			wantStatus: http.StatusBadRequest,
			wantCode:   "InvalidDigest",
		},
		{
			name:       "mismatched Content-MD5",
			path:       "/photos/a.txt",
			body:       "hi",
			headers:    map[string]string{"Content-MD5": base64.StdEncoding.EncodeToString(make([]byte, 16))},
			wantStatus: http.StatusBadRequest,
			wantCode:   "BadDigest",
		},
		{
			name:       "mismatched sha256",
			path:       "/photos/a.txt",
			body:       "hi",
			headers:    map[string]string{"X-Amz-Content-Sha256": strings.Repeat("0", 64)},
			wantStatus: http.StatusBadRequest,
			wantCode:   "XAmzContentSHA256Mismatch",
		},
		{
			name:       "metadata too large",
			path:       "/photos/a.txt",
			body:       "hi",
			headers:    map[string]string{"X-Amz-Meta-Big": strings.Repeat("a", 2049)},
			wantStatus: http.StatusBadRequest,
			wantCode:   "MetadataTooLarge",
		},
		{
			name:       "empty metadata name",
			path:       "/photos/a.txt",
			body:       "hi",
			headers:    map[string]string{"X-Amz-Meta-": "val"},
			wantStatus: http.StatusBadRequest,
			wantCode:   "InvalidArgument",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r, b := doReq(t, http.MethodPut, ts.URL+tt.path, strings.NewReader(tt.body), tt.headers)
			if r.StatusCode != tt.wantStatus || decodeErrorXML(t, b).Code != tt.wantCode {
				t.Fatalf("got status=%d body=%s", r.StatusCode, b)
			}
		})
	}
}

func TestListObjectsV2HTTP(t *testing.T) {
	ts, _, _ := newTestServer(t, Options{MaxListKeys: 2})
	doReq(t, http.MethodPut, ts.URL+"/photos", nil, nil)

	for _, k := range []string{"a.txt", "dir/1.txt", "dir/2.txt", "dir/sub/3.txt", "z.txt"} {
		resp, _ := doReq(t, http.MethodPut, ts.URL+"/photos/"+k, strings.NewReader("v"), nil)
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("put %s: %d", k, resp.StatusCode)
		}
	}

	// Paginate with server-configured default limit (MaxListKeys: 2).
	var gotKeys, gotPrefixes []string
	token := ""
	for {
		u := ts.URL + "/photos?list-type=2&delimiter=/"
		if token != "" {
			u += "&continuation-token=" + token
		}
		resp, body := doReq(t, http.MethodGet, u, nil, nil)
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("list status=%d body=%s", resp.StatusCode, body)
		}
		var res listBucketResultXML
		if err := xml.Unmarshal(body, &res); err != nil {
			t.Fatal(err)
		}
		if res.MaxKeys != 2 || res.KeyCount != len(res.Contents)+len(res.CommonPrefixes) {
			t.Fatalf("unexpected counts: %+v", res)
		}
		for _, c := range res.Contents {
			gotKeys = append(gotKeys, c.Key)
		}
		for _, p := range res.CommonPrefixes {
			gotPrefixes = append(gotPrefixes, p.Prefix)
		}
		if !res.IsTruncated {
			break
		}
		token = res.NextContinuationToken
	}
	if !slices.Equal(gotKeys, []string{"a.txt", "z.txt"}) || !slices.Equal(gotPrefixes, []string{"dir/"}) {
		t.Fatalf("got keys=%q prefixes=%q", gotKeys, gotPrefixes)
	}

	// max-keys=0 returns empty non-truncated response.
	resp, body := doReq(t, http.MethodGet, ts.URL+"/photos?list-type=2&max-keys=0", nil, nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status=%d body=%s", resp.StatusCode, body)
	}
	var zeroRes listBucketResultXML
	if err := xml.Unmarshal(body, &zeroRes); err != nil {
		t.Fatal(err)
	}
	if zeroRes.MaxKeys != 0 || zeroRes.KeyCount != 0 || zeroRes.IsTruncated {
		t.Fatalf("unexpected zero max-keys result: %+v", zeroRes)
	}

	// Invalid parameters.
	resp, body = doReq(t, http.MethodGet, ts.URL+"/photos?list-type=2&max-keys=-1", nil, nil)
	if resp.StatusCode != http.StatusBadRequest || decodeErrorXML(t, body).Code != "InvalidArgument" {
		t.Fatalf("status=%d body=%s", resp.StatusCode, body)
	}
	resp, body = doReq(t, http.MethodGet, ts.URL+"/photos?list-type=2&continuation-token=bad", nil, nil)
	if resp.StatusCode != http.StatusBadRequest || decodeErrorXML(t, body).Code != "InvalidArgument" {
		t.Fatalf("status=%d body=%s", resp.StatusCode, body)
	}
	resp, body = doReq(t, http.MethodGet, ts.URL+"/missing?list-type=2", nil, nil)
	if resp.StatusCode != http.StatusNotFound || decodeErrorXML(t, body).Code != "NoSuchBucket" {
		t.Fatalf("status=%d body=%s", resp.StatusCode, body)
	}
}

func TestByteRangesHTTP(t *testing.T) {
	ts, _, _ := newTestServer(t)
	doReq(t, http.MethodPut, ts.URL+"/photos", nil, nil)
	doReq(t, http.MethodPut, ts.URL+"/photos/data.bin", strings.NewReader("0123456789"), nil)
	doReq(t, http.MethodPut, ts.URL+"/photos/empty.bin", nil, nil)

	valid := []struct {
		name      string
		rangeHdr  string
		wantRange string
		wantBody  string
	}{
		{"first bytes", "bytes=0-3", "bytes 0-3/10", "0123"},
		{"middle bytes", "bytes=2-5", "bytes 2-5/10", "2345"},
		{"clamped end", "bytes=7-999", "bytes 7-9/10", "789"},
		{"open ended", "bytes=6-", "bytes 6-9/10", "6789"},
		{"suffix", "bytes=-4", "bytes 6-9/10", "6789"},
		{"suffix larger than object", "bytes=-50", "bytes 0-9/10", "0123456789"},
	}
	for _, tt := range valid {
		t.Run(tt.name, func(t *testing.T) {
			resp, body := doReq(t, http.MethodGet, ts.URL+"/photos/data.bin", nil, map[string]string{"Range": tt.rangeHdr})
			if resp.StatusCode != http.StatusPartialContent ||
				resp.Header.Get("Content-Range") != tt.wantRange ||
				resp.Header.Get("Content-Length") != strconv.Itoa(len(tt.wantBody)) ||
				string(body) != tt.wantBody {
				t.Fatalf("status=%d range=%q len=%q body=%q",
					resp.StatusCode, resp.Header.Get("Content-Range"), resp.Header.Get("Content-Length"), body)
			}

			respH, bodyH := doReq(t, http.MethodHead, ts.URL+"/photos/data.bin", nil, map[string]string{"Range": tt.rangeHdr})
			if respH.StatusCode != http.StatusPartialContent ||
				respH.Header.Get("Content-Range") != tt.wantRange ||
				respH.Header.Get("Content-Length") != strconv.Itoa(len(tt.wantBody)) ||
				len(bodyH) != 0 {
				t.Fatalf("HEAD status=%d range=%q body=%q", respH.StatusCode, respH.Header.Get("Content-Range"), bodyH)
			}
		})
	}

	invalid := []struct {
		name     string
		path     string
		rangeHdr string
		wantCR   string
	}{
		{"past end", "/photos/data.bin", "bytes=10-12", "bytes */10"},
		{"inverted", "/photos/data.bin", "bytes=5-2", "bytes */10"},
		{"zero suffix", "/photos/data.bin", "bytes=-0", "bytes */10"},
		{"multiple ranges", "/photos/data.bin", "bytes=0-1,3-4", "bytes */10"},
		{"non bytes unit", "/photos/data.bin", "items=0-2", "bytes */10"},
		{"malformed", "/photos/data.bin", "bytes=abc", "bytes */10"},
		{"empty object range", "/photos/empty.bin", "bytes=0-0", "bytes */0"},
		{"empty object suffix", "/photos/empty.bin", "bytes=-1", "bytes */0"},
	}
	for _, tt := range invalid {
		t.Run(tt.name, func(t *testing.T) {
			resp, body := doReq(t, http.MethodGet, ts.URL+tt.path, nil, map[string]string{"Range": tt.rangeHdr})
			if resp.StatusCode != http.StatusRequestedRangeNotSatisfiable ||
				resp.Header.Get("Content-Range") != tt.wantCR ||
				decodeErrorXML(t, body).Code != "InvalidRange" {
				t.Fatalf("status=%d cr=%q body=%s", resp.StatusCode, resp.Header.Get("Content-Range"), body)
			}
		})
	}
}

func TestUnsupportedOperationsRejectedHTTP(t *testing.T) {
	ts, _, _ := newTestServer(t)
	doReq(t, http.MethodPut, ts.URL+"/photos", nil, nil)
	doReq(t, http.MethodPut, ts.URL+"/photos/keep.txt", strings.NewReader("important"), nil)

	notImplemented := []struct {
		name    string
		method  string
		path    string
		headers map[string]string
	}{
		{"upload part query", http.MethodPut, "/photos/keep.txt?partNumber=1&uploadId=u1", nil},
		{"object tagging put", http.MethodPut, "/photos/keep.txt?tagging", nil},
		{"copy object header", http.MethodPut, "/photos/keep.txt", map[string]string{"X-Amz-Copy-Source": "/photos/other"}},
		{"streaming payload", http.MethodPut, "/photos/keep.txt", map[string]string{"X-Amz-Content-Sha256": "STREAMING-AWS4-HMAC-SHA256-PAYLOAD"}},
		{"initiate multipart", http.MethodPost, "/photos/keep.txt?uploads", nil},
		{"complete multipart", http.MethodPost, "/photos/keep.txt?uploadId=u1", nil},
		{"abort multipart", http.MethodDelete, "/photos/keep.txt?uploadId=u1", nil},
		{"delete tagging", http.MethodDelete, "/photos/keep.txt?tagging", nil},
		{"get tagging", http.MethodGet, "/photos/keep.txt?tagging", nil},
		{"list objects v1 omitted", http.MethodGet, "/photos", nil},
		{"list objects v1 explicit", http.MethodGet, "/photos?list-type=1", nil},
		{"bucket acl", http.MethodGet, "/photos?acl", nil},
		{"bucket put acl", http.MethodPut, "/photos?acl", nil},
		{"batch delete", http.MethodPost, "/photos?delete", nil},
	}
	for _, tt := range notImplemented {
		t.Run(tt.name, func(t *testing.T) {
			resp, body := doReq(t, tt.method, ts.URL+tt.path, strings.NewReader("overwrite"), tt.headers)
			if resp.StatusCode != http.StatusNotImplemented || decodeErrorXML(t, body).Code != "NotImplemented" {
				t.Fatalf("status=%d body=%s", resp.StatusCode, body)
			}
		})
	}

	// Rejected operations must never have overwritten or deleted keep.txt.
	resp, body := doReq(t, http.MethodGet, ts.URL+"/photos/keep.txt", nil, nil)
	if resp.StatusCode != http.StatusOK || string(body) != "important" {
		t.Fatalf("object was altered: status=%d body=%q", resp.StatusCode, body)
	}

	// Unsupported methods return 405 MethodNotAllowed.
	for _, target := range []string{"/", "/photos", "/photos/keep.txt", "/-/healthz"} {
		resp, body := doReq(t, http.MethodPatch, ts.URL+target, nil, nil)
		if resp.StatusCode != http.StatusMethodNotAllowed || decodeErrorXML(t, body).Code != "MethodNotAllowed" {
			t.Fatalf("PATCH %s: status=%d body=%s", target, resp.StatusCode, body)
		}
	}
}

type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

func findOnlyBlob(t *testing.T, dataDir string) string {
	t.Helper()
	var found []string
	err := filepath.WalkDir(filepath.Join(dataDir, "blobs"), func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.Type().IsRegular() {
			found = append(found, path)
		}
		return nil
	})
	if err != nil || len(found) != 1 {
		t.Fatalf("found=%v err=%v", found, err)
	}
	return found[0]
}

func flipByteAt(t *testing.T, path string, offset int64) {
	t.Helper()
	f, err := os.OpenFile(path, os.O_RDWR, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	var b [1]byte
	if _, err := f.ReadAt(b[:], offset); err != nil {
		t.Fatal(err)
	}
	b[0] ^= 0xff
	if _, err := f.WriteAt(b[:], offset); err != nil {
		t.Fatal(err)
	}
}

func TestCorruptedBlobAbortsFullDownload(t *testing.T) {
	t.Run("small blob returns 500 without body bytes", func(t *testing.T) {
		dir := t.TempDir()
		engine, _, err := storage.Open(dir)
		if err != nil {
			t.Fatal(err)
		}
		defer engine.Close()

		var logs syncBuffer
		log := slog.New(slog.NewTextHandler(&logs, nil))
		ts := httptest.NewServer(New(log, engine))
		defer ts.Close()

		doReq(t, http.MethodPut, ts.URL+"/photos", nil, nil)
		doReq(t, http.MethodPut, ts.URL+"/photos/small.bin", strings.NewReader("secret-payload"), nil)

		flipByteAt(t, findOnlyBlob(t, dir), 5)

		resp, body := doReq(t, http.MethodGet, ts.URL+"/photos/small.bin", nil, nil)
		if resp.StatusCode != http.StatusInternalServerError || decodeErrorXML(t, body).Code != "InternalError" {
			t.Fatalf("status=%d body=%s", resp.StatusCode, body)
		}
		if !strings.Contains(logs.String(), "corrupted object read") {
			t.Fatalf("expected error log, got %q", logs.String())
		}
	})

	t.Run("large blob holds final chunk and closes connection", func(t *testing.T) {
		for _, corruptOffset := range []int64{0, 200*1024 - 1} {
			dir := t.TempDir()
			engine, _, err := storage.Open(dir)
			if err != nil {
				t.Fatal(err)
			}

			var logs syncBuffer
			log := slog.New(slog.NewTextHandler(&logs, nil))
			ts := httptest.NewServer(New(log, engine))

			data := bytes.Repeat([]byte("abcdefgh"), (200*1024)/8)
			doReq(t, http.MethodPut, ts.URL+"/photos", nil, nil)
			resp, _ := doReq(t, http.MethodPut, ts.URL+"/photos/large.bin", bytes.NewReader(data), nil)
			if resp.StatusCode != http.StatusOK {
				t.Fatalf("put status=%d", resp.StatusCode)
			}

			flipByteAt(t, findOnlyBlob(t, dir), corruptOffset)

			getResp, err := http.Get(ts.URL + "/photos/large.bin")
			if err != nil {
				t.Fatal(err)
			}
			got, readErr := io.ReadAll(getResp.Body)
			_ = getResp.Body.Close()

			if readErr == nil || !errors.Is(readErr, io.ErrUnexpectedEOF) && len(got) == len(data) {
				t.Fatalf("offset=%d: expected failed incomplete download, got len=%d err=%v", corruptOffset, len(got), readErr)
			}
			if len(got) >= len(data) {
				t.Fatalf("offset=%d: received complete %d bytes despite corruption", corruptOffset, len(got))
			}
			if !strings.Contains(logs.String(), "corrupted object read") {
				t.Fatalf("offset=%d: expected corruption log, got %q", corruptOffset, logs.String())
			}

			// Range reads are not SHA-256 verified in v1.
			rangeResp, rangeBody := doReq(t, http.MethodGet, ts.URL+"/photos/large.bin", nil, map[string]string{"Range": "bytes=16-31"})
			if rangeResp.StatusCode != http.StatusPartialContent || len(rangeBody) != 16 {
				t.Fatalf("range read status=%d len=%d", rangeResp.StatusCode, len(rangeBody))
			}

			ts.Close()
			_ = engine.Close()
		}
	})
}
