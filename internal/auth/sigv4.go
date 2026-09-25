// Package auth verifies AWS Signature Version 4 requests.
// The signature covers the request the client claims to send. Callers still
// have to check that the body matches x-amz-content-sha256 when that header
// is a real digest.
package auth

import (
	"crypto/hmac"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"
)

const (
	Algorithm        = "AWS4-HMAC-SHA256"
	emptyPayloadHash = "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855"
	unsignedPayload  = "UNSIGNED-PAYLOAD"
	streamingPayload = "STREAMING-AWS4-HMAC-SHA256-PAYLOAD"
	streamingTrailer = "STREAMING-AWS4-HMAC-SHA256-PAYLOAD-TRAILER"
	unsignedTrailer  = "STREAMING-UNSIGNED-PAYLOAD-TRAILER"
	maxPresignAge    = 90 * 24 * time.Hour
	service          = "s3"
)

var (
	ErrMissingAuth      = errors.New("missing authentication")
	ErrInvalidAccessKey = errors.New("invalid access key")
	ErrSignature        = errors.New("signature mismatch")
	ErrExpired          = errors.New("request expired")
	ErrSkewed           = errors.New("request time skewed")
	ErrMalformed        = errors.New("malformed signature")
)

// Result is the authenticated request material needed to read the body.
type Result struct {
	AccessKey   string
	Region      string
	DateTime    string
	Scope       string
	PayloadHash string
	Signature   string
	SigningKey  []byte
	Presigned   bool
}

// Streaming reports whether the body uses aws-chunked framing.
func (r *Result) Streaming() bool {
	switch r.PayloadHash {
	case streamingPayload, streamingTrailer, unsignedTrailer:
		return true
	default:
		return false
	}
}

// SignedChunks reports whether each aws-chunked chunk carries a signature.
func (r *Result) SignedChunks() bool {
	return r.PayloadHash == streamingPayload || r.PayloadHash == streamingTrailer
}

// HasTrailers reports whether aws-chunked body ends with trailing headers.
func (r *Result) HasTrailers() bool {
	return r.PayloadHash == streamingTrailer || r.PayloadHash == unsignedTrailer
}

// Verify checks header auth or a presigned query. region is taken from the
// credential scope the client signed, so both us-east-1 and auto work.
func Verify(r *http.Request, accessKey, secret string, now time.Time, skew time.Duration) (*Result, error) {
	if accessKey == "" || secret == "" {
		return nil, ErrMissingAuth
	}
	if q := r.URL.Query().Get("X-Amz-Algorithm"); q != "" && r.Header.Get("Authorization") == "" {
		return verifyPresign(r, accessKey, secret, now, skew)
	}
	return verifyHeader(r, accessKey, secret, now, skew)
}

func verifyHeader(r *http.Request, accessKey, secret string, now time.Time, skew time.Duration) (*Result, error) {
	authz := r.Header.Get("Authorization")
	if authz == "" {
		return nil, ErrMissingAuth
	}
	cred, signedHeaders, signature, err := parseAuthorization(authz)
	if err != nil {
		return nil, err
	}
	access, date, region, scope, err := parseCredential(cred)
	if err != nil {
		return nil, err
	}
	if subtle.ConstantTimeCompare([]byte(access), []byte(accessKey)) != 1 {
		return nil, ErrInvalidAccessKey
	}
	amzDate := r.Header.Get("X-Amz-Date")
	if amzDate == "" {
		amzDate = r.Header.Get("Date")
	}
	signedAt, err := time.Parse("20060102T150405Z", amzDate)
	if err != nil {
		return nil, ErrMalformed
	}
	if !dateMatches(date, signedAt) {
		return nil, ErrMalformed
	}
	if delta := now.Sub(signedAt); delta > skew || delta < -skew {
		return nil, ErrSkewed
	}
	payload := r.Header.Get("X-Amz-Content-Sha256")
	if payload == "" {
		return nil, ErrMalformed
	}
	key := signingKey(secret, date, region)
	canon := canonicalRequest(r.Method, r.URL.Path, r.URL.Query(), r, signedHeaders, payload, false)
	if !signaturesEqual(signature, sign(key, amzDate, scope, canon)) {
		return nil, ErrSignature
	}
	return &Result{
		AccessKey:   access,
		Region:      region,
		DateTime:    amzDate,
		Scope:       scope,
		PayloadHash: payload,
		Signature:   strings.ToLower(signature),
		SigningKey:  key,
		Presigned:   false,
	}, nil
}

