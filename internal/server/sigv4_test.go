//go:build linux

package server

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"hash/crc32"
	"hash/crc64"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	v4 "github.com/aws/aws-sdk-go-v2/aws/signer/v4"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	s3types "github.com/aws/aws-sdk-go-v2/service/s3/types"

	"github.com/ashu-wolf/gostore/internal/storage"
)

const (
	testAccessKey = "GOSTORETESTKEY"
	testSecretKey = "super-secret-gostore-signing-key"
	testRegion    = "us-east-1"
)

func newAuthTestServer(t *testing.T, opts ...Options) (*httptest.Server, *syncBuffer) {
	t.Helper()
	dir := t.TempDir()
	engine, _, err := storage.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = engine.Close() })

	opt := Options{
		AccessKey: testAccessKey,
		SecretKey: testSecretKey,
		Region:    testRegion,
	}
	if len(opts) > 0 {
		opt = opts[0]
	}
	var logs syncBuffer
	log := slog.New(slog.NewTextHandler(&logs, nil))
	ts := httptest.NewServer(New(log, engine, opt))
	t.Cleanup(ts.Close)
	return ts, &logs
}

func signS3Request(t *testing.T, req *http.Request, payloadHash string, now time.Time, access, secret, region string) {
	t.Helper()
	req.Header.Set("X-Amz-Content-Sha256", payloadHash)
	signer := v4.NewSigner()
	creds := aws.Credentials{AccessKeyID: access, SecretAccessKey: secret}
	if err := signer.SignHTTP(context.Background(), creds, req, payloadHash, "s3", region, now); err != nil {
		t.Fatal(err)
	}
}

func TestAuthRequiredByDefault(t *testing.T) {
	dir := t.TempDir()
	engine, _, err := storage.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer engine.Close()

	// Default Options{} has no credentials and AllowUnauthenticated == false.
	ts := httptest.NewServer(New(slog.New(slog.NewTextHandler(io.Discard, nil)), engine))
	defer ts.Close()

	resp, body := doReq(t, http.MethodGet, ts.URL+"/", nil, nil)
	if resp.StatusCode != http.StatusForbidden || decodeErrorXML(t, body).Code != "AccessDenied" {
		t.Fatalf("expected 403 AccessDenied, got status=%d body=%s", resp.StatusCode, body)
	}

	// Operational health check remains accessible without auth.
	resp, body = doReq(t, http.MethodGet, ts.URL+"/-/healthz", nil, nil)
	if resp.StatusCode != http.StatusOK || string(body) != "ok\n" {
		t.Fatalf("healthz failed: status=%d body=%s", resp.StatusCode, body)
	}
}

