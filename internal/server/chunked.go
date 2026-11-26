//go:build linux

package server

import (
	"bufio"
	"crypto/hmac"
	"crypto/sha1"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"hash"
	"hash/crc32"
	"hash/crc64"
	"io"
	"net/http"
	"slices"
	"strconv"
	"strings"
)

const (
	maxChunkLineBytes = 8 * 1024
	maxChunkDataBytes = 16 << 20
	crc64NVMEPoly     = 0x9a6c9329ac4bc9b5
)

var (
	errChunkSignature = errors.New("chunk signature mismatch")
	errChunkMalformed = errors.New("malformed aws-chunked stream")
	errTrailerDigest  = errors.New("checksum mismatch")
)

var crc64NVMETable = crc64.MakeTable(crc64NVMEPoly)

type checksumTracker struct {
	hashes map[string]hash.Hash
}

func newChecksumTracker(names []string) (*checksumTracker, error) {
	if len(names) == 0 {
		return nil, nil
	}
	t := &checksumTracker{hashes: make(map[string]hash.Hash, len(names))}
	for _, raw := range names {
		name := strings.ToLower(strings.TrimSpace(raw))
		if name == "" {
			continue
		}
		if _, exists := t.hashes[name]; exists {
			continue
		}
		h, err := newChecksumHash(name)
		if err != nil {
			return nil, err
		}
		t.hashes[name] = h
	}
	return t, nil
}

func newChecksumHash(headerName string) (hash.Hash, error) {
	switch headerName {
	case "x-amz-checksum-crc32":
		return crc32.NewIEEE(), nil
	case "x-amz-checksum-crc32c":
		return crc32.New(crc32.MakeTable(crc32.Castagnoli)), nil
	case "x-amz-checksum-sha1":
		return sha1.New(), nil
	case "x-amz-checksum-sha256":
		return sha256.New(), nil
	case "x-amz-checksum-crc64nvme":
		return crc64.New(crc64NVMETable), nil
	default:
		return nil, fmt.Errorf("%w: unsupported checksum header %q", errTrailerDigest, headerName)
	}
}

func (t *checksumTracker) Write(p []byte) {
	if t == nil {
		return
	}
	for _, h := range t.hashes {
		_, _ = h.Write(p)
	}
}

func (t *checksumTracker) verify(provided map[string]string) error {
	if t == nil {
		return nil
	}
	for name, h := range t.hashes {
		gotVal, ok := provided[name]
		if !ok {
			return fmt.Errorf("%w: missing checksum %q", errTrailerDigest, name)
		}
		wantVal := base64.StdEncoding.EncodeToString(h.Sum(nil))
		if gotVal != wantVal {
			return fmt.Errorf("%w: %s mismatch", errTrailerDigest, name)
		}
	}
	return nil
}

// collectHeaderChecksums finds any x-amz-checksum-* headers sent directly on a
// non-chunked upload.
func collectHeaderChecksums(hdr http.Header) (names []string, values map[string]string) {
	for k, vals := range hdr {
		lower := strings.ToLower(k)
		if strings.HasPrefix(lower, "x-amz-checksum-") {
			if values == nil {
				values = make(map[string]string)
			}
			names = append(names, lower)
			values[lower] = strings.Join(vals, ",")
		}
	}
	return names, values
}

type headerChecksumReader struct {
	r       io.Reader
	tracker *checksumTracker
	values  map[string]string
	done    bool
}

func wrapHeaderChecksums(r io.Reader, hdr http.Header) (io.Reader, error) {
	names, values := collectHeaderChecksums(hdr)
	if len(names) == 0 {
		return r, nil
	}
	tracker, err := newChecksumTracker(names)
	if err != nil {
		return nil, err
	}
	return &headerChecksumReader{r: r, tracker: tracker, values: values}, nil
}

func (h *headerChecksumReader) Read(p []byte) (int, error) {
	if h.done {
		return 0, io.EOF
	}
	n, err := h.r.Read(p)
	if n > 0 {
		h.tracker.Write(p[:n])
	}
	if err == io.EOF {
		h.done = true
		if verr := h.tracker.verify(h.values); verr != nil {
			return n, verr
		}
	}
	return n, err
}

