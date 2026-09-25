package s3api

import (
	"encoding/base64"
	"encoding/xml"
	"errors"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"

	"arca/internal/auth"
	"arca/internal/store"
)

var errPrecondition = errors.New("precondition failed")

func (s *Server) listBuckets(w http.ResponseWriter, r *http.Request) {
	buckets, err := s.Store.ListBuckets()
	if err != nil {
		s.storeErr(w, r, err, false)
		return
	}
	out := listBucketsResult{Xmlns: xmlNS, Owner: ownerXML()}
	for _, b := range buckets {
		out.Buckets.Bucket = append(out.Buckets.Bucket, bucketXML{Name: b.Name, CreationDate: xmlTime(b.Created)})
	}
	writeXML(w, http.StatusOK, out)
}

func (s *Server) createBucket(w http.ResponseWriter, r *http.Request, ar *auth.Result, bucket string) {
	body, err := s.readLimited(r, ar, 64<<10)
	if err != nil {
		s.storeErr(w, r, err, false)
		return
	}
	if len(strings.TrimSpace(string(body))) > 0 {
		var cfg struct {
			Location string `xml:"LocationConstraint"`
		}
		if err := xml.Unmarshal(body, &cfg); err != nil {
			s.err(w, r, http.StatusBadRequest, "MalformedXML", "The XML you provided was not well-formed.", r.URL.Path)
			return
		}
		if cfg.Location != "" && cfg.Location != s.Region {
			s.err(w, r, http.StatusBadRequest, "InvalidLocationConstraint", "The specified location constraint is not valid.", r.URL.Path)
			return
		}
	}
	if err := s.Store.CreateBucket(bucket); err != nil {
		s.storeErr(w, r, err, false)
		return
	}
	w.Header().Set("Location", s.PublicURL+"/"+bucket)
	w.WriteHeader(http.StatusOK)
}