func TestPublishedAWSSigV4Examples(t *testing.T) {
	awsDocNow := func() time.Time {
		return time.Date(2013, 5, 24, 0, 0, 0, 0, time.UTC)
	}
	awsDocCfg := sigV4Config{
		accessKey: "AKIAIOSFODNN7EXAMPLE",
		secretKey: "wJalrXUtnFEMI/K7MDENG/bPxRfiCYEXAMPLEKEY",
		region:    "us-east-1",
		service:   "s3",
		clockSkew: 15 * time.Minute,
		now:       awsDocNow,
	}

	t.Run("AWS S3 SigV4 doc GET Object example", func(t *testing.T) {
		req, err := http.NewRequest(http.MethodGet, "https://examplebucket.s3.amazonaws.com/test.txt", nil)
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Range", "bytes=0-9")
		req.Header.Set("X-Amz-Content-Sha256", emptySHA256)
		req.Header.Set("X-Amz-Date", "20130524T000000Z")
		req.Header.Set("Authorization", "AWS4-HMAC-SHA256 Credential=AKIAIOSFODNN7EXAMPLE/20130524/us-east-1/s3/aws4_request, SignedHeaders=host;range;x-amz-content-sha256;x-amz-date, Signature=f0e8bdb87c964420e857bd35b5d6ed310bd44f0170aba48dd91039c6036bdb41")

		if _, authErr := verifySigV4(req, awsDocCfg); authErr != nil {
			t.Fatalf("expected AWS S3 GET Object example to pass, got %+v", authErr)
		}
	})

	t.Run("AWS S3 SigV4 doc PUT Object example", func(t *testing.T) {
		payload := "Welcome to Amazon S3."
		req, err := http.NewRequest(http.MethodPut, "https://examplebucket.s3.amazonaws.com/test$file.text", strings.NewReader(payload))
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Date", "Fri, 24 May 2013 00:00:00 GMT")
		req.Header.Set("X-Amz-Content-Sha256", sha256Hex([]byte(payload)))
		req.Header.Set("X-Amz-Date", "20130524T000000Z")
		req.Header.Set("X-Amz-Storage-Class", "REDUCED_REDUNDANCY")
		req.Header.Set("Authorization", "AWS4-HMAC-SHA256 Credential=AKIAIOSFODNN7EXAMPLE/20130524/us-east-1/s3/aws4_request, SignedHeaders=date;host;x-amz-content-sha256;x-amz-date;x-amz-storage-class, Signature=98ad721746da40c64f1a55b78f14c238d841ea1380cd77a1b5971af0ece108bd")

		if _, authErr := verifySigV4(req, awsDocCfg); authErr != nil {
			t.Fatalf("expected AWS S3 PUT Object example to pass, got %+v", authErr)
		}
	})

	t.Run("aws-sdk-go-v2 standalone vector (es)", func(t *testing.T) {
		req, err := http.NewRequest(http.MethodGet, "https://hostname-clusterkey.us-west-2.es.amazonaws.com/logs-*/_search?pretty=true", nil)
		if err != nil {
			t.Fatal(err)
		}
		req.URL.RawPath = "/logs-%2A/_search"
		req.Header.Set("X-Amz-Date", "19700101T000000Z")
		req.Header.Set("X-Amz-Security-Token", "SESSION")
		req.Header.Set("Authorization", "AWS4-HMAC-SHA256 Credential=AKID/19700101/us-west-2/es/aws4_request, SignedHeaders=host;x-amz-date;x-amz-security-token, Signature=79d0760751907af16f64a537c1242416dacf51204a7dd5284492d15577973b91")

		cfg := sigV4Config{
			accessKey: "AKID",
			secretKey: "SECRET",
			region:    "us-west-2",
			service:   "es",
			clockSkew: 15 * time.Minute,
			now:       func() time.Time { return time.Unix(0, 0).UTC() },
		}
		if _, authErr := verifySigV4(req, cfg); authErr != nil {
			t.Fatalf("expected published vector to pass, got %+v", authErr)
		}
	})

	t.Run("S3 path-style signed GET and PUT with special keys", func(t *testing.T) {
		fixedNow := time.Date(2026, 11, 26, 12, 0, 0, 0, time.UTC)
		ts, _ := newAuthTestServer(t, Options{
			AccessKey: testAccessKey,
			SecretKey: testSecretKey,
			Region:    testRegion,
			Now:       func() time.Time { return fixedNow },
		})

		reqCreate, _ := http.NewRequest(http.MethodPut, ts.URL+"/photos", nil)
		signS3Request(t, reqCreate, emptySHA256, fixedNow, testAccessKey, testSecretKey, testRegion)
		resp, err := http.DefaultClient.Do(reqCreate)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("create bucket status=%d", resp.StatusCode)
		}

		payload := "Welcome to Amazon S3."
		reqPut, _ := http.NewRequest(http.MethodPut, ts.URL+"/photos/test$file.text", strings.NewReader(payload))
		signS3Request(t, reqPut, sha256Hex([]byte(payload)), fixedNow, testAccessKey, testSecretKey, testRegion)
		resp, err = http.DefaultClient.Do(reqPut)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("put status=%d", resp.StatusCode)
		}

		reqGet, _ := http.NewRequest(http.MethodGet, ts.URL+"/photos/test$file.text", nil)
		signS3Request(t, reqGet, emptySHA256, fixedNow, testAccessKey, testSecretKey, testRegion)
		resp, err = http.DefaultClient.Do(reqGet)
		if err != nil {
			t.Fatal(err)
		}
		got, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if resp.StatusCode != http.StatusOK || string(got) != payload {
			t.Fatalf("get status=%d body=%q", resp.StatusCode, got)
		}
	})
}

