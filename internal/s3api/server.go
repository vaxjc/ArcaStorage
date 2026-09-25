// Package s3api serves the S3 HTTP API used by AWS SDK clients:
// buckets, objects, list, copy, multipart upload, presigned URLs and CORS.
package s3api

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/xml"
	"errors"
	"io"
	"log"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"

	"storage/internal/auth"
	"storage/internal/store"
)

const (
	ownerID   = "storage"
	ownerName = "storage"
	xmlNS     = "http://s3.amazonaws.com/doc/2006-03-01/"
)

// Server is a private S3-compatible endpoint backed by one filesystem store
// and one access key.
type Server struct {
	Store     *store.Store
	AccessKey string
	SecretKey string
	Region    string
	PublicURL string
	Bases     []string
	MaxObject int64
	Skew      time.Duration
	Now       func() time.Time
	Log       *log.Logger
}

// Handler returns the HTTP handler.
func (s *Server) Handler() http.Handler {
	if s.Now == nil {
		s.Now = time.Now
	}
	if s.Skew == 0 {
		s.Skew = 15 * time.Minute
	}
	if s.MaxObject == 0 {
		s.MaxObject = 8 << 30
	}
	if s.Log == nil {
		s.Log = log.Default()
	}
	return http.HandlerFunc(s.serve)
}

func (s *Server) serve(w http.ResponseWriter, r *http.Request) {
	start := s.Now()
	rid := newID()
	w.Header().Set("Server", "storage")
	w.Header().Set("x-amz-request-id", rid)
	sw := &statusWriter{ResponseWriter: w, status: 200}
	bucket, key := s.target(r)
	defer func() {
		s.Log.Printf("rid=%s %s %s %d %s", rid, r.Method, r.URL.Path, sw.status, s.Now().Sub(start).Round(time.Millisecond))
	}()

	if r.Method == http.MethodOptions {
		if !s.writeCORS(sw, r, bucket, true) {
			s.err(sw, r, http.StatusForbidden, "AccessDenied", "CORS origin is not allowed", r.URL.Path)
			return
		}
		sw.WriteHeader(http.StatusNoContent)
		return
	}
	s.writeCORS(sw, r, bucket, false)

	ar, err := auth.Verify(r, s.AccessKey, s.SecretKey, s.Now(), s.Skew)
	if err != nil {
		s.authErr(sw, r, err)
		return
	}
	s.route(sw, r, ar, bucket, key)
}

func (s *Server) route(w http.ResponseWriter, r *http.Request, ar *auth.Result, bucket, key string) {
	q := r.URL.Query()
	if code, ok := unsupported(q); ok {
		s.err(w, r, http.StatusNotImplemented, "NotImplemented", code+" is not supported", r.URL.Path)
		return
	}
	if bucket == "" {
		if r.Method == http.MethodGet && key == "" {
			s.listBuckets(w, r)
			return
		}
		s.err(w, r, http.StatusNotFound, "NoSuchBucket", "The specified bucket does not exist.", r.URL.Path)
		return
	}
	if key == "" {
		s.routeBucket(w, r, ar, bucket, q)
		return
	}
	s.routeObject(w, r, ar, bucket, key, q)
}

func (s *Server) routeBucket(w http.ResponseWriter, r *http.Request, ar *auth.Result, bucket string, q url.Values) {
	switch {
	case q.Has("location") && r.Method == http.MethodGet:
		s.getLocation(w, r, bucket)
	case q.Has("cors"):
		s.routeCORS(w, r, ar, bucket)
	case q.Has("delete") && r.Method == http.MethodPost:
		s.deleteObjects(w, r, ar, bucket)
	case r.Method == http.MethodPut:
		s.createBucket(w, r, ar, bucket)
	case r.Method == http.MethodDelete:
		s.deleteBucket(w, r, bucket)
	case r.Method == http.MethodHead:
		s.headBucket(w, r, bucket)
	case r.Method == http.MethodGet:
		s.listObjects(w, r, bucket, q)
	default:
		w.Header().Set("Allow", "GET, PUT, DELETE, HEAD, POST")
		s.err(w, r, http.StatusMethodNotAllowed, "MethodNotAllowed", "The specified method is not allowed.", r.URL.Path)
	}
}

