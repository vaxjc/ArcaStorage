package s3api

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/xml"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"storage/internal/auth"
	"storage/internal/store"
)

func TestObjectRoundTrip(t *testing.T) {
	ts, access, secret := newTestServer(t)
	ensureBucket(t, ts, access, secret, "photos")
	body := []byte("hola storage")
	put := signed(t, http.MethodPut, ts.URL+"/photos/hola.txt", "photos.example", body, access, secret)
	put.Header.Set("Content-Type", "text/plain")
	put.Header.Set("X-Amz-Meta-Author", "ada")
	res := do(t, ts, put, body, access, secret)
	if res.StatusCode != http.StatusOK {
		t.Fatalf("put %d %s", res.StatusCode, res.Body)
	}
	etag := res.Header.Get("ETag")
	if etag == "" || !strings.Contains(etag, "\"") {
		t.Fatalf("etag %q", etag)
	}

	get := signed(t, http.MethodGet, ts.URL+"/photos/hola.txt", "", nil, access, secret)
	res = do(t, ts, get, nil, access, secret)
	if res.StatusCode != http.StatusOK || res.Body != "hola storage" {
		t.Fatalf("get %d %q", res.StatusCode, res.Body)
	}
	if res.Header.Get("X-Amz-Meta-Author") != "ada" {
		t.Fatalf("meta %q", res.Header.Get("X-Amz-Meta-Author"))
	}

	list := signed(t, http.MethodGet, ts.URL+"/photos?list-type=2&prefix=hola", "", nil, access, secret)
	res = do(t, ts, list, nil, access, secret)
	if res.StatusCode != http.StatusOK || !strings.Contains(res.Body, "hola.txt") {
		t.Fatalf("list %d %s", res.StatusCode, res.Body)
	}

	del := signed(t, http.MethodDelete, ts.URL+"/photos/hola.txt", "", nil, access, secret)
	res = do(t, ts, del, nil, access, secret)
	if res.StatusCode != http.StatusNoContent {
		t.Fatalf("delete %d", res.StatusCode)
	}
}