func TestSigV4TamperedAndErrorCases(t *testing.T) {
	fixedNow := time.Date(2026, 11, 26, 12, 0, 0, 0, time.UTC)
	ts, logs := newAuthTestServer(t, Options{
		AccessKey: testAccessKey,
		SecretKey: testSecretKey,
		Region:    testRegion,
		Now:       func() time.Time { return fixedNow },
	})

	makeSignedPut := func(path, payload string) *http.Request {
		req, err := http.NewRequest(http.MethodPut, ts.URL+path, strings.NewReader(payload))
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("X-Amz-Meta-Note", "hello")
		signS3Request(t, req, sha256Hex([]byte(payload)), fixedNow, testAccessKey, testSecretKey, testRegion)
		return req
	}

	// Setup bucket with valid signed request.
	reqBucket, _ := http.NewRequest(http.MethodPut, ts.URL+"/photos", nil)
	signS3Request(t, reqBucket, emptySHA256, fixedNow, testAccessKey, testSecretKey, testRegion)
	resp, err := http.DefaultClient.Do(reqBucket)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()

	tests := []struct {
		name       string
		build      func() *http.Request
		wantStatus int
		wantCode   string
	}{
		{
			name: "missing authorization",
			build: func() *http.Request {
				r, _ := http.NewRequest(http.MethodGet, ts.URL+"/", nil)
				return r
			},
			wantStatus: http.StatusForbidden,
			wantCode:   "AccessDenied",
		},
		{
			name: "wrong access key",
			build: func() *http.Request {
				r, _ := http.NewRequest(http.MethodGet, ts.URL+"/", nil)
				signS3Request(t, r, emptySHA256, fixedNow, "WRONGKEY", testSecretKey, testRegion)
				return r
			},
			wantStatus: http.StatusForbidden,
			wantCode:   "InvalidAccessKeyId",
		},
		{
			name: "wrong secret key",
			build: func() *http.Request {
				r, _ := http.NewRequest(http.MethodGet, ts.URL+"/", nil)
				signS3Request(t, r, emptySHA256, fixedNow, testAccessKey, "wrong-secret", testRegion)
				return r
			},
			wantStatus: http.StatusForbidden,
			wantCode:   "SignatureDoesNotMatch",
		},
		{
			name: "wrong region",
			build: func() *http.Request {
				r, _ := http.NewRequest(http.MethodGet, ts.URL+"/", nil)
				signS3Request(t, r, emptySHA256, fixedNow, testAccessKey, testSecretKey, "eu-west-1")
				return r
			},
			wantStatus: http.StatusBadRequest,
			wantCode:   "AuthorizationHeaderMalformed",
		},
		{
			name: "wrong service",
			build: func() *http.Request {
				r, _ := http.NewRequest(http.MethodGet, ts.URL+"/", nil)
				r.Header.Set("X-Amz-Content-Sha256", emptySHA256)
				signer := v4.NewSigner()
				_ = signer.SignHTTP(context.Background(), aws.Credentials{AccessKeyID: testAccessKey, SecretAccessKey: testSecretKey}, r, emptySHA256, "ec2", testRegion, fixedNow)
				return r
			},
			wantStatus: http.StatusBadRequest,
			wantCode:   "AuthorizationHeaderMalformed",
		},
		{
			name: "clock skew past",
			build: func() *http.Request {
				r, _ := http.NewRequest(http.MethodGet, ts.URL+"/", nil)
				signS3Request(t, r, emptySHA256, fixedNow.Add(-16*time.Minute), testAccessKey, testSecretKey, testRegion)
				return r
			},
			wantStatus: http.StatusForbidden,
			wantCode:   "RequestTimeTooSkewed",
		},
		{
			name: "clock skew future",
			build: func() *http.Request {
				r, _ := http.NewRequest(http.MethodGet, ts.URL+"/", nil)
				signS3Request(t, r, emptySHA256, fixedNow.Add(16*time.Minute), testAccessKey, testSecretKey, testRegion)
				return r
			},
			wantStatus: http.StatusForbidden,
			wantCode:   "RequestTimeTooSkewed",
		},
		{
			name: "tampered path after signing",
			build: func() *http.Request {
				r := makeSignedPut("/photos/a.txt", "hello")
				r.URL.Path = "/photos/b.txt"
				return r
			},
			wantStatus: http.StatusForbidden,
			wantCode:   "SignatureDoesNotMatch",
		},
		{
			name: "tampered query after signing",
			build: func() *http.Request {
				r, _ := http.NewRequest(http.MethodGet, ts.URL+"/photos?list-type=2&prefix=a", nil)
				signS3Request(t, r, emptySHA256, fixedNow, testAccessKey, testSecretKey, testRegion)
				r.URL.RawQuery = "list-type=2&prefix=b"
				return r
			},
			wantStatus: http.StatusForbidden,
			wantCode:   "SignatureDoesNotMatch",
		},
		{
			name: "tampered signed header after signing",
			build: func() *http.Request {
				r := makeSignedPut("/photos/a.txt", "hello")
				r.Header.Set("X-Amz-Meta-Note", "tampered")
				return r
			},
			wantStatus: http.StatusForbidden,
			wantCode:   "SignatureDoesNotMatch",
		},
		{
			name: "tampered body after signing",
			build: func() *http.Request {
				r := makeSignedPut("/photos/a.txt", "hello")
				r.Body = io.NopCloser(strings.NewReader("world"))
				return r
			},
			wantStatus: http.StatusBadRequest,
			wantCode:   "XAmzContentSHA256Mismatch",
		},
		{
			name: "missing x-amz-content-sha256",
			build: func() *http.Request {
				r, _ := http.NewRequest(http.MethodGet, ts.URL+"/", nil)
				signer := v4.NewSigner()
				_ = signer.SignHTTP(context.Background(), aws.Credentials{AccessKeyID: testAccessKey, SecretAccessKey: testSecretKey}, r, emptySHA256, "s3", testRegion, fixedNow)
				return r
			},
			wantStatus: http.StatusBadRequest,
			wantCode:   "InvalidRequest",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req := tt.build()
			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				t.Fatal(err)
			}
			body, _ := io.ReadAll(resp.Body)
			resp.Body.Close()

			if resp.StatusCode != tt.wantStatus || decodeErrorXML(t, body).Code != tt.wantCode {
				t.Fatalf("got status=%d body=%s, want %d %s", resp.StatusCode, body, tt.wantStatus, tt.wantCode)
			}
			if bytes.Contains(body, []byte(testSecretKey)) {
				t.Fatal("error response leaked secret key")
			}
		})
	}

	if strings.Contains(logs.String(), testSecretKey) {
		t.Fatal("logs leaked secret key")
	}
}

