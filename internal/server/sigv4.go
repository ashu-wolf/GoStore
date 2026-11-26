//go:build linux

package server

import (
	"crypto/hmac"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"fmt"
	"net/http"
	"net/url"
	"path"
	"slices"
	"strings"
	"time"
)

const (
	sigV4Algorithm   = "AWS4-HMAC-SHA256"
	defaultClockSkew = 15 * time.Minute
	emptySHA256      = "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855"

	payloadSigned          = "signed"
	payloadUnsigned        = "UNSIGNED-PAYLOAD"
	payloadChunkedSigned   = "STREAMING-AWS4-HMAC-SHA256-PAYLOAD"
	payloadChunkedTrailer  = "STREAMING-AWS4-HMAC-SHA256-PAYLOAD-TRAILER"
	payloadChunkedUnsigned = "STREAMING-UNSIGNED-PAYLOAD-TRAILER"
)

type sigV4Config struct {
	accessKey string
	secretKey string
	region    string
	service   string
	clockSkew time.Duration
	now       func() time.Time
}

type authResult struct {
	signingKey  []byte
	amzDate     string
	scope       string
	signature   string
	payloadMode string
	payloadHash string
}

type authError struct {
	status  int
	code    string
	message string
}

func verifySigV4(r *http.Request, cfg sigV4Config) (authResult, *authError) {
	if cfg.accessKey == "" || cfg.secretKey == "" {
		return authResult{}, &authError{http.StatusForbidden, "AccessDenied", "Access Denied"}
	}

	authHdr := r.Header.Get("Authorization")
	if authHdr == "" {
		return authResult{}, &authError{http.StatusForbidden, "AccessDenied", "Access Denied"}
	}
	creds, signedHeaders, gotSigHex, err := parseAuthorization(authHdr)
	if err != nil {
		return authResult{}, &authError{http.StatusBadRequest, "AuthorizationHeaderMalformed", err.Error()}
	}

	parts := strings.Split(creds, "/")
	if len(parts) != 5 || parts[4] != "aws4_request" || len(parts[0]) == 0 {
		return authResult{}, &authError{http.StatusBadRequest, "AuthorizationHeaderMalformed", "The authorization header is malformed; the credential is invalid."}
	}
	accessKey, credDate, region, service := parts[0], parts[1], parts[2], parts[3]

	if !constantTimeStringEqual(accessKey, cfg.accessKey) {
		return authResult{}, &authError{http.StatusForbidden, "InvalidAccessKeyId", "The AWS Access Key Id you provided does not exist in our records."}
	}
	if region != cfg.region {
		return authResult{}, &authError{http.StatusBadRequest, "AuthorizationHeaderMalformed", fmt.Sprintf("The authorization header is malformed; the region '%s' is wrong; expecting '%s'", region, cfg.region)}
	}
	if service != cfg.service {
		return authResult{}, &authError{http.StatusBadRequest, "AuthorizationHeaderMalformed", fmt.Sprintf("The authorization header is malformed; the service '%s' is wrong; expecting '%s'", service, cfg.service)}
	}

	reqTime, amzDate, dateErr := parseRequestDate(r)
	if dateErr != nil {
		return authResult{}, &authError{http.StatusForbidden, "AccessDenied", dateErr.Error()}
	}
	if reqTime.UTC().Format("20060102") != credDate {
		return authResult{}, &authError{http.StatusBadRequest, "AuthorizationHeaderMalformed", "The authorization header is malformed; the credential date does not match the request date."}
	}
	now := cfg.now()
	skew := now.Sub(reqTime)
	if skew < 0 {
		skew = -skew
	}
	if skew > cfg.clockSkew {
		return authResult{}, &authError{http.StatusForbidden, "RequestTimeTooSkewed", "The difference between the request time and the current time is too large."}
	}

	if err := validateSignedHeaders(r, signedHeaders); err != nil {
		return authResult{}, &authError{http.StatusBadRequest, "AuthorizationHeaderMalformed", err.Error()}
	}

	payloadHdr := r.Header.Get("X-Amz-Content-Sha256")
	payloadMode, err := classifyPayloadHeader(payloadHdr, cfg.service)
	if err != nil {
		return authResult{}, &authError{http.StatusBadRequest, "InvalidRequest", err.Error()}
	}

	gotSig, err := hex.DecodeString(gotSigHex)
	if err != nil || len(gotSig) != sha256.Size {
		return authResult{}, &authError{http.StatusBadRequest, "AuthorizationHeaderMalformed", "The authorization header is malformed; signature must be 64 hex characters."}
	}

	scope := credDate + "/" + region + "/" + service + "/aws4_request"
	canonReq, err := buildCanonicalRequest(r, signedHeaders, payloadHdr, cfg.service != "s3")
	if err != nil {
		return authResult{}, &authError{http.StatusBadRequest, "InvalidURI", err.Error()}
	}
	canonHash := sha256Hex([]byte(canonReq))
	stringToSign := sigV4Algorithm + "\n" + amzDate + "\n" + scope + "\n" + canonHash

	signingKey := deriveSigningKey(cfg.secretKey, credDate, region, service)
	wantSig := hmacSHA256(signingKey, []byte(stringToSign))

	if !hmac.Equal(gotSig, wantSig) {
		return authResult{}, &authError{http.StatusForbidden, "SignatureDoesNotMatch", "The request signature we calculated does not match the signature you provided."}
	}

	return authResult{
		signingKey:  signingKey,
		amzDate:     amzDate,
		scope:       scope,
		signature:   hex.EncodeToString(wantSig),
		payloadMode: payloadMode,
		payloadHash: payloadHdr,
	}, nil
}

