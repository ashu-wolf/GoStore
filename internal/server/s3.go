//go:build linux

package server

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/ashu-wolf/gostore/internal/blob"
	"github.com/ashu-wolf/gostore/internal/domain"
	"github.com/ashu-wolf/gostore/internal/storage"
)

const streamBufSize = 64 * 1024

func (h *Handler) handleService(w http.ResponseWriter, r *http.Request) {
	if hasUnsupportedQuery(r.URL.Query()) {
		setOp(r, "unsupported")
		h.writeNotImplemented(w, r, "Query parameters are not supported on the service resource.")
		return
	}
	if r.Method != http.MethodGet {
		setOp(r, "unsupported")
		h.writeMethodNotAllowed(w, r)
		return
	}
	setOp(r, "list_buckets")
	buckets := h.store.ListBuckets()
	out := make([]bucketXML, len(buckets))
	for i, b := range buckets {
		out[i] = bucketXML{
			Name:         b.Name,
			CreationDate: b.CreatedAt.UTC().Format("2006-01-02T15:04:05.000Z"),
		}
	}
	writeXML(w, http.StatusOK, listAllMyBucketsXML{
		Xmlns: s3XMLNS,
		Owner: ownerXML{ID: "gostore", DisplayName: "gostore"},
		Buckets: bucketsXML{
			Bucket: out,
		},
	})
}

func (h *Handler) handleBucket(w http.ResponseWriter, r *http.Request, bucket string) {
	q := r.URL.Query()
	switch r.Method {
	case http.MethodPut:
		if hasUnsupportedQuery(q) {
			setOp(r, "unsupported")
			h.writeNotImplemented(w, r, "Bucket subresource operations are not supported.")
			return
		}
		setOp(r, "create_bucket")
		if err := h.store.CreateBucket(bucket); err != nil {
			h.writeMappedError(w, r, err)
			return
		}
		w.Header().Set("Location", "/"+bucket)
		w.WriteHeader(http.StatusOK)

	case http.MethodDelete:
		if hasUnsupportedQuery(q) {
			setOp(r, "unsupported")
			h.writeNotImplemented(w, r, "Bucket subresource operations are not supported.")
			return
		}
		setOp(r, "delete_bucket")
		if err := h.store.DeleteBucket(bucket); err != nil {
			h.writeMappedError(w, r, err)
			return
		}
		w.WriteHeader(http.StatusNoContent)

	case http.MethodGet:
		if hasUnsupportedQuery(q, "list-type", "prefix", "delimiter", "max-keys", "continuation-token", "start-after", "encoding-type", "fetch-owner") {
			setOp(r, "unsupported")
			h.writeNotImplemented(w, r, "The requested bucket operation is not supported.")
			return
		}
		switch q.Get("list-type") {
		case "", "1":
			setOp(r, "unsupported")
			h.writeNotImplemented(w, r, "Only ListObjectsV2 (list-type=2) is supported.")
			return
		case "2":
			setOp(r, "list_objects_v2")
			h.handleListObjectsV2(w, r, bucket, q)
		default:
			setOp(r, "list_objects_v2")
			h.writeError(w, r, http.StatusBadRequest, "InvalidArgument", "Invalid list-type parameter.")
		}

	case http.MethodPost:
		setOp(r, "unsupported")
		h.writeNotImplemented(w, r, "The requested bucket operation is not supported.")

	default:
		setOp(r, "unsupported")
		h.writeMethodNotAllowed(w, r)
	}
}