func buildSignedAWSChunkedBody(t *testing.T, chunks [][]byte, trailers map[string]string, seedSig string, signingKey []byte, amzDate, scope string) []byte {
	t.Helper()
	var buf bytes.Buffer
	prevSig := seedSig

	signChunk := func(data []byte) string {
		sts := "AWS4-HMAC-SHA256-PAYLOAD\n" + amzDate + "\n" + scope + "\n" + prevSig + "\n" + emptySHA256 + "\n" + sha256Hex(data)
		sig := hex.EncodeToString(hmacSHA256(signingKey, []byte(sts)))
		prevSig = sig
		return sig
	}

	for _, ch := range chunks {
		sig := signChunk(ch)
		fmt.Fprintf(&buf, "%x;chunk-signature=%s\r\n", len(ch), sig)
		buf.Write(ch)
		buf.WriteString("\r\n")
	}
	zeroSig := signChunk(nil)
	fmt.Fprintf(&buf, "0;chunk-signature=%s\r\n", zeroSig)

	if len(trailers) > 0 {
		var keys []string
		for k := range trailers {
			keys = append(keys, k)
		}
		// Sort trailer keys for canonical trailer string.
		for i := 0; i < len(keys); i++ {
			for j := i + 1; j < len(keys); j++ {
				if keys[j] < keys[i] {
					keys[i], keys[j] = keys[j], keys[i]
				}
			}
		}
		var canon strings.Builder
		for _, k := range keys {
			fmt.Fprintf(&buf, "%s:%s\r\n", k, trailers[k])
			canon.WriteString(k)
			canon.WriteByte(':')
			canon.WriteString(trailers[k])
			canon.WriteByte('\n')
		}
		sts := "AWS4-HMAC-SHA256-TRAILER\n" + amzDate + "\n" + scope + "\n" + prevSig + "\n" + sha256Hex([]byte(canon.String()))
		tSig := hex.EncodeToString(hmacSHA256(signingKey, []byte(sts)))
		fmt.Fprintf(&buf, "x-amz-trailer-signature:%s\r\n", tSig)
	}
	buf.WriteString("\r\n")
	return buf.Bytes()
}

