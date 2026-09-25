package auth

import (
	"net/http"
	"testing"
	"time"
)

// Official AWS SigV4 GET Object example.
// https://docs.aws.amazon.com/AmazonS3/latest/API/sig-v4-header-based-auth.html
func TestAWSExampleSignature(t *testing.T) {
	req, err := http.NewRequest(http.MethodGet, "https://examplebucket.s3.amazonaws.com/test.txt", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Host = "examplebucket.s3.amazonaws.com"
	req.Header.Set("Range", "bytes=0-9")
	req.Header.Set("X-Amz-Content-Sha256", emptyPayloadHash)
	req.Header.Set("X-Amz-Date", "20130524T000000Z")
	const (
		access = "AKIAIOSFODNN7EXAMPLE"
		secret = "wJalrXUtnFEMI/K7MDENG/bPxRfiCYEXAMPLEKEY"
		region = "us-east-1"
		date   = "20130524"
		scope  = "20130524/us-east-1/s3/aws4_request"
	)
	signed := "host;range;x-amz-content-sha256;x-amz-date"
	canon := canonicalRequest(req.Method, req.URL.Path, req.URL.Query(), req, signed, emptyPayloadHash, false)
	got := sign(signingKey(secret, date, region), "20130524T000000Z", scope, canon)
	const want = "f0e8bdb87c964420e857bd35b5d6ed310bd44f0170aba48dd91039c6036bdb41"
	if got != want {
		t.Fatalf("signature %s, want %s\ncanonical:\n%s", got, want, canon)
	}
}

func TestSignAndVerify(t *testing.T) {
	req, err := http.NewRequest(http.MethodPut, "http://127.0.0.1:9000/photos/a.txt", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Host = "127.0.0.1:9000"
	req.Header.Set("Content-Type", "text/plain")
	now := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	Sign(req, "access", "secret", "us-east-1", emptyPayloadHash, now)
	res, err := Verify(req, "access", "secret", now, 15*time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if res.Region != "us-east-1" || res.PayloadHash != emptyPayloadHash {
		t.Fatalf("result %+v", res)
	}
	if _, err := Verify(req, "other", "secret", now, 15*time.Minute); err != ErrInvalidAccessKey {
		t.Fatalf("got %v", err)
	}
	req.Header.Set("X-Amz-Date", "20130524T000000Z")
	if _, err := Verify(req, "access", "secret", now, 15*time.Minute); err != ErrSignature && err != ErrSkewed && err != ErrMalformed {
		t.Fatalf("tampered date got %v", err)
	}
}

func TestPresign(t *testing.T) {
	req, err := http.NewRequest(http.MethodGet, "http://localhost:9000/photos/a.txt", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Host = "localhost:9000"
	now := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	Presign(req, "access", "secret", "auto", time.Hour, now)
	res, err := Verify(req, "access", "secret", now.Add(time.Minute), 15*time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if !res.Presigned || res.Region != "auto" {
		t.Fatalf("result %+v", res)
	}
	if _, err := Verify(req, "access", "secret", now.Add(2*time.Hour), 15*time.Minute); err != ErrExpired {
		t.Fatalf("got %v", err)
	}
}