func (h *Handler) handleListObjectsV2(w http.ResponseWriter, r *http.Request, bucket string, q url.Values) {
	encodingType := q.Get("encoding-type")
	if encodingType != "" && encodingType != "url" {
		h.writeError(w, r, http.StatusBadRequest, "InvalidArgument", "Invalid encoding-type parameter.")
		return
	}

	maxKeys := h.maxListKeys
	zeroMax := false
	if rawMax := q.Get("max-keys"); rawMax != "" {
		v, err := strconv.Atoi(rawMax)
		if err != nil || v < 0 {
			h.writeError(w, r, http.StatusBadRequest, "InvalidArgument", "Invalid max-keys parameter.")
			return
		}
		if v == 0 {
			zeroMax = true
			maxKeys = 1 // validate bucket and token, then return zero entries
		} else if v < maxKeys {
			maxKeys = v
		}
	}

	opts := domain.ListOptions{
		Bucket:            bucket,
		Prefix:            q.Get("prefix"),
		Delimiter:         q.Get("delimiter"),
		MaxKeys:           maxKeys,
		ContinuationToken: q.Get("continuation-token"),
		StartAfter:        q.Get("start-after"),
	}
	res, err := h.store.ListObjects(opts)
	if err != nil {
		h.writeMappedError(w, r, err)
		return
	}
	if zeroMax {
		res = domain.ListResult{}
		maxKeys = 0
	}

	encode := func(s string) string { return s }
	if encodingType == "url" {
		encode = url.QueryEscape
	}

	contents := make([]objectXML, len(res.Objects))
	for i, o := range res.Objects {
		contents[i] = objectXML{
			Key:          encode(o.Key),
			LastModified: o.ModifiedAt.UTC().Format("2006-01-02T15:04:05.000Z"),
			ETag:         `"` + o.ETag + `"`,
			Size:         o.Size,
			StorageClass: "STANDARD",
		}
	}
	prefixes := make([]commonPrefixXML, len(res.CommonPrefixes))
	for i, p := range res.CommonPrefixes {
		prefixes[i] = commonPrefixXML{Prefix: encode(p)}
	}

	writeXML(w, http.StatusOK, listBucketResultXML{
		Xmlns:                 s3XMLNS,
		Name:                  bucket,
		Prefix:                encode(opts.Prefix),
		KeyCount:              len(contents) + len(prefixes),
		MaxKeys:               maxKeys,
		Delimiter:             encode(opts.Delimiter),
		IsTruncated:           res.IsTruncated,
		ContinuationToken:     opts.ContinuationToken,
		NextContinuationToken: res.NextContinuationToken,
		StartAfter:            encode(opts.StartAfter),
		EncodingType:          encodingType,
		Contents:              contents,
		CommonPrefixes:        prefixes,
	})
}

func (h *Handler) handleObject(w http.ResponseWriter, r *http.Request, bucket, key string) {
	q := r.URL.Query()
	switch r.Method {
	case http.MethodPut:
		rawSHA := r.Header.Get("X-Amz-Content-Sha256")
		unsupportedStreaming := strings.HasPrefix(rawSHA, "STREAMING-") &&
			rawSHA != payloadChunkedSigned &&
			rawSHA != payloadChunkedTrailer &&
			rawSHA != payloadChunkedUnsigned
		if r.Header.Get("X-Amz-Copy-Source") != "" || unsupportedStreaming {
			setOp(r, "unsupported")
			h.writeNotImplemented(w, r, "The requested object upload operation is not supported.")
			return
		}
		if q.Has("partNumber") || q.Has("uploadId") {
			if hasUnsupportedQuery(q, "partNumber", "uploadId") {
				setOp(r, "unsupported")
				h.writeNotImplemented(w, r, "The requested object upload operation is not supported.")
				return
			}
			setOp(r, "upload_part")
			h.handleUploadPart(w, r, bucket, key, q)
			return
		}
		if hasUnsupportedQuery(q) {
			setOp(r, "unsupported")
			h.writeNotImplemented(w, r, "The requested object upload operation is not supported.")
			return
		}
		setOp(r, "put_object")
		h.handlePutObject(w, r, bucket, key)

	case http.MethodGet:
		if q.Has("uploadId") {
			if hasUnsupportedQuery(q, "uploadId", "max-parts", "part-number-marker", "encoding-type") {
				setOp(r, "unsupported")
				h.writeNotImplemented(w, r, "Object subresource operations are not supported.")
				return
			}
			setOp(r, "list_parts")
			h.handleListParts(w, r, bucket, key, q)
			return
		}
		if hasUnsupportedQuery(q) {
			setOp(r, "unsupported")
			h.writeNotImplemented(w, r, "Object subresource operations are not supported.")
			return
		}
		setOp(r, "get_object")
		h.handleGetOrHeadObject(w, r, bucket, key)

	case http.MethodHead:
		if hasUnsupportedQuery(q) {
			setOp(r, "unsupported")
			h.writeNotImplemented(w, r, "Object subresource operations are not supported.")
			return
		}
		setOp(r, "head_object")
		h.handleGetOrHeadObject(w, r, bucket, key)

	case http.MethodDelete:
		if q.Has("uploadId") {
			if hasUnsupportedQuery(q, "uploadId") {
				setOp(r, "unsupported")
				h.writeNotImplemented(w, r, "Object subresource operations are not supported.")
				return
			}
			setOp(r, "abort_multipart")
			h.handleAbortMultipartUpload(w, r, bucket, key, q.Get("uploadId"))
			return
		}
		if hasUnsupportedQuery(q) {
			setOp(r, "unsupported")
			h.writeNotImplemented(w, r, "Object subresource operations are not supported.")
			return
		}
		setOp(r, "delete_object")
		if err := h.store.DeleteObject(bucket, key); err != nil {
			h.writeMappedError(w, r, err)
			return
		}
		w.WriteHeader(http.StatusNoContent)

	case http.MethodPost:
		switch {
		case q.Has("uploads") && !hasUnsupportedQuery(q, "uploads"):
			setOp(r, "initiate_multipart")
			h.handleCreateMultipartUpload(w, r, bucket, key)
		case q.Has("uploadId") && !hasUnsupportedQuery(q, "uploadId"):
			setOp(r, "complete_multipart")
			h.handleCompleteMultipartUpload(w, r, bucket, key, q.Get("uploadId"))
		default:
			setOp(r, "unsupported")
			h.writeNotImplemented(w, r, "The requested object operation is not supported.")
		}

	default:
		setOp(r, "unsupported")
		h.writeMethodNotAllowed(w, r)
	}
}