func TestPresignAndVirtualHost(t *testing.T) {
	ts, access, secret := newTestServer(t)
	ensureBucket(t, ts, access, secret, "photos")
	body := []byte("virtual")
	put := signed(t, http.MethodPut, ts.URL+"/photos/v.txt", "", body, access, secret)
	do(t, ts, put, body, access, secret)

	req, err := http.NewRequest(http.MethodGet, ts.URL+"/photos/v.txt", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Host = req.URL.Host
	auth.Presign(req, access, secret, "us-east-1", time.Hour, time.Now())
	res, err := ts.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	got, _ := io.ReadAll(res.Body)
	if res.StatusCode != http.StatusOK || string(got) != "virtual" {
		t.Fatalf("presign %d %s", res.StatusCode, got)
	}

	vreq, err := http.NewRequest(http.MethodGet, "http://photos.localhost/v.txt", nil)
	if err != nil {
		t.Fatal(err)
	}
	vreq.URL.Host = req.URL.Host
	vreq.Host = "photos.localhost"
	auth.Sign(vreq, access, secret, "auto", emptySHA, time.Now())
	res, err = ts.Client().Do(vreq)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	got, _ = io.ReadAll(res.Body)
	if res.StatusCode != http.StatusOK || string(got) != "virtual" {
		t.Fatalf("virtual %d %s host %s", res.StatusCode, got, vreq.Host)
	}
}

func TestRejectsBadSignatureAndTraversal(t *testing.T) {
	ts, access, secret := newTestServer(t)
	ensureBucket(t, ts, access, secret, "photos")
	req, err := http.NewRequest(http.MethodGet, ts.URL+"/photos", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Host = req.URL.Host
	auth.Sign(req, access, secret, "us-east-1", emptySHA, time.Now())
	req.Header.Set("X-Amz-Date", "20130524T000000Z")
	res, err := ts.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusForbidden {
		t.Fatalf("status %d", res.StatusCode)
	}
	bad := signed(t, http.MethodPut, ts.URL+"/photos/%2e%2e/secret", "", []byte("x"), access, secret)
	got := do(t, ts, bad, []byte("x"), access, secret)
	if got.StatusCode != http.StatusBadRequest {
		t.Fatalf("traversal %d %s", got.StatusCode, got.Body)
	}
}

func TestMultipartDelimiterAndDelete(t *testing.T) {
	ts, access, secret := newTestServer(t)
	ensureBucket(t, ts, access, secret, "photos")
	create := signed(t, http.MethodPost, ts.URL+"/photos/dir/a.bin?uploads", "", nil, access, secret)
	res := do(t, ts, create, nil, access, secret)
	if res.StatusCode != http.StatusOK {
		t.Fatalf("init %d %s", res.StatusCode, res.Body)
	}
	var init struct {
		UploadID string `xml:"UploadId"`
	}
	if err := xml.Unmarshal([]byte(res.Body), &init); err != nil || init.UploadID == "" {
		t.Fatalf("upload id %v %s", err, res.Body)
	}
	partBody := []byte("abc")
	part := signed(t, http.MethodPut, ts.URL+"/photos/dir/a.bin?partNumber=1&uploadId="+init.UploadID, "", partBody, access, secret)
	res = do(t, ts, part, partBody, access, secret)
	if res.StatusCode != http.StatusOK {
		t.Fatalf("part %d %s", res.StatusCode, res.Body)
	}
	completeXML := `<CompleteMultipartUpload><Part><PartNumber>1</PartNumber><ETag>` + res.Header.Get("ETag") + `</ETag></Part></CompleteMultipartUpload>`
	done := signed(t, http.MethodPost, ts.URL+"/photos/dir/a.bin?uploadId="+init.UploadID, "", []byte(completeXML), access, secret)
	res = do(t, ts, done, []byte(completeXML), access, secret)
	if res.StatusCode != http.StatusOK || !strings.Contains(res.Body, "-1") {
		t.Fatalf("complete %d %s", res.StatusCode, res.Body)
	}

	other := []byte("root")
	put := signed(t, http.MethodPut, ts.URL+"/photos/root.txt", "", other, access, secret)
	do(t, ts, put, other, access, secret)
	list := signed(t, http.MethodGet, ts.URL+"/photos?delimiter=/", "", nil, access, secret)
	res = do(t, ts, list, nil, access, secret)
	if !strings.Contains(res.Body, "dir/") || !strings.Contains(res.Body, "root.txt") {
		t.Fatalf("list %s", res.Body)
	}

	delXML := `<Delete><Object><Key>root.txt</Key></Object><Object><Key>dir/a.bin</Key></Object></Delete>`
	del := signed(t, http.MethodPost, ts.URL+"/photos?delete", "", []byte(delXML), access, secret)
	res = do(t, ts, del, []byte(delXML), access, secret)
	if res.StatusCode != http.StatusOK || strings.Count(res.Body, "<Key>") != 2 {
		t.Fatalf("delete objects %d %s", res.StatusCode, res.Body)
	}
}

func TestUnsignedChunkedUpload(t *testing.T) {
	ts, access, secret := newTestServer(t)
	ensureBucket(t, ts, access, secret, "photos")
	payload := []byte("chunked-body")
	wire := awsChunk(payload)
	req, err := http.NewRequest(http.MethodPut, ts.URL+"/photos/c.txt", bytes.NewReader(wire))
	if err != nil {
		t.Fatal(err)
	}
	req.Host = req.URL.Host
	req.Header.Set("Content-Encoding", "aws-chunked")
	req.Header.Set("X-Amz-Decoded-Content-Length", "12")
	req.Header.Set("X-Amz-Trailer", "x-amz-checksum-crc32")
	auth.Sign(req, access, secret, "us-east-1", "STREAMING-UNSIGNED-PAYLOAD-TRAILER", time.Now())
	res, err := ts.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	msg, _ := io.ReadAll(res.Body)
	if res.StatusCode != http.StatusOK {
		t.Fatalf("chunked put %d %s", res.StatusCode, msg)
	}
	get := signed(t, http.MethodGet, ts.URL+"/photos/c.txt", "", nil, access, secret)
	got := do(t, ts, get, nil, access, secret)
	if got.Body != string(payload) {
		t.Fatalf("got %q", got.Body)
	}
}

func awsChunk(payload []byte) []byte {
	sum := crc32Sum(payload)
	var b bytes.Buffer
	b.WriteString("c\r\n")
	b.Write(payload)
	b.WriteString("\r\n0\r\n")
	b.WriteString("x-amz-checksum-crc32:" + sum + "\r\n")
	b.WriteString("\r\n")
	return b.Bytes()
}

func ensureBucket(t *testing.T, ts *httptest.Server, access, secret, name string) {
	t.Helper()
	req := signed(t, http.MethodPut, ts.URL+"/"+name, "", nil, access, secret)
	res := do(t, ts, req, nil, access, secret)
	if res.StatusCode != http.StatusOK && res.StatusCode != http.StatusConflict {
		t.Fatalf("create bucket %d %s", res.StatusCode, res.Body)
	}
}

func newTestServer(t *testing.T) (*httptest.Server, string, string) {
	t.Helper()
	st, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	const access, secret = "TESTACCESSKEY", "test-secret-key"
	srv := &Server{
		Store: st, AccessKey: access, SecretKey: secret,
		Region: "us-east-1", PublicURL: "http://localhost:9000",
		Bases: []string{"127.0.0.1", "localhost"}, MaxObject: 1 << 20,
	}
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)
	return ts, access, secret
}

type result struct {
	StatusCode int
	Header     http.Header
	Body       string
}

func signed(t *testing.T, method, rawURL, _ string, body []byte, access, secret string) *http.Request {
	t.Helper()
	var rdr io.Reader
	if body != nil {
		rdr = bytes.NewReader(body)
	}
	req, err := http.NewRequest(method, rawURL, rdr)
	if err != nil {
		t.Fatal(err)
	}
	req.Host = req.URL.Host
	req.ContentLength = int64(len(body))
	return req
}

func do(t *testing.T, ts *httptest.Server, req *http.Request, body []byte, access, secret string) result {
	t.Helper()
	sum := sha256.Sum256(body)
	auth.Sign(req, access, secret, "us-east-1", hex.EncodeToString(sum[:]), time.Now())
	if body != nil {
		req.Body = io.NopCloser(bytes.NewReader(body))
		req.ContentLength = int64(len(body))
	}
	res, err := ts.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	b, _ := io.ReadAll(res.Body)
	return result{StatusCode: res.StatusCode, Header: res.Header, Body: string(b)}
}

const emptySHA = "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855"