func (s *Server) deleteBucket(w http.ResponseWriter, r *http.Request, bucket string) {
	if err := s.Store.DeleteBucket(bucket); err != nil {
		s.storeErr(w, r, err, false)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) headBucket(w http.ResponseWriter, r *http.Request, bucket string) {
	if _, err := s.Store.Bucket(bucket); err != nil {
		s.storeErr(w, r, err, false)
		return
	}
	w.WriteHeader(http.StatusOK)
}

func (s *Server) getLocation(w http.ResponseWriter, r *http.Request, bucket string) {
	if _, err := s.Store.Bucket(bucket); err != nil {
		s.storeErr(w, r, err, false)
		return
	}
	region := s.Region
	if region == "us-east-1" {
		region = ""
	}
	writeXML(w, http.StatusOK, locationXML{Xmlns: xmlNS, Region: region})
}

func (s *Server) putObject(w http.ResponseWriter, r *http.Request, ar *auth.Result, bucket, key string) {
	if err := s.preconditions(bucket, key, r.Header.Get("If-Match"), r.Header.Get("If-None-Match")); err != nil {
		if errors.Is(err, errPrecondition) {
			s.err(w, r, http.StatusPreconditionFailed, "PreconditionFailed", "At least one of the preconditions you specified did not hold.", r.URL.Path)
			return
		}
		s.storeErr(w, r, err, true)
		return
	}
	body, err := openBody(r, ar, s.MaxObject)
	if err != nil {
		s.storeErr(w, r, err, true)
		return
	}
	defer body.Close()
	meta, err := s.Store.PutObject(bucket, key, body, metaFromHeaders(r.Header))
	if err != nil {
		s.storeErr(w, r, err, true)
		return
	}
	writeObjectHeaders(w, meta)
	w.WriteHeader(http.StatusOK)
}

func (s *Server) getObject(w http.ResponseWriter, r *http.Request, bucket, key string) {
	f, meta, err := s.Store.Get(bucket, key)
	if err != nil {
		s.storeErr(w, r, err, true)
		return
	}
	defer f.Close()
	writeObjectHeaders(w, meta)
	http.ServeContent(w, r, key, meta.ModTime, f)
}

func (s *Server) headObject(w http.ResponseWriter, r *http.Request, bucket, key string) {
	meta, err := s.Store.Head(bucket, key)
	if err != nil {
		s.storeErr(w, r, err, true)
		return
	}
	writeObjectHeaders(w, meta)
	w.Header().Set("Content-Length", strconv.FormatInt(meta.Size, 10))
	w.WriteHeader(http.StatusOK)
}

func (s *Server) deleteObject(w http.ResponseWriter, r *http.Request, bucket, key string) {
	if r.Header.Get("If-Match") != "" {
		if err := s.preconditions(bucket, key, r.Header.Get("If-Match"), ""); err != nil {
			if errors.Is(err, errPrecondition) {
				s.err(w, r, http.StatusPreconditionFailed, "PreconditionFailed", "At least one of the preconditions you specified did not hold.", r.URL.Path)
				return
			}
			s.storeErr(w, r, err, true)
			return
		}
	}
	if err := s.Store.DeleteObject(bucket, key); err != nil {
		s.storeErr(w, r, err, true)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) deleteObjects(w http.ResponseWriter, r *http.Request, ar *auth.Result, bucket string) {
	if _, err := s.Store.Bucket(bucket); err != nil {
		s.storeErr(w, r, err, false)
		return
	}
	body, err := s.readLimited(r, ar, 1<<20)
	if err != nil {
		s.storeErr(w, r, err, false)
		return
	}
	var req deleteRequest
	if err := xml.Unmarshal(body, &req); err != nil {
		s.err(w, r, http.StatusBadRequest, "MalformedXML", "The XML you provided was not well-formed.", r.URL.Path)
		return
	}
	var resp deleteResult
	resp.Xmlns = xmlNS
	for _, obj := range req.Objects {
		if err := s.Store.DeleteObject(bucket, obj.Key); err != nil {
			resp.Errors = append(resp.Errors, deleteError{Key: obj.Key, Code: "InternalError", Message: "We encountered an internal error. Please try again."})
			continue
		}
		if !req.Quiet {
			resp.Deleted = append(resp.Deleted, deletedKey{Key: obj.Key})
		}
	}
	writeXML(w, http.StatusOK, resp)
}

func (s *Server) copyObject(w http.ResponseWriter, r *http.Request, bucket, key string) {
	srcBucket, srcKey, err := parseCopySource(r.Header.Get("X-Amz-Copy-Source"))
	if err != nil {
		s.err(w, r, http.StatusBadRequest, "InvalidArgument", "The copy source is invalid.", r.URL.Path)
		return
	}
	src, err := s.Store.Head(srcBucket, srcKey)
	if err != nil {
		s.storeErr(w, r, err, true)
		return
	}
	if r.Header.Get("X-Amz-Copy-Source-If-Match") != "" && !etagMatch(r.Header.Get("X-Amz-Copy-Source-If-Match"), src.ETag) {
		s.err(w, r, http.StatusPreconditionFailed, "PreconditionFailed", "At least one of the preconditions you specified did not hold.", r.URL.Path)
		return
	}
	if r.Header.Get("X-Amz-Copy-Source-If-None-Match") != "" && etagMatch(r.Header.Get("X-Amz-Copy-Source-If-None-Match"), src.ETag) {
		s.err(w, r, http.StatusPreconditionFailed, "PreconditionFailed", "At least one of the preconditions you specified did not hold.", r.URL.Path)
		return
	}
	replace := strings.EqualFold(r.Header.Get("X-Amz-Metadata-Directive"), "REPLACE")
	if !replace && srcBucket == bucket && srcKey == key {
		s.err(w, r, http.StatusBadRequest, "InvalidRequest", "This copy request is illegal because it is trying to copy an object to itself without changing the object's metadata.", r.URL.Path)
		return
	}
	meta := src
	if replace {
		meta = metaFromHeaders(r.Header)
	}
	out, err := s.Store.CopyObject(bucket, key, srcBucket, srcKey, replace, meta)
	if err != nil {
		s.storeErr(w, r, err, true)
		return
	}
	writeXML(w, http.StatusOK, copyResult{Xmlns: xmlNS, LastModified: xmlTime(out.ModTime), ETag: quote(out.ETag)})
}

func (s *Server) listObjects(w http.ResponseWriter, r *http.Request, bucket string, q url.Values) {
	objs, err := s.Store.List(bucket, q.Get("prefix"))
	if err != nil {
		s.storeErr(w, r, err, false)
		return
	}
	max := 1000
	if v := q.Get("max-keys"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 0 {
			s.err(w, r, http.StatusBadRequest, "InvalidArgument", "max-keys is invalid.", r.URL.Path)
			return
		}
		if n < max {
			max = n
		}
	}
	after := q.Get("start-after")
	if q.Get("continuation-token") != "" {
		if q.Get("start-after") != "" {
			s.err(w, r, http.StatusBadRequest, "InvalidArgument", "continuation-token and start-after cannot both be set.", r.URL.Path)
			return
		}
		b, err := base64.RawURLEncoding.DecodeString(q.Get("continuation-token"))
		if err != nil {
			s.err(w, r, http.StatusBadRequest, "InvalidArgument", "continuation-token is invalid.", r.URL.Path)
			return
		}
		after = string(b)
	}
	if q.Get("marker") != "" {
		after = q.Get("marker")
	}
	contents, commons, trunc, last := paginate(objs, q.Get("prefix"), q.Get("delimiter"), after, max)
	enc := q.Get("encoding-type") == "url"
	v2 := q.Get("list-type") == "2"
	if v2 {
		out := listV2Result{
			Xmlns: xmlNS, Name: bucket, Prefix: encodeKey(q.Get("prefix"), enc),
			Delimiter: encodeKey(q.Get("delimiter"), enc), MaxKeys: max, KeyCount: len(contents),
			IsTruncated: trunc, StartAfter: encodeKey(q.Get("start-after"), enc),
		}
		if q.Get("continuation-token") != "" {
			out.ContinuationToken = q.Get("continuation-token")
		}
		for _, o := range contents {
			out.Contents = append(out.Contents, contentXML(o, enc))
		}
		for _, c := range commons {
			out.CommonPrefixes = append(out.CommonPrefixes, commonPrefix{Prefix: encodeKey(c, enc)})
		}
		if trunc && last != "" {
			out.NextContinuationToken = base64.RawURLEncoding.EncodeToString([]byte(last))
		}
		writeXML(w, http.StatusOK, out)
		return
	}
	out := listV1Result{
		Xmlns: xmlNS, Name: bucket, Prefix: encodeKey(q.Get("prefix"), enc),
		Marker: encodeKey(q.Get("marker"), enc), Delimiter: encodeKey(q.Get("delimiter"), enc),
		MaxKeys: max, IsTruncated: trunc,
	}
	for _, o := range contents {
		out.Contents = append(out.Contents, contentXML(o, enc))
	}
	for _, c := range commons {
		out.CommonPrefixes = append(out.CommonPrefixes, commonPrefix{Prefix: encodeKey(c, enc)})
	}
	if trunc && last != "" {
		out.NextMarker = encodeKey(last, enc)
	}
	writeXML(w, http.StatusOK, out)
}

func paginate(objs []store.Object, prefix, delim, after string, max int) (contents []store.Object, commons []string, trunc bool, last string) {
	type item struct {
		key string
		obj *store.Object
	}
	seen := map[string]struct{}{}
	var items []item
	for i := range objs {
		rel := strings.TrimPrefix(objs[i].Key, prefix)
		if delim != "" {
			if idx := strings.Index(rel, delim); idx >= 0 {
				cp := prefix + rel[:idx+len(delim)]
				if _, ok := seen[cp]; ok {
					continue
				}
				seen[cp] = struct{}{}
				items = append(items, item{key: cp})
				continue
			}
		}
		items = append(items, item{key: objs[i].Key, obj: &objs[i]})
	}
	sort.Slice(items, func(i, j int) bool { return items[i].key < items[j].key })
	for _, it := range items {
		if after != "" && it.key <= after {
			continue
		}
		if len(contents)+len(commons) >= max {
			return contents, commons, true, last
		}
		last = it.key
		if it.obj == nil {
			commons = append(commons, it.key)
			continue
		}
		contents = append(contents, *it.obj)
	}
	return contents, commons, false, last
}

func contentXML(o store.Object, enc bool) contentXMLType {
	return contentXMLType{
		Key:          encodeKey(o.Key, enc),
		LastModified: xmlTime(o.Meta.ModTime),
		ETag:         quote(o.Meta.ETag),
		Size:         o.Meta.Size,
		StorageClass: "STANDARD",
		Owner:        ownerXML(),
	}
}

func (s *Server) createMultipart(w http.ResponseWriter, r *http.Request, bucket, key string) {
	id, err := s.Store.CreateMultipart(bucket, key, metaFromHeaders(r.Header))
	if err != nil {
		s.storeErr(w, r, err, true)
		return
	}
	writeXML(w, http.StatusOK, initiateResult{Xmlns: xmlNS, Bucket: bucket, Key: key, UploadID: id})
}

func (s *Server) uploadPart(w http.ResponseWriter, r *http.Request, ar *auth.Result, bucket, uploadID, partNumber string) {
	n, err := strconv.Atoi(partNumber)
	if err != nil || n < 1 || n > 10000 {
		s.err(w, r, http.StatusBadRequest, "InvalidArgument", "partNumber is invalid.", r.URL.Path)
		return
	}
	body, err := openBody(r, ar, s.MaxObject)
	if err != nil {
		s.storeErr(w, r, err, true)
		return
	}
	defer body.Close()
	part, err := s.Store.PutPart(bucket, uploadID, n, body)
	if err != nil {
		s.storeErr(w, r, err, true)
		return
	}
	w.Header().Set("ETag", quote(part.ETag))
	w.WriteHeader(http.StatusOK)
}

func (s *Server) completeMultipart(w http.ResponseWriter, r *http.Request, ar *auth.Result, bucket, key, uploadID string) {
	body, err := s.readLimited(r, ar, 2<<20)
	if err != nil {
		s.storeErr(w, r, err, true)
		return
	}
	var req completeRequest
	if err := xml.Unmarshal(body, &req); err != nil || len(req.Parts) == 0 {
		s.err(w, r, http.StatusBadRequest, "MalformedXML", "The XML you provided was not well-formed.", r.URL.Path)
		return
	}
	numbers := make([]int, len(req.Parts))
	etags := make([]string, len(req.Parts))
	for i, p := range req.Parts {
		numbers[i] = p.PartNumber
		etags[i] = p.ETag
	}
	upKey, _, err := s.Store.ListParts(bucket, uploadID)
	if err != nil {
		s.storeErr(w, r, err, true)
		return
	}
	if upKey != key {
		s.err(w, r, http.StatusBadRequest, "InvalidArgument", "The upload does not match this key.", r.URL.Path)
		return
	}
	gotKey, meta, err := s.Store.CompleteMultipart(bucket, uploadID, numbers, etags)
	if err != nil {
		s.storeErr(w, r, err, true)
		return
	}
	if gotKey != key {
		s.err(w, r, http.StatusBadRequest, "InvalidArgument", "The upload does not match this key.", r.URL.Path)
		return
	}
	writeXML(w, http.StatusOK, completeResult{
		Xmlns: xmlNS, Location: s.PublicURL + "/" + bucket + "/" + key,
		Bucket: bucket, Key: key, ETag: quote(meta.ETag),
	})
}

func (s *Server) abortMultipart(w http.ResponseWriter, r *http.Request, bucket, uploadID string) {
	if err := s.Store.AbortMultipart(bucket, uploadID); err != nil {
		s.storeErr(w, r, err, true)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) listParts(w http.ResponseWriter, r *http.Request, bucket, uploadID string) {
	key, parts, err := s.Store.ListParts(bucket, uploadID)
	if err != nil {
		s.storeErr(w, r, err, true)
		return
	}
	out := listPartsResult{Xmlns: xmlNS, Bucket: bucket, Key: key, UploadID: uploadID, MaxParts: 10000, IsTruncated: false, Owner: ownerXML()}
	for _, p := range parts {
		out.Part = append(out.Part, partXML{PartNumber: p.Number, ETag: quote(p.ETag), Size: p.Size, LastModified: xmlTime(time.Now().UTC())})
	}
	writeXML(w, http.StatusOK, out)
}

func (s *Server) routeTagging(w http.ResponseWriter, r *http.Request, ar *auth.Result, bucket, key string) {
	switch r.Method {
	case http.MethodGet:
		meta, err := s.Store.Head(bucket, key)
		if err != nil {
			s.storeErr(w, r, err, true)
			return
		}
		out := taggingXML{Xmlns: xmlNS}
		for k, v := range meta.Tags {
			out.TagSet.Tag = append(out.TagSet.Tag, tagXML{Key: k, Value: v})
		}
		sort.Slice(out.TagSet.Tag, func(i, j int) bool { return out.TagSet.Tag[i].Key < out.TagSet.Tag[j].Key })
		writeXML(w, http.StatusOK, out)
	case http.MethodPut:
		body, err := s.readLimited(r, ar, 64<<10)
		if err != nil {
			s.storeErr(w, r, err, true)
			return
		}
		var in taggingXML
		if err := xml.Unmarshal(body, &in); err != nil {
			s.err(w, r, http.StatusBadRequest, "MalformedXML", "The XML you provided was not well-formed.", r.URL.Path)
			return
		}
		tags := map[string]string{}
		for _, t := range in.TagSet.Tag {
			if t.Key == "" || len(t.Key) > 128 || len(t.Value) > 256 {
				s.err(w, r, http.StatusBadRequest, "InvalidTag", "The tag is invalid.", r.URL.Path)
				return
			}
			tags[t.Key] = t.Value
		}
		if len(tags) > 10 {
			s.err(w, r, http.StatusBadRequest, "InvalidTag", "The tag set has too many tags.", r.URL.Path)
			return
		}
		err = s.Store.UpdateMeta(bucket, key, func(m *store.Meta) error {
			m.Tags = tags
			return nil
		})
		if err != nil {
			s.storeErr(w, r, err, true)
			return
		}
		w.WriteHeader(http.StatusOK)
	case http.MethodDelete:
		if err := s.Store.UpdateMeta(bucket, key, func(m *store.Meta) error {
			m.Tags = nil
			return nil
		}); err != nil {
			s.storeErr(w, r, err, true)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	default:
		s.err(w, r, http.StatusMethodNotAllowed, "MethodNotAllowed", "The specified method is not allowed.", r.URL.Path)
	}
}

func (s *Server) getACL(w http.ResponseWriter, r *http.Request, bucket, key string) {
	if key == "" {
		if _, err := s.Store.Bucket(bucket); err != nil {
			s.storeErr(w, r, err, false)
			return
		}
	} else if _, err := s.Store.Head(bucket, key); err != nil {
		s.storeErr(w, r, err, true)
		return
	}
	w.Header().Set("Content-Type", "application/xml")
	w.WriteHeader(http.StatusOK)
	_, _ = io.WriteString(w, xml.Header+`<AccessControlPolicy xmlns="`+xmlNS+`"><Owner><ID>`+ownerID+`</ID><DisplayName>`+ownerName+`</DisplayName></Owner><AccessControlList><Grant><Grantee xmlns:xsi="http://www.w3.org/2001/XMLSchema-instance" xsi:type="CanonicalUser"><ID>`+ownerID+`</ID><DisplayName>`+ownerName+`</DisplayName></Grantee><Permission>FULL_CONTROL</Permission></Grant></AccessControlList></AccessControlPolicy>`)
}

func (s *Server) preconditions(bucket, key, ifMatch, ifNone string) error {
	meta, err := s.Store.Head(bucket, key)
	exists := err == nil
	if err != nil && !errors.Is(err, store.ErrNotFound) {
		return err
	}
	if ifMatch != "" && (!exists || !etagMatch(ifMatch, meta.ETag)) {
		return errPrecondition
	}
	if ifNone != "" && exists && etagMatch(ifNone, meta.ETag) {
		return errPrecondition
	}
	return nil
}

func (s *Server) readLimited(r *http.Request, ar *auth.Result, max int64) ([]byte, error) {
	body, err := openBody(r, ar, max)
	if err != nil {
		return nil, err
	}
	defer body.Close()
	b, err := io.ReadAll(io.LimitReader(body, max+1))
	if err != nil {
		return nil, err
	}
	if int64(len(b)) > max {
		return nil, errTooLarge
	}
	return b, nil
}

func metaFromHeaders(h http.Header) store.Meta {
	meta := store.Meta{
		ContentType:        h.Get("Content-Type"),
		ContentDisposition: h.Get("Content-Disposition"),
		ContentEncoding:    cleanEncoding(h.Get("Content-Encoding")),
		CacheControl:       h.Get("Cache-Control"),
		Metadata:           map[string]string{},
		Tags:               parseTagQuery(h.Get("X-Amz-Tagging")),
		Checksums:          map[string]string{},
	}
	if meta.ContentType == "" {
		meta.ContentType = "application/octet-stream"
	}
	for k, vals := range h {
		if len(vals) == 0 {
			continue
		}
		lk := strings.ToLower(k)
		switch {
		case strings.HasPrefix(lk, "x-amz-meta-"):
			meta.Metadata[strings.TrimPrefix(lk, "x-amz-meta-")] = vals[0]
		case strings.HasPrefix(lk, "x-amz-checksum-"):
			meta.Checksums[strings.TrimPrefix(lk, "x-amz-checksum-")] = vals[0]
		}
	}
	return meta
}

func writeObjectHeaders(w http.ResponseWriter, meta store.Meta) {
	w.Header().Set("Content-Type", meta.ContentType)
	w.Header().Set("ETag", quote(meta.ETag))
	w.Header().Set("Last-Modified", meta.ModTime.UTC().Format(http.TimeFormat))
	w.Header().Set("Accept-Ranges", "bytes")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	if meta.ContentDisposition != "" {
		w.Header().Set("Content-Disposition", meta.ContentDisposition)
	}
	if meta.ContentEncoding != "" {
		w.Header().Set("Content-Encoding", meta.ContentEncoding)
	}
	if meta.CacheControl != "" {
		w.Header().Set("Cache-Control", meta.CacheControl)
	}
	for k, v := range meta.Metadata {
		w.Header().Set("X-Amz-Meta-"+k, v)
	}
	for k, v := range meta.Checksums {
		w.Header().Set("X-Amz-Checksum-"+k, v)
	}
}

func cleanEncoding(v string) string {
	var out []string
	for _, p := range strings.Split(v, ",") {
		p = strings.TrimSpace(p)
		if p == "" || strings.EqualFold(p, "aws-chunked") {
			continue
		}
		out = append(out, p)
	}
	return strings.Join(out, ",")
}

func parseTagQuery(v string) map[string]string {
	if v == "" {
		return nil
	}
	q, err := url.ParseQuery(v)
	if err != nil {
		return nil
	}
	out := map[string]string{}
	for k, vals := range q {
		if len(vals) > 0 {
			out[k] = vals[0]
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

func parseCopySource(h string) (string, string, error) {
	if i := strings.IndexByte(h, '?'); i >= 0 {
		h = h[:i]
	}
	decoded, err := url.PathUnescape(strings.TrimPrefix(h, "/"))
	if err != nil || decoded == "" {
		return "", "", errPrecondition
	}
	bucket, key, ok := strings.Cut(decoded, "/")
	if !ok || bucket == "" || key == "" {
		return "", "", errPrecondition
	}
	return bucket, key, nil
}

func etagMatch(header, etag string) bool {
	for _, p := range strings.Split(header, ",") {
		p = strings.TrimSpace(p)
		if p == "*" {
			return true
		}
		if strings.EqualFold(strings.Trim(p, `"`), etag) {
			return true
		}
	}
	return false
}

func quote(etag string) string {
	if strings.HasPrefix(etag, `"`) {
		return etag
	}
	return `"` + etag + `"`
}

func encodeKey(key string, enc bool) string {
	if !enc || key == "" {
		return key
	}
	return url.PathEscape(key)
}

func xmlTime(t time.Time) string {
	return t.UTC().Format("2006-01-02T15:04:05.000Z")
}

func ownerXML() ownerXMLType {
	return ownerXMLType{ID: ownerID, DisplayName: ownerName}
}