func (h *Handler) prepareUploadBody(w http.ResponseWriter, r *http.Request) (io.Reader, blob.PutOptions, func(bool), bool) {
	var wantMD5 string
	if rawMD5 := r.Header.Get("Content-MD5"); rawMD5 != "" {
		sum, err := base64.StdEncoding.DecodeString(rawMD5)
		if err != nil || len(sum) != 16 {
			h.writeError(w, r, http.StatusBadRequest, "InvalidDigest", "The Content-MD5 you specified was invalid.")
			return nil, blob.PutOptions{}, nil, false
		}
		wantMD5 = hex.EncodeToString(sum)
	}

	auth, authenticated := authFromContext(r)
	isChunked := authenticated && (auth.payloadMode == payloadChunkedSigned ||
		auth.payloadMode == payloadChunkedTrailer ||
		auth.payloadMode == payloadChunkedUnsigned)

	rc := http.NewResponseController(w)
	var body io.Reader = &deadlineReader{
		r:       r.Body,
		rc:      rc,
		timeout: h.idleTransferTimeout,
	}
	cleanup := func(resetDeadline bool) {
		if resetDeadline && h.idleTransferTimeout > 0 {
			_ = rc.SetReadDeadline(time.Time{})
		}
	}

	var (
		wantSize   *int64
		wantSHA256 string
	)

	if isChunked {
		rawDecoded := r.Header.Get("X-Amz-Decoded-Content-Length")
		decodedLen, err := strconv.ParseInt(rawDecoded, 10, 64)
		if err != nil || decodedLen < 0 {
			cleanup(false)
			h.writeError(w, r, http.StatusBadRequest, "InvalidArgument", "Invalid x-amz-decoded-content-length header.")
			return nil, blob.PutOptions{}, nil, false
		}
		if decodedLen > h.maxObjectSize {
			cleanup(false)
			h.writeError(w, r, http.StatusBadRequest, "EntityTooLarge", "Your proposed upload exceeds the maximum allowed size.")
			return nil, blob.PutOptions{}, nil, false
		}
		wantSize = &decodedLen
		chunked, err := newAWSChunkedReader(body, auth, parseExpectedTrailers(r.Header))
		if err != nil {
			cleanup(false)
			h.writeMappedError(w, r, err)
			return nil, blob.PutOptions{}, nil, false
		}
		body = chunked
	} else {
		if r.ContentLength > h.maxObjectSize {
			cleanup(false)
			h.writeError(w, r, http.StatusBadRequest, "EntityTooLarge", "Your proposed upload exceeds the maximum allowed size.")
			return nil, blob.PutOptions{}, nil, false
		}
		if r.ContentLength >= 0 {
			cl := r.ContentLength
			wantSize = &cl
		}
		if rawSHA := r.Header.Get("X-Amz-Content-Sha256"); rawSHA != "" && rawSHA != payloadUnsigned {
			if len(rawSHA) != 64 {
				cleanup(false)
				h.writeError(w, r, http.StatusBadRequest, "InvalidArgument", "Invalid x-amz-content-sha256 header.")
				return nil, blob.PutOptions{}, nil, false
			}
			if _, err := hex.DecodeString(rawSHA); err != nil {
				cleanup(false)
				h.writeError(w, r, http.StatusBadRequest, "InvalidArgument", "Invalid x-amz-content-sha256 header.")
				return nil, blob.PutOptions{}, nil, false
			}
			wantSHA256 = rawSHA
		}
		wrapped, err := wrapHeaderChecksums(body, r.Header)
		if err != nil {
			cleanup(false)
			h.writeMappedError(w, r, err)
			return nil, blob.PutOptions{}, nil, false
		}
		body = wrapped
	}

	return body, blob.PutOptions{
		MaxSize:    h.maxObjectSize,
		WantSize:   wantSize,
		WantMD5:    wantMD5,
		WantSHA256: wantSHA256,
	}, cleanup, true
}