func verifyPresign(r *http.Request, accessKey, secret string, now time.Time, skew time.Duration) (*Result, error) {
	q := r.URL.Query()
	if q.Get("X-Amz-Algorithm") != Algorithm {
		return nil, ErrMalformed
	}
	access, date, region, scope, err := parseCredential(q.Get("X-Amz-Credential"))
	if err != nil {
		return nil, err
	}
	if subtle.ConstantTimeCompare([]byte(access), []byte(accessKey)) != 1 {
		return nil, ErrInvalidAccessKey
	}
	amzDate := q.Get("X-Amz-Date")
	signedAt, err := time.Parse("20060102T150405Z", amzDate)
	if err != nil {
		return nil, ErrMalformed
	}
	if !dateMatches(date, signedAt) {
		return nil, ErrMalformed
	}
	expiresSec, err := strconv.ParseInt(q.Get("X-Amz-Expires"), 10, 64)
	if err != nil || expiresSec <= 0 {
		return nil, ErrMalformed
	}
	expires := time.Duration(expiresSec) * time.Second
	if expires > maxPresignAge {
		return nil, ErrMalformed
	}
	if now.Before(signedAt.Add(-skew)) || now.After(signedAt.Add(expires)) {
		return nil, ErrExpired
	}
	signedHeaders := q.Get("X-Amz-SignedHeaders")
	if signedHeaders == "" || !strings.Contains(signedHeaders, "host") {
		return nil, ErrMalformed
	}
	payload := q.Get("X-Amz-Content-Sha256")
	if payload == "" {
		payload = unsignedPayload
	}
	signature := q.Get("X-Amz-Signature")
	if signature == "" {
		return nil, ErrMalformed
	}
	key := signingKey(secret, date, region)
	canon := canonicalRequest(r.Method, r.URL.Path, q, r, signedHeaders, payload, true)
	if !signaturesEqual(signature, sign(key, amzDate, scope, canon)) {
		return nil, ErrSignature
	}
	return &Result{
		AccessKey:   access,
		Region:      region,
		DateTime:    amzDate,
		Scope:       scope,
		PayloadHash: payload,
		Signature:   strings.ToLower(signature),
		SigningKey:  key,
		Presigned:   true,
	}, nil
}

// Sign adds header authentication to r. payloadHash is a hex digest,
// UNSIGNED-PAYLOAD, or one of the STREAMING-* sentinels.
func Sign(r *http.Request, accessKey, secret, region, payloadHash string, now time.Time) {
	amzDate := now.UTC().Format("20060102T150405Z")
	date := amzDate[:8]
	scope := date + "/" + region + "/" + service + "/aws4_request"
	r.Header.Set("X-Amz-Date", amzDate)
	r.Header.Set("X-Amz-Content-Sha256", payloadHash)
	signed := []string{"host", "x-amz-content-sha256", "x-amz-date"}
	for k := range r.Header {
		lk := strings.ToLower(k)
		if strings.HasPrefix(lk, "x-amz-") && lk != "x-amz-date" && lk != "x-amz-content-sha256" {
			signed = append(signed, lk)
		}
	}
	sort.Strings(signed)
	signedHeaders := strings.Join(signed, ";")
	canon := canonicalRequest(r.Method, r.URL.Path, r.URL.Query(), r, signedHeaders, payloadHash, false)
	signature := sign(signingKey(secret, date, region), amzDate, scope, canon)
	r.Header.Set("Authorization", fmt.Sprintf("%s Credential=%s/%s, SignedHeaders=%s, Signature=%s",
		Algorithm, accessKey, scope, signedHeaders, signature))
}