func buildUnsignedAWSChunkedBody(chunks [][]byte, trailers map[string]string) []byte {
	var buf bytes.Buffer
	for _, ch := range chunks {
		fmt.Fprintf(&buf, "%x\r\n", len(ch))
		buf.Write(ch)
		buf.WriteString("\r\n")
	}
	buf.WriteString("0\r\n")
	for k, v := range trailers {
		fmt.Fprintf(&buf, "%s:%s\r\n", k, v)
	}
	buf.WriteString("\r\n")
	return buf.Bytes()
}

func TestAWSChunkedPayloadsAndTrailers(t *testing.T) {
	fixedNow := time.Date(2026, 11, 26, 12, 0, 0, 0, time.UTC)
	ts, _ := newAuthTestServer(t, Options{
		AccessKey: testAccessKey,
		SecretKey: testSecretKey,
		Region:    testRegion,
		Now:       func() time.Time { return fixedNow },
	})

	reqBucket, _ := http.NewRequest(http.MethodPut, ts.URL+"/photos", nil)
	signS3Request(t, reqBucket, emptySHA256, fixedNow, testAccessKey, testSecretKey, testRegion)
	resp, err := http.DefaultClient.Do(reqBucket)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()

	chunks := [][]byte{[]byte("hello "), []byte("chunked "), []byte("world!")}
	full := bytes.Join(chunks, nil)
	amzDate := fixedNow.Format("20060102T150405Z")
	date := fixedNow.Format("20060102")
	scope := date + "/" + testRegion + "/s3/aws4_request"
	signingKey := deriveSigningKey(testSecretKey, date, testRegion, "s3")

	extractSig := func(authHdr string) string {
		_, _, sig, err := parseAuthorization(authHdr)
		if err != nil {
			t.Fatal(err)
		}
		return sig
	}

	t.Run("signed aws-chunked upload", func(t *testing.T) {
		req, _ := http.NewRequest(http.MethodPut, ts.URL+"/photos/signed-chunked.txt", nil)
		req.Header.Set("Content-Encoding", "aws-chunked")
		req.Header.Set("X-Amz-Decoded-Content-Length", strconv.Itoa(len(full)))
		signS3Request(t, req, payloadChunkedSigned, fixedNow, testAccessKey, testSecretKey, testRegion)
		wire := buildSignedAWSChunkedBody(t, chunks, nil, extractSig(req.Header.Get("Authorization")), signingKey, amzDate, scope)
		req.Body = io.NopCloser(bytes.NewReader(wire))
		req.ContentLength = int64(len(wire))

		r, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		b, _ := io.ReadAll(r.Body)
		r.Body.Close()
		if r.StatusCode != http.StatusOK {
			t.Fatalf("status=%d body=%s", r.StatusCode, b)
		}
	})

	t.Run("signed aws-chunked tampered chunk fails", func(t *testing.T) {
		req, _ := http.NewRequest(http.MethodPut, ts.URL+"/photos/bad-chunk.txt", nil)
		req.Header.Set("Content-Encoding", "aws-chunked")
		req.Header.Set("X-Amz-Decoded-Content-Length", strconv.Itoa(len(full)))
		signS3Request(t, req, payloadChunkedSigned, fixedNow, testAccessKey, testSecretKey, testRegion)
		wire := buildSignedAWSChunkedBody(t, chunks, nil, extractSig(req.Header.Get("Authorization")), signingKey, amzDate, scope)
		// Corrupt one byte in the first chunk's payload.
		idx := bytes.Index(wire, []byte("hello "))
		wire[idx] = 'X'
		req.Body = io.NopCloser(bytes.NewReader(wire))
		req.ContentLength = int64(len(wire))

		r, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		b, _ := io.ReadAll(r.Body)
		r.Body.Close()
		if r.StatusCode != http.StatusForbidden || decodeErrorXML(t, b).Code != "SignatureDoesNotMatch" {
			t.Fatalf("expected 403 SignatureDoesNotMatch, got status=%d body=%s", r.StatusCode, b)
		}
	})

	t.Run("signed aws-chunked with checksum trailer", func(t *testing.T) {
		crc := crc32.NewIEEE()
		_, _ = crc.Write(full)
		crcB64 := base64.StdEncoding.EncodeToString(crc.Sum(nil))
		trailers := map[string]string{"x-amz-checksum-crc32": crcB64}

		req, _ := http.NewRequest(http.MethodPut, ts.URL+"/photos/signed-trailer.txt", nil)
		req.Header.Set("Content-Encoding", "aws-chunked")
		req.Header.Set("X-Amz-Decoded-Content-Length", strconv.Itoa(len(full)))
		req.Header.Set("X-Amz-Trailer", "x-amz-checksum-crc32")
		signS3Request(t, req, payloadChunkedTrailer, fixedNow, testAccessKey, testSecretKey, testRegion)
		wire := buildSignedAWSChunkedBody(t, chunks, trailers, extractSig(req.Header.Get("Authorization")), signingKey, amzDate, scope)
		req.Body = io.NopCloser(bytes.NewReader(wire))
		req.ContentLength = int64(len(wire))

		r, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		b, _ := io.ReadAll(r.Body)
		r.Body.Close()
		if r.StatusCode != http.StatusOK {
			t.Fatalf("status=%d body=%s", r.StatusCode, b)
		}
	})

	t.Run("unsigned aws-chunked with crc64nvme trailer and bad checksum rejection", func(t *testing.T) {
		c64 := crc64.New(crc64.MakeTable(crc64NVMEPoly))
		_, _ = c64.Write(full)
		goodB64 := base64.StdEncoding.EncodeToString(c64.Sum(nil))

		send := func(val string) (int, []byte) {
			req, _ := http.NewRequest(http.MethodPut, ts.URL+"/photos/unsigned-trailer.txt", nil)
			req.Header.Set("Content-Encoding", "aws-chunked")
			req.Header.Set("X-Amz-Decoded-Content-Length", strconv.Itoa(len(full)))
			req.Header.Set("X-Amz-Trailer", "x-amz-checksum-crc64nvme")
			signS3Request(t, req, payloadChunkedUnsigned, fixedNow, testAccessKey, testSecretKey, testRegion)
			wire := buildUnsignedAWSChunkedBody(chunks, map[string]string{"x-amz-checksum-crc64nvme": val})
			req.Body = io.NopCloser(bytes.NewReader(wire))
			req.ContentLength = int64(len(wire))

			r, err := http.DefaultClient.Do(req)
			if err != nil {
				t.Fatal(err)
			}
			b, _ := io.ReadAll(r.Body)
			r.Body.Close()
			return r.StatusCode, b
		}

		status, body := send(goodB64)
		if status != http.StatusOK {
			t.Fatalf("valid unsigned trailer: status=%d body=%s", status, body)
		}
		status, body = send("AAAAAAAAAAA=")
		if status != http.StatusBadRequest || decodeErrorXML(t, body).Code != "BadDigest" {
			t.Fatalf("bad trailer checksum: status=%d body=%s", status, body)
		}
	})
}