func (h *Handler) handlePutObject(w http.ResponseWriter, r *http.Request, bucket, key string) {
	body, checks, cleanup, ok := h.prepareUploadBody(w, r)
	if !ok {
		return
	}

	ct := r.Header.Get("Content-Type")
	if ct == "" {
		ct = "application/octet-stream"
	}

	obj, err := h.store.PutObject(r.Context(), storage.PutRequest{
		Bucket:      bucket,
		Key:         key,
		ContentType: ct,
		Metadata:    extractMetadata(r.Header),
		Checks:      checks,
	}, body)
	cleanup(err == nil)
	if err != nil {
		h.writeMappedError(w, r, err)
		return
	}
	h.metrics.AddBytesIn(obj.Size)
	w.Header().Set("ETag", `"`+obj.ETag+`"`)
	w.WriteHeader(http.StatusOK)
}

func (h *Handler) handleCreateMultipartUpload(w http.ResponseWriter, r *http.Request, bucket, key string) {
	ct := r.Header.Get("Content-Type")
	if ct == "" {
		ct = "application/octet-stream"
	}
	u, err := h.store.InitiateMultipartUpload(bucket, key, ct, extractMetadata(r.Header))
	if err != nil {
		h.writeMappedError(w, r, err)
		return
	}
	writeXML(w, http.StatusOK, initiateMultipartUploadResultXML{
		Xmlns:    s3XMLNS,
		Bucket:   u.Bucket,
		Key:      u.Key,
		UploadID: u.ID,
	})
}

func (h *Handler) handleUploadPart(w http.ResponseWriter, r *http.Request, bucket, key string, q url.Values) {
	uploadID := q.Get("uploadId")
	if uploadID == "" {
		h.writeError(w, r, http.StatusBadRequest, "InvalidArgument", "Missing uploadId parameter.")
		return
	}
	partNum, err := strconv.Atoi(q.Get("partNumber"))
	if err != nil || partNum < 1 {
		h.writeError(w, r, http.StatusBadRequest, "InvalidArgument", "Part number must be a positive integer.")
		return
	}

	body, checks, cleanup, ok := h.prepareUploadBody(w, r)
	if !ok {
		return
	}

	part, err := h.store.UploadPart(r.Context(), bucket, key, uploadID, partNum, checks, body)
	cleanup(err == nil)
	if err != nil {
		h.writeMappedError(w, r, err)
		return
	}
	h.metrics.AddBytesIn(part.Size)
	w.Header().Set("ETag", `"`+part.ETag+`"`)
	w.WriteHeader(http.StatusOK)
}