func (s *Server) routeObject(w http.ResponseWriter, r *http.Request, ar *auth.Result, bucket, key string, q url.Values) {
	switch {
	case q.Has("uploads") && r.Method == http.MethodPost:
		s.createMultipart(w, r, bucket, key)
	case q.Get("uploadId") != "" && q.Get("partNumber") != "" && r.Method == http.MethodPut:
		s.uploadPart(w, r, ar, bucket, q.Get("uploadId"), q.Get("partNumber"))
	case q.Get("uploadId") != "" && r.Method == http.MethodPost:
		s.completeMultipart(w, r, ar, bucket, key, q.Get("uploadId"))
	case q.Get("uploadId") != "" && r.Method == http.MethodDelete:
		s.abortMultipart(w, r, bucket, q.Get("uploadId"))
	case q.Get("uploadId") != "" && r.Method == http.MethodGet:
		s.listParts(w, r, bucket, q.Get("uploadId"))
	case q.Has("tagging"):
		s.routeTagging(w, r, ar, bucket, key)
	case q.Has("acl") && (r.Method == http.MethodGet || r.Method == http.MethodHead):
		s.getACL(w, r, bucket, key)
	case q.Has("acl") && r.Method == http.MethodPut:
		w.WriteHeader(http.StatusOK)
	case r.Method == http.MethodPut && r.Header.Get("X-Amz-Copy-Source") != "":
		s.copyObject(w, r, bucket, key)
	case r.Method == http.MethodPut:
		s.putObject(w, r, ar, bucket, key)
	case r.Method == http.MethodGet:
		s.getObject(w, r, bucket, key)
	case r.Method == http.MethodHead:
		s.headObject(w, r, bucket, key)
	case r.Method == http.MethodDelete:
		s.deleteObject(w, r, bucket, key)
	default:
		s.err(w, r, http.StatusMethodNotAllowed, "MethodNotAllowed", "The specified method is not allowed.", r.URL.Path)
	}
}

func (s *Server) target(r *http.Request) (bucket, key string) {
	host := hostname(r.Host)
	for _, base := range s.Bases {
		if host == base {
			return splitPath(r.URL.Path)
		}
		suffix := "." + base
		if strings.HasSuffix(host, suffix) && len(host) > len(suffix) {
			return host[:len(host)-len(suffix)], strings.TrimPrefix(r.URL.Path, "/")
		}
	}
	return splitPath(r.URL.Path)
}

func splitPath(p string) (bucket, key string) {
	p = strings.TrimPrefix(p, "/")
	if p == "" {
		return "", ""
	}
	bucket, key, _ = strings.Cut(p, "/")
	return bucket, key
}

func hostname(hostport string) string {
	host, _, err := net.SplitHostPort(hostport)
	if err != nil {
		return hostport
	}
	return host
}

func unsupported(q url.Values) (string, bool) {
	for _, name := range []string{
		"policy", "lifecycle", "versioning", "encryption", "replication",
		"notification", "accelerate", "logging", "website", "requestPayment",
		"object-lock", "legal-hold", "retention", "publicAccessBlock", "versions",
		"analytics", "inventory", "metrics", "ownershipControls", "intelligent-tiering",
	} {
		if q.Has(name) {
			return name, true
		}
	}
	return "", false
}

func (s *Server) authErr(w http.ResponseWriter, r *http.Request, err error) {
	switch {
	case errors.Is(err, auth.ErrInvalidAccessKey):
		s.err(w, r, http.StatusForbidden, "InvalidAccessKeyId", "The access key does not exist.", r.URL.Path)
	case errors.Is(err, auth.ErrSignature):
		s.err(w, r, http.StatusForbidden, "SignatureDoesNotMatch", "The request signature does not match.", r.URL.Path)
	case errors.Is(err, auth.ErrExpired):
		s.err(w, r, http.StatusForbidden, "AccessDenied", "Request has expired.", r.URL.Path)
	case errors.Is(err, auth.ErrSkewed):
		s.err(w, r, http.StatusForbidden, "RequestTimeTooSkewed", "The difference between the request time and the server time is too large.", r.URL.Path)
	default:
		s.err(w, r, http.StatusForbidden, "AccessDenied", "Access Denied.", r.URL.Path)
	}
}