func parseAuthorization(hdr string) (credential string, signedHeaders []string, signature string, err error) {
	rest, ok := strings.CutPrefix(hdr, sigV4Algorithm+" ")
	if !ok {
		return "", nil, "", fmt.Errorf("unsupported authorization mechanism")
	}
	var rawSigned string
	for _, part := range strings.Split(rest, ",") {
		part = strings.TrimSpace(part)
		k, v, ok := strings.Cut(part, "=")
		if !ok {
			return "", nil, "", fmt.Errorf("malformed authorization parameter")
		}
		switch k {
		case "Credential":
			credential = v
		case "SignedHeaders":
			rawSigned = v
		case "Signature":
			signature = v
		default:
			return "", nil, "", fmt.Errorf("unknown authorization parameter %q", k)
		}
	}
	if credential == "" || rawSigned == "" || signature == "" {
		return "", nil, "", fmt.Errorf("missing authorization parameters")
	}
	signedHeaders = strings.Split(rawSigned, ";")
	return credential, signedHeaders, signature, nil
}

func parseRequestDate(r *http.Request) (time.Time, string, error) {
	if raw := r.Header.Get("X-Amz-Date"); raw != "" {
		t, err := time.Parse("20060102T150405Z", raw)
		if err != nil {
			return time.Time{}, "", fmt.Errorf("invalid X-Amz-Date header")
		}
		return t, raw, nil
	}
	if raw := r.Header.Get("Date"); raw != "" {
		t, err := http.ParseTime(raw)
		if err != nil {
			return time.Time{}, "", fmt.Errorf("invalid Date header")
		}
		return t.UTC(), t.UTC().Format("20060102T150405Z"), nil
	}
	return time.Time{}, "", fmt.Errorf("AWS authentication requires a valid Date or x-amz-date header")
}

func validateSignedHeaders(r *http.Request, headers []string) error {
	if len(headers) == 0 {
		return fmt.Errorf("missing SignedHeaders")
	}
	hasHost := false
	hasDate := false
	hasAmzDate := r.Header.Get("X-Amz-Date") != ""
	hasContentSHA := r.Header.Get("X-Amz-Content-Sha256") != ""
	signedContentSHA := false

	prev := ""
	for i, h := range headers {
		if h == "" || h != strings.ToLower(h) {
			return fmt.Errorf("SignedHeaders must be lowercase")
		}
		if i > 0 && h <= prev {
			return fmt.Errorf("SignedHeaders must be sorted without duplicates")
		}
		prev = h
		switch h {
		case "host":
			hasHost = true
		case "x-amz-date":
			hasDate = true
		case "date":
			if !hasAmzDate {
				hasDate = true
			}
		case "x-amz-content-sha256":
			signedContentSHA = true
		}
	}
	if !hasHost || !hasDate || (hasContentSHA && !signedContentSHA) {
		return fmt.Errorf("required headers must be included in SignedHeaders")
	}
	return nil
}

func classifyPayloadHeader(hdr, service string) (string, error) {
	if hdr == "" {
		if service == "s3" {
			return "", fmt.Errorf("missing required header: x-amz-content-sha256")
		}
		return payloadSigned, nil
	}
	switch hdr {
	case payloadUnsigned, payloadChunkedSigned, payloadChunkedTrailer, payloadChunkedUnsigned:
		return hdr, nil
	default:
		if len(hdr) != 64 || hdr != strings.ToLower(hdr) {
			return "", fmt.Errorf("invalid x-amz-content-sha256 header")
		}
		if _, err := hex.DecodeString(hdr); err != nil {
			return "", fmt.Errorf("invalid x-amz-content-sha256 header")
		}
		return payloadSigned, nil
	}
}