func (h *Handler) handleListParts(w http.ResponseWriter, r *http.Request, bucket, key string, q url.Values) {
	uploadID := q.Get("uploadId")
	if uploadID == "" {
		h.writeError(w, r, http.StatusBadRequest, "InvalidArgument", "Missing uploadId parameter.")
		return
	}
	if et := q.Get("encoding-type"); et != "" && et != "url" {
		h.writeError(w, r, http.StatusBadRequest, "InvalidArgument", "Invalid encoding-type parameter.")
		return
	}
	maxParts := h.maxListKeys
	zeroMax := false
	if rawMax := q.Get("max-parts"); rawMax != "" {
		v, err := strconv.Atoi(rawMax)
		if err != nil || v < 0 {
			h.writeError(w, r, http.StatusBadRequest, "InvalidArgument", "Invalid max-parts parameter.")
			return
		}
		if v == 0 {
			zeroMax = true
			maxParts = 1
		} else if v < maxParts {
			maxParts = v
		}
	}
	marker := 0
	if rawMarker := q.Get("part-number-marker"); rawMarker != "" {
		v, err := strconv.Atoi(rawMarker)
		if err != nil || v < 0 {
			h.writeError(w, r, http.StatusBadRequest, "InvalidArgument", "Invalid part-number-marker parameter.")
			return
		}
		marker = v
	}

	res, err := h.store.ListParts(domain.ListPartsOptions{
		Bucket:           bucket,
		Key:              key,
		UploadID:         uploadID,
		PartNumberMarker: marker,
		MaxParts:         maxParts,
	})
	if err != nil {
		h.writeMappedError(w, r, err)
		return
	}
	if zeroMax {
		res.Parts = nil
		res.IsTruncated = false
		res.NextPartNumberMarker = 0
		res.MaxParts = 0
	}

	keyOut := key
	if q.Get("encoding-type") == "url" {
		keyOut = url.QueryEscape(key)
	}
	partsXML := make([]partXML, len(res.Parts))
	for i, p := range res.Parts {
		partsXML[i] = partXML{
			PartNumber:   p.Number,
			LastModified: p.ModifiedAt.UTC().Format("2006-01-02T15:04:05.000Z"),
			ETag:         `"` + p.ETag + `"`,
			Size:         p.Size,
		}
	}
	writeXML(w, http.StatusOK, listPartsResultXML{
		Xmlns:                s3XMLNS,
		Bucket:               bucket,
		Key:                  keyOut,
		UploadID:             uploadID,
		StorageClass:         "STANDARD",
		PartNumberMarker:     marker,
		NextPartNumberMarker: res.NextPartNumberMarker,
		MaxParts:             res.MaxParts,
		IsTruncated:          res.IsTruncated,
		Parts:                partsXML,
	})
}

func (h *Handler) handleCompleteMultipartUpload(w http.ResponseWriter, r *http.Request, bucket, key, uploadID string) {
	if uploadID == "" {
		h.writeError(w, r, http.StatusBadRequest, "InvalidArgument", "Missing uploadId parameter.")
		return
	}
	const maxCompleteBody = 4 << 20
	raw, err := io.ReadAll(io.LimitReader(r.Body, maxCompleteBody+1))
	if err != nil {
		h.writeError(w, r, http.StatusBadRequest, "MalformedXML", "Could not read completion body.")
		return
	}
	if len(raw) > maxCompleteBody {
		h.writeError(w, r, http.StatusBadRequest, "EntityTooLarge", "Completion request body is too large.")
		return
	}
	if auth, ok := authFromContext(r); ok && auth.payloadMode == payloadSigned {
		if !strings.EqualFold(sha256Hex(raw), auth.payloadHash) {
			h.writeError(w, r, http.StatusBadRequest, "XAmzContentSHA256Mismatch", "The provided 'x-amz-content-sha256' header does not match what was computed.")
			return
		}
	}

	var reqXML completeMultipartUploadReqXML
	if err := xml.Unmarshal(raw, &reqXML); err != nil {
		h.writeError(w, r, http.StatusBadRequest, "MalformedXML", "The XML you provided was not well-formed or did not validate against our published schema.")
		return
	}
	submitted := make([]domain.CompletedPart, len(reqXML.Parts))
	for i, p := range reqXML.Parts {
		submitted[i] = domain.CompletedPart{Number: p.PartNumber, ETag: p.ETag}
	}

	res, err := h.store.CompleteMultipartUpload(r.Context(), bucket, key, uploadID, submitted)
	if err != nil {
		h.writeMappedError(w, r, err)
		return
	}
	quotedETag := `"` + res.ETag + `"`
	w.Header().Set("ETag", quotedETag)
	writeXML(w, http.StatusOK, completeMultipartUploadResultXML{
		Xmlns:    s3XMLNS,
		Location: "/" + bucket + "/" + key,
		Bucket:   bucket,
		Key:      key,
		ETag:     quotedETag,
	})
}