func (s *Server) storeErr(w http.ResponseWriter, r *http.Request, err error, key bool) {
	switch {
	case errors.Is(err, store.ErrNoSuchBucket):
		s.err(w, r, http.StatusNotFound, "NoSuchBucket", "The specified bucket does not exist.", r.URL.Path)
	case errors.Is(err, store.ErrNotFound) && key:
		s.err(w, r, http.StatusNotFound, "NoSuchKey", "The specified key does not exist.", r.URL.Path)
	case errors.Is(err, store.ErrNotFound):
		s.err(w, r, http.StatusNotFound, "NoSuchBucket", "The specified bucket does not exist.", r.URL.Path)
	case errors.Is(err, store.ErrExists):
		s.err(w, r, http.StatusConflict, "BucketAlreadyOwnedByYou", "The bucket already exists.", r.URL.Path)
	case errors.Is(err, store.ErrNotEmpty):
		s.err(w, r, http.StatusConflict, "BucketNotEmpty", "The bucket you tried to delete is not empty.", r.URL.Path)
	case errors.Is(err, store.ErrInvalid):
		s.err(w, r, http.StatusBadRequest, "InvalidArgument", "The request was invalid.", r.URL.Path)
	case errors.Is(err, store.ErrPartMissing):
		s.err(w, r, http.StatusBadRequest, "InvalidPart", "One or more of the specified parts could not be found.", r.URL.Path)
	case errors.Is(err, errBadDigest):
		s.err(w, r, http.StatusBadRequest, "BadDigest", "The Content-MD5 or checksum you specified did not match.", r.URL.Path)
	case errors.Is(err, errIncomplete):
		s.err(w, r, http.StatusBadRequest, "IncompleteBody", "You did not provide the number of bytes specified by the Content-Length HTTP header.", r.URL.Path)
	case errors.Is(err, errTooLarge):
		s.err(w, r, http.StatusBadRequest, "EntityTooLarge", "The object is larger than the configured maximum.", r.URL.Path)
	case errors.Is(err, errChunkFormat):
		s.err(w, r, http.StatusBadRequest, "InvalidArgument", "The chunked body is malformed.", r.URL.Path)
	case errors.Is(err, auth.ErrSignature):
		s.err(w, r, http.StatusForbidden, "SignatureDoesNotMatch", "The request signature does not match.", r.URL.Path)
	case errors.Is(err, io.ErrUnexpectedEOF):
		s.err(w, r, http.StatusBadRequest, "IncompleteBody", "The request body ended early.", r.URL.Path)
	default:
		s.err(w, r, http.StatusInternalServerError, "InternalError", "We encountered an internal error. Please try again.", r.URL.Path)
	}
}

type apiError struct {
	XMLName   xml.Name `xml:"Error"`
	Code      string   `xml:"Code"`
	Message   string   `xml:"Message"`
	Resource  string   `xml:"Resource"`
	RequestID string   `xml:"RequestId"`
}

func (s *Server) err(w http.ResponseWriter, r *http.Request, status int, code, message, resource string) {
	if r.Method == http.MethodHead {
		w.WriteHeader(status)
		return
	}
	writeXML(w, status, apiError{Code: code, Message: message, Resource: resource, RequestID: w.Header().Get("x-amz-request-id")})
}

func writeXML(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/xml")
	w.WriteHeader(status)
	_, _ = w.Write([]byte(xml.Header))
	enc := xml.NewEncoder(w)
	enc.Indent("", "  ")
	_ = enc.Encode(v)
}

func newID() string {
	var b [8]byte
	_, _ = rand.Read(b[:])
	return hex.EncodeToString(b[:])
}

type statusWriter struct {
	http.ResponseWriter
	status int
	wrote  bool
}

func (s *statusWriter) WriteHeader(code int) {
	if s.wrote {
		return
	}
	s.wrote = true
	s.status = code
	s.ResponseWriter.WriteHeader(code)
}

func (s *statusWriter) Write(p []byte) (int, error) {
	if !s.wrote {
		s.WriteHeader(http.StatusOK)
	}
	return s.ResponseWriter.Write(p)
}