func buildCanonicalRequest(r *http.Request, signedHeaders []string, payloadHash string, normalizeURI bool) (string, error) {
	canonURI, err := canonicalURI(r, normalizeURI)
	if err != nil {
		return "", err
	}
	canonQuery, err := canonicalQueryString(r.URL.RawQuery)
	if err != nil {
		return "", err
	}

	var b strings.Builder
	b.WriteString(r.Method)
	b.WriteByte('\n')
	b.WriteString(canonURI)
	b.WriteByte('\n')
	b.WriteString(canonQuery)
	b.WriteByte('\n')

	for _, h := range signedHeaders {
		b.WriteString(h)
		b.WriteByte(':')
		vals := headerValues(r, h)
		for i, v := range vals {
			if i > 0 {
				b.WriteByte(',')
			}
			b.WriteString(collapseWhitespace(v))
		}
		b.WriteByte('\n')
	}
	b.WriteByte('\n')
	b.WriteString(strings.Join(signedHeaders, ";"))
	b.WriteByte('\n')
	if payloadHash == "" {
		b.WriteString(emptySHA256)
	} else {
		b.WriteString(payloadHash)
	}
	return b.String(), nil
}

func headerValues(r *http.Request, lowerName string) []string {
	if lowerName == "host" {
		if r.Host != "" {
			return []string{r.Host}
		}
		return r.Header.Values("Host")
	}
	// Go's http.Header stores canonicalized keys; scan case-insensitively to
	// handle non-canonical test headers cleanly too.
	var out []string
	for k, vals := range r.Header {
		if strings.ToLower(k) == lowerName {
			out = append(out, vals...)
		}
	}
	return out
}

func collapseWhitespace(s string) string {
	s = strings.TrimSpace(s)
	var b strings.Builder
	b.Grow(len(s))
	inSpace := false
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c == ' ' || c == '\t' {
			if !inSpace {
				b.WriteByte(' ')
				inSpace = true
			}
			continue
		}
		inSpace = false
		b.WriteByte(c)
	}
	return b.String()
}

func canonicalURI(r *http.Request, normalize bool) (string, error) {
	rawPath := r.URL.EscapedPath()
	if rawPath == "" {
		return "/", nil
	}
	if normalize {
		trailingSlash := len(rawPath) > 1 && strings.HasSuffix(rawPath, "/")
		cleaned := path.Clean(rawPath)
		if trailingSlash && cleaned != "/" {
			cleaned += "/"
		}
		return awsURIEncode(cleaned, false), nil
	}

	segs := strings.Split(rawPath, "/")
	for i, seg := range segs {
		unescaped, err := url.PathUnescape(seg)
		if err != nil {
			return "", fmt.Errorf("invalid path encoding")
		}
		segs[i] = awsURIEncode(unescaped, true)
	}
	return strings.Join(segs, "/"), nil
}

func canonicalQueryString(rawQuery string) (string, error) {
	if rawQuery == "" {
		return "", nil
	}
	type pair struct{ k, v string }
	parts := strings.Split(rawQuery, "&")
	pairs := make([]pair, 0, len(parts))
	for _, p := range parts {
		kRaw, vRaw, _ := strings.Cut(p, "=")
		k, err := url.QueryUnescape(kRaw)
		if err != nil {
			return "", fmt.Errorf("invalid query key")
		}
		v, err := url.QueryUnescape(vRaw)
		if err != nil {
			return "", fmt.Errorf("invalid query value")
		}
		pairs = append(pairs, pair{
			k: awsURIEncode(k, true),
			v: awsURIEncode(v, true),
		})
	}
	slices.SortFunc(pairs, func(a, b pair) int {
		if c := strings.Compare(a.k, b.k); c != 0 {
			return c
		}
		return strings.Compare(a.v, b.v)
	})
	var b strings.Builder
	for i, p := range pairs {
		if i > 0 {
			b.WriteByte('&')
		}
		b.WriteString(p.k)
		b.WriteByte('=')
		b.WriteString(p.v)
	}
	return b.String(), nil
}

func awsURIEncode(s string, encodeSlash bool) string {
	var b strings.Builder
	b.Grow(len(s))
	const hexDigits = "0123456789ABCDEF"
	for i := 0; i < len(s); i++ {
		c := s[i]
		isUnreserved := (c >= 'A' && c <= 'Z') ||
			(c >= 'a' && c <= 'z') ||
			(c >= '0' && c <= '9') ||
			c == '_' || c == '-' || c == '~' || c == '.'
		if isUnreserved || (c == '/' && !encodeSlash) {
			b.WriteByte(c)
			continue
		}
		b.WriteByte('%')
		b.WriteByte(hexDigits[c>>4])
		b.WriteByte(hexDigits[c&0x0f])
	}
	return b.String()
}

func deriveSigningKey(secret, date, region, service string) []byte {
	kDate := hmacSHA256([]byte("AWS4"+secret), []byte(date))
	kRegion := hmacSHA256(kDate, []byte(region))
	kService := hmacSHA256(kRegion, []byte(service))
	return hmacSHA256(kService, []byte("aws4_request"))
}

func hmacSHA256(key, data []byte) []byte {
	mac := hmac.New(sha256.New, key)
	_, _ = mac.Write(data)
	return mac.Sum(nil)
}

func sha256Hex(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

func constantTimeStringEqual(a, b string) bool {
	return subtle.ConstantTimeCompare([]byte(a), []byte(b)) == 1
}