// awsChunkedReader decodes AWS Signature V4 streaming (signed or unsigned)
// payloads, verifies chunk signatures and optional trailers, and exposes only
// the decoded object bytes.
type awsChunkedReader struct {
	br           *bufio.Reader
	signed       bool
	hasTrailers  bool
	auth         authResult
	prevSig      string
	expectedTrai []string
	tracker      *checksumTracker

	chunkRem int64
	chunkSum hash.Hash
	chunkGot string
	eof      bool
}

func newAWSChunkedReader(body io.Reader, auth authResult, expectedTrailers []string) (*awsChunkedReader, error) {
	tracker, err := newChecksumTracker(expectedTrailers)
	if err != nil {
		return nil, err
	}
	return &awsChunkedReader{
		br:           bufio.NewReaderSize(body, streamBufSize),
		signed:       auth.payloadMode == payloadChunkedSigned || auth.payloadMode == payloadChunkedTrailer,
		hasTrailers:  auth.payloadMode == payloadChunkedTrailer || auth.payloadMode == payloadChunkedUnsigned,
		auth:         auth,
		prevSig:      auth.signature,
		expectedTrai: expectedTrailers,
		tracker:      tracker,
	}, nil
}

func (c *awsChunkedReader) Read(p []byte) (int, error) {
	if c.eof {
		return 0, io.EOF
	}
	for c.chunkRem == 0 {
		if err := c.nextChunkHeader(); err != nil {
			return 0, err
		}
		if c.eof {
			return 0, io.EOF
		}
	}
	toRead := len(p)
	if int64(toRead) > c.chunkRem {
		toRead = int(c.chunkRem)
	}
	n, err := c.br.Read(p[:toRead])
	if n > 0 {
		if c.chunkSum != nil {
			_, _ = c.chunkSum.Write(p[:n])
		}
		c.tracker.Write(p[:n])
		c.chunkRem -= int64(n)
		if c.chunkRem == 0 {
			if verr := c.finishChunk(); verr != nil {
				return n, verr
			}
		}
	}
	if err == io.EOF && c.chunkRem > 0 {
		return n, fmt.Errorf("%w: unexpected EOF inside chunk", errChunkMalformed)
	}
	if err != nil && err != io.EOF {
		return n, err
	}
	return n, nil
}

func (c *awsChunkedReader) nextChunkHeader() error {
	line, err := readCRLFLine(c.br, maxChunkLineBytes)
	if err != nil {
		return err
	}
	sizeHex, rest, hasMeta := strings.Cut(line, ";")
	size, err := strconv.ParseInt(strings.TrimSpace(sizeHex), 16, 64)
	if err != nil || size < 0 || size > maxChunkDataBytes {
		return fmt.Errorf("%w: invalid chunk size", errChunkMalformed)
	}

	var sig string
	if c.signed {
		if !hasMeta {
			return fmt.Errorf("%w: missing chunk signature", errChunkMalformed)
		}
		var ok bool
		sig, ok = strings.CutPrefix(strings.TrimSpace(rest), "chunk-signature=")
		if !ok || len(sig) != 64 {
			return fmt.Errorf("%w: invalid chunk signature header", errChunkMalformed)
		}
	}

	if size == 0 {
		if c.signed {
			if err := c.verifyChunkSignature(emptySHA256, sig); err != nil {
				return err
			}
		}
		if err := c.consumeTrailers(); err != nil {
			return err
		}
		c.eof = true
		return nil
	}

	c.chunkRem = size
	if c.signed {
		if c.chunkSum == nil {
			c.chunkSum = sha256.New()
		} else {
			c.chunkSum.Reset()
		}
		c.chunkGot = sig
	}
	return nil
}

func (c *awsChunkedReader) finishChunk() error {
	line, err := readCRLFLine(c.br, 16)
	if err != nil {
		return err
	}
	if line != "" {
		return fmt.Errorf("%w: expected CRLF after chunk", errChunkMalformed)
	}
	if c.signed {
		chunkHash := hex.EncodeToString(c.chunkSum.Sum(nil))
		if err := c.verifyChunkSignature(chunkHash, c.chunkGot); err != nil {
			return err
		}
	}
	return nil
}