func (h *Handler) handleAbortMultipartUpload(w http.ResponseWriter, r *http.Request, bucket, key, uploadID string) {
	if uploadID == "" {
		h.writeError(w, r, http.StatusBadRequest, "InvalidArgument", "Missing uploadId parameter.")
		return
	}
	if err := h.store.AbortMultipartUpload(bucket, key, uploadID); err != nil {
		h.writeMappedError(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (h *Handler) handleGetOrHeadObject(w http.ResponseWriter, r *http.Request, bucket, key string) {
	var (
		obj domain.Object
		rdr io.ReadSeekCloser
		err error
	)
	if r.Method == http.MethodHead {
		obj, err = h.store.HeadObject(bucket, key)
	} else {
		obj, rdr, err = h.store.OpenObject(bucket, key)
	}
	if err != nil {
		h.writeMappedError(w, r, err)
		return
	}
	if rdr != nil {
		defer rdr.Close()
	}

	rc := http.NewResponseController(w)
	dw := &deadlineWriter{w: w, rc: rc, timeout: h.idleTransferTimeout}
	defer func() {
		if h.idleTransferTimeout > 0 {
			_ = rc.SetWriteDeadline(time.Time{})
		}
	}()

	if rangeHdr := r.Header.Get("Range"); rangeHdr != "" {
		br, err := parseByteRange(rangeHdr, obj.Size)
		if err != nil {
			w.Header().Set("Content-Range", fmt.Sprintf("bytes */%d", obj.Size))
			h.writeError(w, r, http.StatusRequestedRangeNotSatisfiable, "InvalidRange", "The requested range is not satisfiable.")
			return
		}
		writeObjectHeaders(w, obj, br.length())
		w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", br.start, br.end, obj.Size))
		w.WriteHeader(http.StatusPartialContent)
		if r.Method == http.MethodHead {
			return
		}
		if _, err := rdr.Seek(br.start, io.SeekStart); err != nil {
			h.log.Error("range seek failed", "bucket", bucket, "key", key, "error", err)
			abortConnection(w)
			return
		}
		copied, err := io.CopyN(dw, rdr, br.length())
		h.metrics.AddBytesOut(copied)
		if err != nil {
			h.log.Error("range read failed", "bucket", bucket, "key", key, "error", err)
			abortConnection(w)
		}
		return
	}

	if r.Method == http.MethodHead {
		writeObjectHeaders(w, obj, obj.Size)
		w.WriteHeader(http.StatusOK)
		return
	}

	headersSent, integrityErr, writeErr := streamVerified(dw, rdr, obj.Size, obj.SHA256, func() {
		writeObjectHeaders(w, obj, obj.Size)
		w.WriteHeader(http.StatusOK)
	}, h.metrics.AddBytesOut)
	if writeErr != nil {
		return
	}
	if integrityErr != nil {
		h.log.Error("corrupted object read", "bucket", bucket, "key", key, "error", integrityErr)
		if !headersSent {
			h.writeError(w, r, http.StatusInternalServerError, "InternalError", "Stored object failed integrity check.")
			return
		}
		setErrCode(r, "InternalError")
		abortConnection(w)
	}
}

// streamVerified streams r to w while verifying size and SHA-256. The final
// chunk is held back until EOF and checksum verification succeed.
func streamVerified(w io.Writer, r io.Reader, wantSize int64, wantSHA256 string, sendHeaders func(), onBytes func(int64)) (headersSent bool, integrityErr error, writeErr error) {
	curr := make([]byte, streamBufSize)
	prev := make([]byte, streamBufSize)
	prevLen := 0
	var total int64
	sum := sha256.New()

	for {
		n, readErr := r.Read(curr)
		if n > 0 {
			_, _ = sum.Write(curr[:n])
			total += int64(n)
			if total > wantSize {
				return headersSent, fmt.Errorf("blob size %d exceeds metadata size %d", total, wantSize), nil
			}
			if prevLen > 0 {
				if !headersSent {
					sendHeaders()
					headersSent = true
				}
				written, err := w.Write(prev[:prevLen])
				if onBytes != nil && written > 0 {
					onBytes(int64(written))
				}
				if err != nil {
					return headersSent, nil, err
				}
			}
			prev, curr = curr, prev
			prevLen = n
		}
		if readErr == io.EOF {
			if total != wantSize {
				return headersSent, fmt.Errorf("blob size %d does not match metadata size %d", total, wantSize), nil
			}
			gotSHA := hex.EncodeToString(sum.Sum(nil))
			if !strings.EqualFold(gotSHA, wantSHA256) {
				return headersSent, fmt.Errorf("SHA-256 mismatch: got %s, want %s", gotSHA, wantSHA256), nil
			}
			if !headersSent {
				sendHeaders()
				headersSent = true
			}
			if prevLen > 0 {
				written, err := w.Write(prev[:prevLen])
				if onBytes != nil && written > 0 {
					onBytes(int64(written))
				}
				if err != nil {
					return headersSent, nil, err
				}
			}
			return headersSent, nil, nil
		}
		if readErr != nil {
			return headersSent, fmt.Errorf("read blob: %w", readErr), nil
		}
	}
}

func abortConnection(w http.ResponseWriter) {
	if conn, _, err := http.NewResponseController(w).Hijack(); err == nil {
		_ = conn.Close()
		return
	}
	panic(http.ErrAbortHandler)
}

type deadlineReader struct {
	r       io.Reader
	rc      *http.ResponseController
	timeout time.Duration
}

func (d *deadlineReader) Read(p []byte) (int, error) {
	if d.timeout > 0 {
		if err := d.rc.SetReadDeadline(time.Now().Add(d.timeout)); err != nil && !errors.Is(err, http.ErrNotSupported) {
			return 0, err
		}
	}
	return d.r.Read(p)
}

type deadlineWriter struct {
	w       io.Writer
	rc      *http.ResponseController
	timeout time.Duration
}

func (d *deadlineWriter) Write(p []byte) (int, error) {
	if d.timeout > 0 {
		if err := d.rc.SetWriteDeadline(time.Now().Add(d.timeout)); err != nil && !errors.Is(err, http.ErrNotSupported) {
			return 0, err
		}
	}
	return d.w.Write(p)
}

func writeObjectHeaders(w http.ResponseWriter, obj domain.Object, contentLength int64) {
	ct := obj.ContentType
	if ct == "" {
		ct = "application/octet-stream"
	}
	w.Header().Set("Content-Type", ct)
	w.Header().Set("Content-Length", strconv.FormatInt(contentLength, 10))
	w.Header().Set("Last-Modified", obj.ModifiedAt.UTC().Format(http.TimeFormat))
	w.Header().Set("ETag", `"`+obj.ETag+`"`)
	w.Header().Set("Accept-Ranges", "bytes")
	for k, v := range obj.Metadata {
		w.Header().Set("X-Amz-Meta-"+k, v)
	}
}

func extractMetadata(hdr http.Header) map[string]string {
	var out map[string]string
	for k, vals := range hdr {
		lower := strings.ToLower(k)
		if name, ok := strings.CutPrefix(lower, "x-amz-meta-"); ok {
			if out == nil {
				out = make(map[string]string)
			}
			out[name] = strings.Join(vals, ",")
		}
	}
	return out
}

func hasUnsupportedQuery(q url.Values, allowed ...string) bool {
	for k := range q {
		if k == "x-id" {
			continue
		}
		ok := false
		for _, a := range allowed {
			if k == a {
				ok = true
				break
			}
		}
		if !ok {
			return true
		}
	}
	return false
}

func (h *Handler) writeNotImplemented(w http.ResponseWriter, r *http.Request, msg string) {
	h.writeError(w, r, http.StatusNotImplemented, "NotImplemented", msg)
}

func (h *Handler) writeMethodNotAllowed(w http.ResponseWriter, r *http.Request) {
	h.writeError(w, r, http.StatusMethodNotAllowed, "MethodNotAllowed", "The specified method is not allowed against this resource.")
}

func (h *Handler) writeMappedError(w http.ResponseWriter, r *http.Request, err error) {
	switch {
	case errors.Is(err, domain.ErrInvalidBucketName):
		h.writeError(w, r, http.StatusBadRequest, "InvalidBucketName", err.Error())
	case errors.Is(err, domain.ErrBucketExists):
		h.writeError(w, r, http.StatusConflict, "BucketAlreadyOwnedByYou", "Your previous request to create the named bucket succeeded and you already own it.")
	case errors.Is(err, domain.ErrNoSuchBucket):
		h.writeError(w, r, http.StatusNotFound, "NoSuchBucket", "The specified bucket does not exist.")
	case errors.Is(err, domain.ErrBucketNotEmpty):
		h.writeError(w, r, http.StatusConflict, "BucketNotEmpty", "The bucket you tried to delete is not empty.")
	case errors.Is(err, domain.ErrNoSuchKey):
		h.writeError(w, r, http.StatusNotFound, "NoSuchKey", "The specified key does not exist.")
	case errors.Is(err, domain.ErrNoSuchUpload):
		h.writeError(w, r, http.StatusNotFound, "NoSuchUpload", "The specified upload does not exist.")
	case errors.Is(err, domain.ErrInvalidPart):
		h.writeError(w, r, http.StatusBadRequest, "InvalidPart", err.Error())
	case errors.Is(err, domain.ErrInvalidOrder):
		h.writeError(w, r, http.StatusBadRequest, "InvalidPartOrder", "The list of parts was not in ascending order. Parts list must be specified in order by part number.")
	case errors.Is(err, domain.ErrEntityTooSmall):
		h.writeError(w, r, http.StatusBadRequest, "EntityTooSmall", "Your proposed upload is smaller than the minimum allowed object size.")
	case errors.Is(err, domain.ErrEntityTooLarge):
		h.writeError(w, r, http.StatusBadRequest, "EntityTooLarge", "Your proposed upload exceeds the maximum allowed size.")
	case errors.Is(err, domain.ErrInsufficientStorage):
		h.writeError(w, r, http.StatusInsufficientStorage, "InsufficientStorage", "There is not enough free storage space for this operation.")
	case errors.Is(err, os.ErrDeadlineExceeded), errors.Is(err, context.DeadlineExceeded):
		h.writeError(w, r, http.StatusRequestTimeout, "RequestTimeout", "Your socket connection to the server was not read from or written to within the timeout period.")
	case errors.Is(err, context.Canceled):
		h.writeError(w, r, http.StatusRequestTimeout, "RequestTimeout", "The request was canceled before completion.")
	case errors.Is(err, domain.ErrInvalidKey),
		errors.Is(err, domain.ErrInvalidMetadata),
		errors.Is(err, domain.ErrInvalidToken),
		errors.Is(err, domain.ErrInvalidArgument):
		h.writeError(w, r, http.StatusBadRequest, "InvalidArgument", err.Error())
	case errors.Is(err, domain.ErrMetadataTooLarge):
		h.writeError(w, r, http.StatusBadRequest, "MetadataTooLarge", err.Error())
	case errors.Is(err, blob.ErrTooLarge):
		h.writeError(w, r, http.StatusBadRequest, "EntityTooLarge", "Your proposed upload exceeds the maximum allowed size.")
	case errors.Is(err, blob.ErrSizeMismatch):
		h.writeError(w, r, http.StatusBadRequest, "IncompleteBody", "You did not provide the number of bytes specified by the Content-Length HTTP header.")
	case errors.Is(err, blob.ErrChecksumMismatch):
		if strings.Contains(err.Error(), "SHA-256") {
			h.writeError(w, r, http.StatusBadRequest, "XAmzContentSHA256Mismatch", "The provided 'x-amz-content-sha256' header does not match what was computed.")
			return
		}
		h.writeError(w, r, http.StatusBadRequest, "BadDigest", "The Content-MD5 you specified did not match what we received.")
	case errors.Is(err, errChunkSignature):
		h.writeError(w, r, http.StatusForbidden, "SignatureDoesNotMatch", "The chunk signature we calculated does not match the signature you provided.")
	case errors.Is(err, errTrailerDigest):
		h.writeError(w, r, http.StatusBadRequest, "BadDigest", err.Error())
	case errors.Is(err, errChunkMalformed):
		h.writeError(w, r, http.StatusBadRequest, "InvalidArgument", err.Error())
	case errors.Is(err, storage.ErrDamaged):
		h.log.Error("damaged object", "path", r.URL.Path, "error", err)
		h.writeError(w, r, http.StatusInternalServerError, "InternalError", "Stored object is damaged.")
	default:
		h.log.Error("request failed", "method", r.Method, "path", r.URL.Path, "error", err)
		h.writeError(w, r, http.StatusInternalServerError, "InternalError", "We encountered an internal error. Please try again.")
	}
}

func (h *Handler) writeError(w http.ResponseWriter, r *http.Request, status int, code, message string) {
	setErrCode(r, code)
	if r.ContentLength != 0 {
		w.Header().Set("Connection", "close")
	}
	if r.Method == http.MethodHead {
		w.WriteHeader(status)
		return
	}
	writeXML(w, status, errorXML{
		Code:      code,
		Message:   message,
		Resource:  r.URL.Path,
		RequestID: requestID(r),
	})
}