func TestAWSSDKGoV2Integration(t *testing.T) {
	ts, _ := newAuthTestServer(t)

	newClient := func(access, secret string) *s3.Client {
		cfg := aws.Config{
			Region:      testRegion,
			Credentials: credentials.NewStaticCredentialsProvider(access, secret, ""),
		}
		return s3.NewFromConfig(cfg, func(o *s3.Options) {
			o.BaseEndpoint = aws.String(ts.URL)
			o.UsePathStyle = true
		})
	}

	ctx := context.Background()
	client := newClient(testAccessKey, testSecretKey)

	// Wrong credentials must fail.
	if _, err := newClient(testAccessKey, "wrong-secret").ListBuckets(ctx, &s3.ListBucketsInput{}); err == nil {
		t.Fatal("expected error with wrong secret key")
	}

	// CreateBucket & ListBuckets.
	if _, err := client.CreateBucket(ctx, &s3.CreateBucketInput{Bucket: aws.String("sdk-bucket")}); err != nil {
		t.Fatalf("CreateBucket: %v", err)
	}
	bucketsOut, err := client.ListBuckets(ctx, &s3.ListBucketsInput{})
	if err != nil || len(bucketsOut.Buckets) != 1 || aws.ToString(bucketsOut.Buckets[0].Name) != "sdk-bucket" {
		t.Fatalf("ListBuckets: %+v, err=%v", bucketsOut, err)
	}

	// PutObject: small object with metadata + larger streaming object (>128 KiB).
	largeData := bytes.Repeat([]byte("gostore-sdk-v2-"), 10_000)
	objects := map[string][]byte{
		"dir/hello.txt": []byte("hello from aws-sdk-go-v2"),
		"dir/large.bin": largeData,
	}
	for k, data := range objects {
		_, err := client.PutObject(ctx, &s3.PutObjectInput{
			Bucket:            aws.String("sdk-bucket"),
			Key:               aws.String(k),
			Body:              bytes.NewReader(data),
			ContentType:       aws.String("application/octet-stream"),
			Metadata:          map[string]string{"sdk": "go-v2"},
			ChecksumAlgorithm: s3types.ChecksumAlgorithmCrc32,
		})
		if err != nil {
			t.Fatalf("PutObject(%s): %v", k, err)
		}
	}

	// ListObjectsV2.
	listOut, err := client.ListObjectsV2(ctx, &s3.ListObjectsV2Input{
		Bucket: aws.String("sdk-bucket"),
		Prefix: aws.String("dir/"),
	})
	if err != nil || len(listOut.Contents) != 2 {
		t.Fatalf("ListObjectsV2: %+v, err=%v", listOut, err)
	}

	// HeadObject & GetObject (full + range).
	headOut, err := client.HeadObject(ctx, &s3.HeadObjectInput{
		Bucket: aws.String("sdk-bucket"),
		Key:    aws.String("dir/hello.txt"),
	})
	if err != nil || aws.ToInt64(headOut.ContentLength) != int64(len(objects["dir/hello.txt"])) || headOut.Metadata["sdk"] != "go-v2" {
		t.Fatalf("HeadObject: %+v, err=%v", headOut, err)
	}

	getOut, err := client.GetObject(ctx, &s3.GetObjectInput{
		Bucket: aws.String("sdk-bucket"),
		Key:    aws.String("dir/large.bin"),
	})
	if err != nil {
		t.Fatalf("GetObject: %v", err)
	}
	gotLarge, err := io.ReadAll(getOut.Body)
	_ = getOut.Body.Close()
	if err != nil || !bytes.Equal(gotLarge, largeData) {
		t.Fatalf("GetObject large mismatch: len=%d err=%v", len(gotLarge), err)
	}

	rangeOut, err := client.GetObject(ctx, &s3.GetObjectInput{
		Bucket: aws.String("sdk-bucket"),
		Key:    aws.String("dir/hello.txt"),
		Range:  aws.String("bytes=0-4"),
	})
	if err != nil {
		t.Fatalf("GetObject range: %v", err)
	}
	gotRange, _ := io.ReadAll(rangeOut.Body)
	_ = rangeOut.Body.Close()
	if string(gotRange) != "hello" {
		t.Fatalf("range got %q", gotRange)
	}

	// DeleteObject.
	for k := range objects {
		if _, err := client.DeleteObject(ctx, &s3.DeleteObjectInput{
			Bucket: aws.String("sdk-bucket"),
			Key:    aws.String(k),
		}); err != nil {
			t.Fatalf("DeleteObject(%s): %v", k, err)
		}
	}

	// Multipart upload through AWS SDK for Go v2 (using small MinPartSize via a second server?
	// No: a 1-part upload or >= 5 MiB parts works on default MinPartSize! Test 5 MiB + 1 KiB).
	createOut, err := client.CreateMultipartUpload(ctx, &s3.CreateMultipartUploadInput{
		Bucket:      aws.String("sdk-bucket"),
		Key:         aws.String("mp.bin"),
		ContentType: aws.String("application/octet-stream"),
	})
	if err != nil {
		t.Fatalf("CreateMultipartUpload: %v", err)
	}
	mpPart1 := bytes.Repeat([]byte("A"), 5<<20)
	mpPart2 := bytes.Repeat([]byte("B"), 1024)
	up1, err := client.UploadPart(ctx, &s3.UploadPartInput{
		Bucket:     aws.String("sdk-bucket"),
		Key:        aws.String("mp.bin"),
		UploadId:   createOut.UploadId,
		PartNumber: aws.Int32(1),
		Body:       bytes.NewReader(mpPart1),
	})
	if err != nil {
		t.Fatalf("UploadPart 1: %v", err)
	}
	up2, err := client.UploadPart(ctx, &s3.UploadPartInput{
		Bucket:     aws.String("sdk-bucket"),
		Key:        aws.String("mp.bin"),
		UploadId:   createOut.UploadId,
		PartNumber: aws.Int32(2),
		Body:       bytes.NewReader(mpPart2),
	})
	if err != nil {
		t.Fatalf("UploadPart 2: %v", err)
	}
	lpOut, err := client.ListParts(ctx, &s3.ListPartsInput{
		Bucket:   aws.String("sdk-bucket"),
		Key:      aws.String("mp.bin"),
		UploadId: createOut.UploadId,
	})
	if err != nil || len(lpOut.Parts) != 2 {
		t.Fatalf("ListParts: %+v, err=%v", lpOut, err)
	}
	compOut, err := client.CompleteMultipartUpload(ctx, &s3.CompleteMultipartUploadInput{
		Bucket:   aws.String("sdk-bucket"),
		Key:      aws.String("mp.bin"),
		UploadId: createOut.UploadId,
		MultipartUpload: &s3types.CompletedMultipartUpload{
			Parts: []s3types.CompletedPart{
				{PartNumber: aws.Int32(1), ETag: up1.ETag},
				{PartNumber: aws.Int32(2), ETag: up2.ETag},
			},
		},
	})
	if err != nil || !strings.HasSuffix(aws.ToString(compOut.ETag), `-2"`) {
		t.Fatalf("CompleteMultipartUpload: %+v, err=%v", compOut, err)
	}
	mpGet, err := client.GetObject(ctx, &s3.GetObjectInput{
		Bucket: aws.String("sdk-bucket"),
		Key:    aws.String("mp.bin"),
	})
	if err != nil {
		t.Fatalf("GetObject multipart: %v", err)
	}
	mpGot, err := io.ReadAll(mpGet.Body)
	_ = mpGet.Body.Close()
	if err != nil || len(mpGot) != len(mpPart1)+len(mpPart2) || !bytes.Equal(mpGot[:len(mpPart1)], mpPart1) || !bytes.Equal(mpGot[len(mpPart1):], mpPart2) {
		t.Fatalf("multipart content mismatch: len=%d err=%v", len(mpGot), err)
	}
	if _, err := client.DeleteObject(ctx, &s3.DeleteObjectInput{
		Bucket: aws.String("sdk-bucket"),
		Key:    aws.String("mp.bin"),
	}); err != nil {
		t.Fatalf("DeleteObject(mp.bin): %v", err)
	}

	if _, err := client.DeleteBucket(ctx, &s3.DeleteBucketInput{Bucket: aws.String("sdk-bucket")}); err != nil {
		t.Fatalf("DeleteBucket: %v", err)
	}
}