func (c *awsChunkedReader) verifyChunkSignature(chunkHash, gotSigHex string) error {
	gotSig, err := hex.DecodeString(gotSigHex)
	if err != nil || len(gotSig) != sha256.Size {
		return errChunkSignature
	}
	sts := "AWS4-HMAC-SHA256-PAYLOAD\n" +
		c.auth.amzDate + "\n" +
		c.auth.scope + "\n" +
		c.prevSig + "\n" +
		emptySHA256 + "\n" +
		chunkHash
	wantSig := hmacSHA256(c.auth.signingKey, []byte(sts))
	if !hmac.Equal(gotSig, wantSig) {
		return errChunkSignature
	}
	c.prevSig = hex.EncodeToString(wantSig)
	return nil
}

func (c *awsChunkedReader) consumeTrailers() error {
	trailers := make(map[string]string)
	var trailerSig string
	for {
		line, err := readCRLFLine(c.br, maxChunkLineBytes)
		if err != nil {
			return err
		}
		if line == "" {
			break
		}
		if !c.hasTrailers {
			return fmt.Errorf("%w: unexpected trailer", errChunkMalformed)
		}
		name, val, ok := strings.Cut(line, ":")
		if !ok {
			return fmt.Errorf("%w: malformed trailer line", errChunkMalformed)
		}
		name = strings.ToLower(strings.TrimSpace(name))
		val = strings.TrimSpace(val)
		if name == "x-amz-trailer-signature" {
			trailerSig = val
			continue
		}
		trailers[name] = val
	}

	if c.signed && c.hasTrailers {
		if trailerSig == "" {
			return fmt.Errorf("%w: missing trailer signature", errChunkSignature)
		}
		if err := c.verifyTrailerSignature(trailers, trailerSig); err != nil {
			return err
		}
	}
	return c.tracker.verify(trailers)
}

func (c *awsChunkedReader) verifyTrailerSignature(trailers map[string]string, gotSigHex string) error {
	gotSig, err := hex.DecodeString(gotSigHex)
	if err != nil || len(gotSig) != sha256.Size {
		return errChunkSignature
	}
	keys := make([]string, 0, len(trailers))
	for k := range trailers {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	var canon strings.Builder
	for _, k := range keys {
		canon.WriteString(k)
		canon.WriteByte(':')
		canon.WriteString(collapseWhitespace(trailers[k]))
		canon.WriteByte('\n')
	}
	sts := "AWS4-HMAC-SHA256-TRAILER\n" +
		c.auth.amzDate + "\n" +
		c.auth.scope + "\n" +
		c.prevSig + "\n" +
		sha256Hex([]byte(canon.String()))
	wantSig := hmacSHA256(c.auth.signingKey, []byte(sts))
	if !hmac.Equal(gotSig, wantSig) {
		return errChunkSignature
	}
	return nil
}

func readCRLFLine(br *bufio.Reader, maxLen int) (string, error) {
	var buf []byte
	for {
		frag, err := br.ReadSlice('\n')
		buf = append(buf, frag...)
		if len(buf) > maxLen {
			return "", fmt.Errorf("%w: line too long", errChunkMalformed)
		}
		if err == nil {
			break
		}
		if !errors.Is(err, bufio.ErrBufferFull) {
			return "", fmt.Errorf("%w: %v", errChunkMalformed, err)
		}
	}
	if len(buf) < 2 || buf[len(buf)-2] != '\r' {
		return "", fmt.Errorf("%w: missing CRLF", errChunkMalformed)
	}
	return string(buf[:len(buf)-2]), nil
}

// parseExpectedTrailers parses X-Amz-Trailer header values (comma-separated).
func parseExpectedTrailers(hdr http.Header) []string {
	var out []string
	for _, v := range hdr.Values("X-Amz-Trailer") {
		for _, part := range strings.Split(v, ",") {
			if name := strings.ToLower(strings.TrimSpace(part)); name != "" {
				out = append(out, name)
			}
		}
	}
	return out
}