// Presign adds query-string authentication. The request Host must already be set.
func Presign(r *http.Request, accessKey, secret, region string, expires time.Duration, now time.Time) {
	amzDate := now.UTC().Format("20060102T150405Z")
	date := amzDate[:8]
	scope := date + "/" + region + "/" + service + "/aws4_request"
	q := r.URL.Query()
	q.Set("X-Amz-Algorithm", Algorithm)
	q.Set("X-Amz-Credential", accessKey+"/"+scope)
	q.Set("X-Amz-Date", amzDate)
	q.Set("X-Amz-Expires", strconv.FormatInt(int64(expires/time.Second), 10))
	q.Set("X-Amz-SignedHeaders", "host")
	canon := canonicalRequest(r.Method, r.URL.Path, q, r, "host", unsignedPayload, true)
	q.Set("X-Amz-Signature", sign(signingKey(secret, date, region), amzDate, scope, canon))
	r.URL.RawQuery = q.Encode()
}

// ChunkSignature is the signature of one aws-chunked body chunk.
// prev is the previous chunk signature, or the header signature for the first chunk.
func ChunkSignature(key []byte, dateTime, scope, prev string, chunk []byte) string {
	sum := sha256.Sum256(chunk)
	return ChunkSignatureHash(key, dateTime, scope, prev, sum[:])
}

// ChunkSignatureHash is ChunkSignature when the caller already hashed the chunk.
func ChunkSignatureHash(key []byte, dateTime, scope, prev string, sum []byte) string {
	sts := strings.Join([]string{
		"AWS4-HMAC-SHA256-PAYLOAD",
		dateTime,
		scope,
		prev,
		emptyPayloadHash,
		hex.EncodeToString(sum),
	}, "\n")
	return hex.EncodeToString(hmacSHA256(key, []byte(sts)))
}

// TrailerSignature signs trailing headers. canonical is "name:value\n" lines
// sorted by name, excluding x-amz-trailer-signature.
func TrailerSignature(key []byte, dateTime, scope, prev, canonical string) string {
	sum := sha256.Sum256([]byte(canonical))
	sts := strings.Join([]string{
		"AWS4-HMAC-SHA256-TRAILER",
		dateTime,
		scope,
		prev,
		hex.EncodeToString(sum[:]),
	}, "\n")
	return hex.EncodeToString(hmacSHA256(key, []byte(sts)))
}

func canonicalRequest(method, path string, query url.Values, r *http.Request, signedHeaders, payloadHash string, dropSignature bool) string {
	if path == "" {
		path = "/"
	}
	headers := strings.Split(signedHeaders, ";")
	var hb strings.Builder
	for _, h := range headers {
		h = strings.ToLower(strings.TrimSpace(h))
		if h == "" {
			continue
		}
		hb.WriteString(h)
		hb.WriteByte(':')
		hb.WriteString(headerValue(r, h))
		hb.WriteByte('\n')
	}
	return strings.Join([]string{
		method,
		encode(path, false),
		canonicalQuery(query, dropSignature),
		hb.String(),
		signedHeaders,
		payloadHash,
	}, "\n")
}

func headerValue(r *http.Request, name string) string {
	var raw string
	if name == "host" {
		raw = r.Host
	} else {
		raw = strings.Join(r.Header.Values(name), ",")
	}
	return strings.Join(strings.Fields(raw), " ")
}

func canonicalQuery(q url.Values, dropSignature bool) string {
	type pair struct{ k, v string }
	var pairs []pair
	for k, values := range q {
		if dropSignature && strings.EqualFold(k, "X-Amz-Signature") {
			continue
		}
		for _, v := range values {
			pairs = append(pairs, pair{k, v})
		}
	}
	sort.Slice(pairs, func(i, j int) bool {
		if pairs[i].k == pairs[j].k {
			return pairs[i].v < pairs[j].v
		}
		return pairs[i].k < pairs[j].k
	})
	var b strings.Builder
	for i, p := range pairs {
		if i > 0 {
			b.WriteByte('&')
		}
		b.WriteString(encode(p.k, true))
		b.WriteByte('=')
		b.WriteString(encode(p.v, true))
	}
	return b.String()
}

func encode(s string, encodeSlash bool) string {
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		c := s[i]
		if isUnreserved(c) || (c == '/' && !encodeSlash) {
			b.WriteByte(c)
			continue
		}
		fmt.Fprintf(&b, "%%%02X", c)
	}
	return b.String()
}

func isUnreserved(c byte) bool {
	return (c >= 'A' && c <= 'Z') || (c >= 'a' && c <= 'z') || (c >= '0' && c <= '9') || c == '-' || c == '_' || c == '.' || c == '~'
}

func parseAuthorization(h string) (credential, signedHeaders, signature string, err error) {
	if !strings.HasPrefix(h, Algorithm+" ") {
		return "", "", "", ErrMalformed
	}
	rest := strings.TrimSpace(h[len(Algorithm)+1:])
	parts := splitAuthParts(rest)
	for _, p := range parts {
		k, v, ok := strings.Cut(p, "=")
		if !ok {
			return "", "", "", ErrMalformed
		}
		switch strings.TrimSpace(k) {
		case "Credential":
			credential = strings.TrimSpace(v)
		case "SignedHeaders":
			signedHeaders = strings.TrimSpace(v)
		case "Signature":
			signature = strings.TrimSpace(v)
		}
	}
	if credential == "" || signedHeaders == "" || signature == "" {
		return "", "", "", ErrMalformed
	}
	return credential, signedHeaders, signature, nil
}

func splitAuthParts(s string) []string {
	var parts []string
	var b strings.Builder
	for _, r := range s {
		if r == ',' {
			parts = append(parts, strings.TrimSpace(b.String()))
			b.Reset()
			continue
		}
		b.WriteRune(r)
	}
	if b.Len() > 0 {
		parts = append(parts, strings.TrimSpace(b.String()))
	}
	return parts
}

func parseCredential(cred string) (access, date, region, scope string, err error) {
	parts := strings.Split(cred, "/")
	if len(parts) != 5 || parts[3] != service || parts[4] != "aws4_request" {
		return "", "", "", "", ErrMalformed
	}
	if len(parts[1]) != 8 || parts[0] == "" || parts[2] == "" {
		return "", "", "", "", ErrMalformed
	}
	scope = strings.Join(parts[1:], "/")
	return parts[0], parts[1], parts[2], scope, nil
}

func dateMatches(date string, signedAt time.Time) bool {
	return signedAt.UTC().Format("20060102") == date
}

func signingKey(secret, date, region string) []byte {
	kDate := hmacSHA256([]byte("AWS4"+secret), []byte(date))
	kRegion := hmacSHA256(kDate, []byte(region))
	kService := hmacSHA256(kRegion, []byte(service))
	return hmacSHA256(kService, []byte("aws4_request"))
}

func sign(key []byte, dateTime, scope, canonical string) string {
	sum := sha256.Sum256([]byte(canonical))
	sts := strings.Join([]string{Algorithm, dateTime, scope, hex.EncodeToString(sum[:])}, "\n")
	return hex.EncodeToString(hmacSHA256(key, []byte(sts)))
}

func hmacSHA256(key, data []byte) []byte {
	m := hmac.New(sha256.New, key)
	_, _ = m.Write(data)
	return m.Sum(nil)
}

func signaturesEqual(a, b string) bool {
	ab, err1 := hex.DecodeString(a)
	bb, err2 := hex.DecodeString(b)
	if err1 != nil || err2 != nil || len(ab) != len(bb) || len(ab) == 0 {
		return false
	}
	return subtle.ConstantTimeCompare(ab, bb) == 1
}
