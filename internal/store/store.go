// Package store keeps buckets and objects on the local filesystem.
// Writes land in a temp file in the same directory and are renamed into
// place only after the body is fully read, so a failed upload never
// replaces a good object.
package store

import (
	"crypto/md5"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

var (
	ErrNotFound     = errors.New("not found")
	ErrNoSuchBucket = errors.New("no such bucket")
	ErrExists       = errors.New("exists")
	ErrNotEmpty     = errors.New("not empty")
	ErrInvalid      = errors.New("invalid")
	ErrPartMissing  = errors.New("part missing")
)

// Meta is the object metadata stored beside the bytes.
type Meta struct {
	Size               int64             `json:"size"`
	ETag               string            `json:"etag"`
	ContentType        string            `json:"contentType,omitempty"`
	ContentDisposition string            `json:"contentDisposition,omitempty"`
	ContentEncoding    string            `json:"contentEncoding,omitempty"`
	CacheControl       string            `json:"cacheControl,omitempty"`
	Metadata           map[string]string `json:"metadata,omitempty"`
	Tags               map[string]string `json:"tags,omitempty"`
	Checksums          map[string]string `json:"checksums,omitempty"`
	ModTime            time.Time         `json:"modTime"`
}

// Object is one stored key.
type Object struct {
	Key  string
	Meta Meta
}

// Bucket is one bucket and its creation time.
type Bucket struct {
	Name    string
	Created time.Time
}

// CORSRule is a stored browser access rule.
type CORSRule struct {
	Origins []string `json:"origins"`
	Methods []string `json:"methods"`
	Headers []string `json:"headers,omitempty"`
	Expose  []string `json:"expose,omitempty"`
	MaxAge  int      `json:"maxAge,omitempty"`
}

// Part is one piece of an in-progress multipart upload.
type Part struct {
	Number int
	ETag   string
	Size   int64
}

type bucketMeta struct {
	Created time.Time  `json:"created"`
	CORS    []CORSRule `json:"cors,omitempty"`
	CORSSet bool       `json:"corsSet,omitempty"`
}

type uploadMeta struct {
	Key                string            `json:"key"`
	ContentType        string            `json:"contentType,omitempty"`
	ContentDisposition string            `json:"contentDisposition,omitempty"`
	ContentEncoding    string            `json:"contentEncoding,omitempty"`
	CacheControl       string            `json:"cacheControl,omitempty"`
	Metadata           map[string]string `json:"metadata,omitempty"`
	Tags               map[string]string `json:"tags,omitempty"`
	Created            time.Time         `json:"created"`
}

// Store is a filesystem object store rooted at a single directory.
type Store struct {
	root  string
	mu    sync.Mutex
	locks map[string]*sync.RWMutex
}

// Open creates the data directory if needed.
func Open(root string) (*Store, error) {
	if err := os.MkdirAll(root, 0o700); err != nil {
		return nil, err
	}
	return &Store{root: root, locks: map[string]*sync.RWMutex{}}, nil
}

func (s *Store) lock(bucket string) *sync.RWMutex {
	s.mu.Lock()
	defer s.mu.Unlock()
	l, ok := s.locks[bucket]
	if !ok {
		l = &sync.RWMutex{}
		s.locks[bucket] = l
	}
	return l
}

// CreateBucket creates an empty bucket.
func (s *Store) CreateBucket(name string) error {
	if err := validBucket(name); err != nil {
		return err
	}
	l := s.lock(name)
	l.Lock()
	defer l.Unlock()
	dir := s.bucketDir(name)
	if _, err := os.Stat(filepath.Join(dir, "bucket.json")); err == nil {
		return ErrExists
	}
	if err := os.MkdirAll(filepath.Join(dir, "o"), 0o700); err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Join(dir, "m"), 0o700); err != nil {
		return err
	}
	return writeJSON(filepath.Join(dir, "bucket.json"), bucketMeta{Created: time.Now().UTC()})
}

// DeleteBucket removes an empty bucket.
func (s *Store) DeleteBucket(name string) error {
	l := s.lock(name)
	l.Lock()
	defer l.Unlock()
	if _, err := s.readBucket(name); err != nil {
		return err
	}
	empty, err := dirEmpty(filepath.Join(s.bucketDir(name), "o"))
	if err != nil || !empty {
		if err != nil {
			return err
		}
		return ErrNotEmpty
	}
	uploads := filepath.Join(s.bucketDir(name), "u")
	if st, err := os.Stat(uploads); err == nil && st.IsDir() {
		empty, err = dirEmpty(uploads)
		if err != nil {
			return err
		}
		if !empty {
			return ErrNotEmpty
		}
	}
	return os.RemoveAll(s.bucketDir(name))
}

// Bucket returns creation time.
func (s *Store) Bucket(name string) (Bucket, error) {
	l := s.lock(name)
	l.RLock()
	defer l.RUnlock()
	meta, err := s.readBucket(name)
	if err != nil {
		return Bucket{}, err
	}
	return Bucket{Name: name, Created: meta.Created}, nil
}

// ListBuckets returns every bucket sorted by name.
func (s *Store) ListBuckets() ([]Bucket, error) {
	root := filepath.Join(s.root, "b")
	entries, err := os.ReadDir(root)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var out []Bucket
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		b, err := s.Bucket(e.Name())
		if err != nil {
			continue
		}
		out = append(out, b)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

// PutCORS replaces the bucket CORS rules.
func (s *Store) PutCORS(name string, rules []CORSRule) error {
	l := s.lock(name)
	l.Lock()
	defer l.Unlock()
	meta, err := s.readBucket(name)
	if err != nil {
		return err
	}
	meta.CORS = rules
	meta.CORSSet = true
	return writeJSON(filepath.Join(s.bucketDir(name), "bucket.json"), meta)
}

// CORS returns rules and whether the bucket set them explicitly.
func (s *Store) CORS(name string) ([]CORSRule, bool, error) {
	l := s.lock(name)
	l.RLock()
	defer l.RUnlock()
	meta, err := s.readBucket(name)
	if err != nil {
		return nil, false, err
	}
	return meta.CORS, meta.CORSSet, nil
}

// DeleteCORS clears explicit CORS rules and restores the server default.
func (s *Store) DeleteCORS(name string) error {
	l := s.lock(name)
	l.Lock()
	defer l.Unlock()
	meta, err := s.readBucket(name)
	if err != nil {
		return err
	}
	meta.CORS = nil
	meta.CORSSet = false
	return writeJSON(filepath.Join(s.bucketDir(name), "bucket.json"), meta)
}

// PutObject stores r as key. The reader must return a non-EOF error if the
// body should be rejected; the temp file is then discarded.
func (s *Store) PutObject(bucket, key string, r io.Reader, in Meta) (Meta, error) {
	if err := validBucket(bucket); err != nil {
		return Meta{}, err
	}
	key, err := validKey(key)
	if err != nil {
		return Meta{}, err
	}
	l := s.lock(bucket)
	l.Lock()
	defer l.Unlock()
	if _, err := s.readBucket(bucket); err != nil {
		return Meta{}, err
	}
	return s.putLocked(bucket, key, r, in)
}

func (s *Store) putLocked(bucket, key string, r io.Reader, in Meta) (Meta, error) {
	objPath, err := s.objectPath(bucket, key)
	if err != nil {
		return Meta{}, err
	}
	if err := os.MkdirAll(filepath.Dir(objPath), 0o700); err != nil {
		return Meta{}, err
	}
	tmp, err := os.CreateTemp(filepath.Dir(objPath), ".up-*")
	if err != nil {
		return Meta{}, err
	}
	tmpName := tmp.Name()
	ok := false
	defer func() {
		if !ok {
			_ = os.Remove(tmpName)
		}
	}()
	h := md5.New()
	n, err := io.Copy(tmp, io.TeeReader(r, h))
	if err != nil {
		_ = tmp.Close()
		return Meta{}, err
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return Meta{}, err
	}
	if err := tmp.Close(); err != nil {
		return Meta{}, err
	}
	if err := os.Chmod(tmpName, 0o600); err != nil {
		return Meta{}, err
	}
	if err := os.Rename(tmpName, objPath); err != nil {
		return Meta{}, err
	}
	ok = true
	_ = syncDir(filepath.Dir(objPath))
	in.Size = n
	in.ETag = hex.EncodeToString(h.Sum(nil))
	in.ModTime = time.Now().UTC()
	if err := s.writeMeta(bucket, key, in); err != nil {
		return Meta{}, err
	}
	return in, nil
}

// Get returns a reader for the object. The caller closes it.
func (s *Store) Get(bucket, key string) (io.ReadSeekCloser, Meta, error) {
	key, err := validKey(key)
	if err != nil {
		return nil, Meta{}, err
	}
	l := s.lock(bucket)
	l.RLock()
	if _, err := s.readBucket(bucket); err != nil {
		l.RUnlock()
		return nil, Meta{}, err
	}
	meta, err := s.readMeta(bucket, key)
	if err != nil {
		l.RUnlock()
		return nil, Meta{}, err
	}
	f, err := os.Open(mustPath(s.objectPath(bucket, key)))
	if err != nil {
		l.RUnlock()
		if errors.Is(err, os.ErrNotExist) {
			return nil, Meta{}, ErrNotFound
		}
		return nil, Meta{}, err
	}
	l.RUnlock()
	return f, meta, nil
}

// UpdateMeta rewrites metadata for an existing object. The size and ETag stay put.
func (s *Store) UpdateMeta(bucket, key string, fn func(*Meta) error) error {
	key, err := validKey(key)
	if err != nil {
		return err
	}
	l := s.lock(bucket)
	l.Lock()
	defer l.Unlock()
	if _, err := s.readBucket(bucket); err != nil {
		return err
	}
	meta, err := s.readMeta(bucket, key)
	if err != nil {
		return err
	}
	if err := fn(&meta); err != nil {
		return err
	}
	return s.writeMeta(bucket, key, meta)
}

// Head returns metadata.
func (s *Store) Head(bucket, key string) (Meta, error) {
	key, err := validKey(key)
	if err != nil {
		return Meta{}, err
	}
	l := s.lock(bucket)
	l.RLock()
	defer l.RUnlock()
	if _, err := s.readBucket(bucket); err != nil {
		return Meta{}, err
	}
	return s.readMeta(bucket, key)
}

// DeleteObject removes a key. Missing keys are not an error.
func (s *Store) DeleteObject(bucket, key string) error {
	key, err := validKey(key)
	if err != nil {
		return err
	}
	l := s.lock(bucket)
	l.Lock()
	defer l.Unlock()
	if _, err := s.readBucket(bucket); err != nil {
		return err
	}
	obj, err := s.objectPath(bucket, key)
	if err != nil {
		return err
	}
	_ = os.Remove(obj)
	_ = os.Remove(s.metaPath(bucket, key))
	return nil
}

// List returns every object in the bucket. prefix restricts the result when set.
func (s *Store) List(bucket, prefix string) ([]Object, error) {
	l := s.lock(bucket)
	l.RLock()
	defer l.RUnlock()
	if _, err := s.readBucket(bucket); err != nil {
		return nil, err
	}
	root := filepath.Join(s.bucketDir(bucket), "o")
	var out []Object
	err := filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() || !strings.HasSuffix(d.Name(), ".obj") {
			return nil
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		key := filepath.ToSlash(strings.TrimSuffix(rel, ".obj"))
		if prefix != "" && !strings.HasPrefix(key, prefix) {
			return nil
		}
		meta, err := s.readMeta(bucket, key)
		if err != nil {
			return nil
		}
		out = append(out, Object{Key: key, Meta: meta})
		return nil
	})
	if err != nil {
		return nil, err
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Key < out[j].Key })
	return out, nil
}

// CopyObject copies bytes from src to dst. in replaces metadata when replace is set.
func (s *Store) CopyObject(dstBucket, dstKey, srcBucket, srcKey string, replace bool, in Meta) (Meta, error) {
	src, meta, err := s.Get(srcBucket, srcKey)
	if err != nil {
		return Meta{}, err
	}
	defer src.Close()
	if !replace {
		in = meta
	} else {
		in.Checksums = meta.Checksums
	}
	return s.PutObject(dstBucket, dstKey, src, in)
}

// CreateMultipart starts an upload and returns its id.
func (s *Store) CreateMultipart(bucket, key string, in Meta) (string, error) {
	if err := validBucket(bucket); err != nil {
		return "", err
	}
	key, err := validKey(key)
	if err != nil {
		return "", err
	}
	l := s.lock(bucket)
	l.Lock()
	defer l.Unlock()
	if _, err := s.readBucket(bucket); err != nil {
		return "", err
	}
	id, err := randomID()
	if err != nil {
		return "", err
	}
	dir := s.uploadDir(bucket, id)
	if err := os.MkdirAll(filepath.Join(dir, "parts"), 0o700); err != nil {
		return "", err
	}
	err = writeJSON(filepath.Join(dir, "upload.json"), uploadMeta{
		Key:                key,
		ContentType:        in.ContentType,
		ContentDisposition: in.ContentDisposition,
		ContentEncoding:    in.ContentEncoding,
		CacheControl:       in.CacheControl,
		Metadata:           in.Metadata,
		Tags:               in.Tags,
		Created:            time.Now().UTC(),
	})
	if err != nil {
		return "", err
	}
	return id, nil
}

// PutPart stores one part. part is 1-based.
func (s *Store) PutPart(bucket, uploadID string, part int, r io.Reader) (Part, error) {
	if part < 1 || part > 10000 || !validID(uploadID) {
		return Part{}, ErrInvalid
	}
	l := s.lock(bucket)
	l.Lock()
	defer l.Unlock()
	up, err := s.readUpload(bucket, uploadID)
	if err != nil {
		return Part{}, err
	}
	_ = up
	dir := filepath.Join(s.uploadDir(bucket, uploadID), "parts")
	name := partName(part)
	tmp, err := os.CreateTemp(dir, ".part-*")
	if err != nil {
		return Part{}, err
	}
	tmpName := tmp.Name()
	ok := false
	defer func() {
		if !ok {
			_ = os.Remove(tmpName)
		}
	}()
	h := md5.New()
	n, err := io.Copy(tmp, io.TeeReader(r, h))
	if err != nil {
		_ = tmp.Close()
		return Part{}, err
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return Part{}, err
	}
	if err := tmp.Close(); err != nil {
		return Part{}, err
	}
	dest := filepath.Join(dir, name)
	if err := os.Rename(tmpName, dest); err != nil {
		return Part{}, err
	}
	ok = true
	p := Part{Number: part, ETag: hex.EncodeToString(h.Sum(nil)), Size: n}
	if err := writeJSON(dest+".json", p); err != nil {
		return Part{}, err
	}
	return p, nil
}

// ListParts returns uploaded parts in order.
func (s *Store) ListParts(bucket, uploadID string) (string, []Part, error) {
	if !validID(uploadID) {
		return "", nil, ErrInvalid
	}
	l := s.lock(bucket)
	l.RLock()
	defer l.RUnlock()
	up, err := s.readUpload(bucket, uploadID)
	if err != nil {
		return "", nil, err
	}
	parts, err := s.parts(bucket, uploadID)
	return up.Key, parts, err
}

// CompleteMultipart concatenates the listed parts into the final object.
func (s *Store) CompleteMultipart(bucket, uploadID string, numbers []int, etags []string) (string, Meta, error) {
	if !validID(uploadID) || len(numbers) == 0 || len(numbers) != len(etags) {
		return "", Meta{}, ErrInvalid
	}
	l := s.lock(bucket)
	l.Lock()
	defer l.Unlock()
	up, err := s.readUpload(bucket, uploadID)
	if err != nil {
		return "", Meta{}, err
	}
	var readers []io.Reader
	var closers []io.Closer
	var md5s []string
	var total int64
	defer func() {
		for _, c := range closers {
			_ = c.Close()
		}
	}()
	for i, n := range numbers {
		if i > 0 && numbers[i] <= numbers[i-1] {
			return "", Meta{}, ErrInvalid
		}
		p, err := s.readPart(bucket, uploadID, n)
		if err != nil {
			return "", Meta{}, err
		}
		want := strings.Trim(etags[i], `"`)
		if want != "" && !strings.EqualFold(want, p.ETag) {
			return "", Meta{}, ErrInvalid
		}
		f, err := os.Open(filepath.Join(s.uploadDir(bucket, uploadID), "parts", partName(n)))
		if err != nil {
			return "", Meta{}, err
		}
		closers = append(closers, f)
		readers = append(readers, f)
		md5s = append(md5s, p.ETag)
		total += p.Size
	}
	etag, err := compositeETag(md5s)
	if err != nil {
		return "", Meta{}, err
	}
	// putLocked hashes the bytes for a single-part ETag. Multipart ETags are the
	// hash of the part hashes, so replace it after the object is stored.
	meta, err := s.putLocked(bucket, up.Key, io.MultiReader(readers...), Meta{
		ContentType:        up.ContentType,
		ContentDisposition: up.ContentDisposition,
		ContentEncoding:    up.ContentEncoding,
		CacheControl:       up.CacheControl,
		Metadata:           up.Metadata,
		Tags:               up.Tags,
	})
	if err != nil {
		return "", Meta{}, err
	}
	meta.ETag = etag
	meta.Size = total
	if err := s.writeMeta(bucket, up.Key, meta); err != nil {
		return "", Meta{}, err
	}
	_ = os.RemoveAll(s.uploadDir(bucket, uploadID))
	return up.Key, meta, nil
}

// AbortMultipart deletes an in-progress upload.
func (s *Store) AbortMultipart(bucket, uploadID string) error {
	if !validID(uploadID) {
		return ErrInvalid
	}
	l := s.lock(bucket)
	l.Lock()
	defer l.Unlock()
	if _, err := s.readUpload(bucket, uploadID); err != nil {
		return err
	}
	return os.RemoveAll(s.uploadDir(bucket, uploadID))
}

func (s *Store) readBucket(name string) (bucketMeta, error) {
	var meta bucketMeta
	b, err := os.ReadFile(filepath.Join(s.bucketDir(name), "bucket.json"))
	if errors.Is(err, os.ErrNotExist) {
		return meta, ErrNoSuchBucket
	}
	if err != nil {
		return meta, err
	}
	if err := json.Unmarshal(b, &meta); err != nil {
		return meta, err
	}
	return meta, nil
}

func (s *Store) objectPath(bucket, key string) (string, error) {
	return s.under(filepath.Join("b", bucket, "o"), key+".obj")
}

func (s *Store) metaPath(bucket, key string) string {
	p, err := s.under(filepath.Join("b", bucket, "m"), key+".json")
	if err != nil {
		return ""
	}
	return p
}

func (s *Store) under(prefix, key string) (string, error) {
	full := filepath.Join(s.root, prefix, filepath.FromSlash(key))
	rel, err := filepath.Rel(s.root, full)
	if err != nil || strings.HasPrefix(rel, "..") {
		return "", ErrInvalid
	}
	return full, nil
}

func (s *Store) writeMeta(bucket, key string, meta Meta) error {
	path := s.metaPath(bucket, key)
	if path == "" {
		return ErrInvalid
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	return writeJSON(path, meta)
}

func (s *Store) readMeta(bucket, key string) (Meta, error) {
	var meta Meta
	b, err := os.ReadFile(s.metaPath(bucket, key))
	if errors.Is(err, os.ErrNotExist) {
		return meta, ErrNotFound
	}
	if err != nil {
		return meta, err
	}
	if err := json.Unmarshal(b, &meta); err != nil {
		return meta, err
	}
	return meta, nil
}

func (s *Store) bucketDir(name string) string {
	return filepath.Join(s.root, "b", name)
}

func (s *Store) uploadDir(bucket, id string) string {
	return filepath.Join(s.bucketDir(bucket), "u", id)
}

func (s *Store) readUpload(bucket, id string) (uploadMeta, error) {
	var up uploadMeta
	b, err := os.ReadFile(filepath.Join(s.uploadDir(bucket, id), "upload.json"))
	if errors.Is(err, os.ErrNotExist) {
		return up, ErrNotFound
	}
	if err != nil {
		return up, err
	}
	if err := json.Unmarshal(b, &up); err != nil {
		return up, err
	}
	return up, nil
}

func (s *Store) readPart(bucket, id string, n int) (Part, error) {
	var p Part
	b, err := os.ReadFile(filepath.Join(s.uploadDir(bucket, id), "parts", partName(n)+".json"))
	if errors.Is(err, os.ErrNotExist) {
		return p, ErrPartMissing
	}
	if err != nil {
		return p, err
	}
	if err := json.Unmarshal(b, &p); err != nil {
		return p, err
	}
	return p, nil
}

func (s *Store) parts(bucket, id string) ([]Part, error) {
	dir := filepath.Join(s.uploadDir(bucket, id), "parts")
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	var out []Part
	for _, e := range entries {
		if !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		var p Part
		b, err := os.ReadFile(filepath.Join(dir, e.Name()))
		if err != nil {
			return nil, err
		}
		if err := json.Unmarshal(b, &p); err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Number < out[j].Number })
	return out, nil
}

func partName(n int) string {
	return hex.EncodeToString([]byte{byte(n >> 8), byte(n)})
}

func compositeETag(etags []string) (string, error) {
	h := md5.New()
	for _, e := range etags {
		b, err := hex.DecodeString(e)
		if err != nil || len(b) != md5.Size {
			return "", ErrInvalid
		}
		_, _ = h.Write(b)
	}
	return hex.EncodeToString(h.Sum(nil)) + "-" + itoa(len(etags)), nil
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b [20]byte
	i := len(b)
	for n > 0 {
		i--
		b[i] = byte('0' + n%10)
		n /= 10
	}
	return string(b[i:])
}

func randomID() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(b[:]), nil
}

func validID(id string) bool {
	if len(id) != 32 {
		return false
	}
	_, err := hex.DecodeString(id)
	return err == nil
}

func validBucket(name string) error {
	if len(name) < 3 || len(name) > 63 {
		return ErrInvalid
	}
	for i := 0; i < len(name); i++ {
		c := name[i]
		switch {
		case c >= 'a' && c <= 'z', c >= '0' && c <= '9':
		case c == '.' || c == '-':
			if i == 0 || i == len(name)-1 {
				return ErrInvalid
			}
		default:
			return ErrInvalid
		}
	}
	if strings.Contains(name, "..") || strings.Contains(name, ".-") || strings.Contains(name, "-.") {
		return ErrInvalid
	}
	if net.ParseIP(name) != nil {
		return ErrInvalid
	}
	return nil
}

func validKey(key string) (string, error) {
	key = strings.TrimPrefix(key, "/")
	if key == "" || len(key) > 1024 || strings.ContainsRune(key, 0) {
		return "", ErrInvalid
	}
	parts := strings.Split(key, "/")
	for _, p := range parts {
		if p == "" || p == "." || p == ".." || strings.ContainsAny(p, `\`) {
			return "", ErrInvalid
		}
	}
	return strings.Join(parts, "/"), nil
}

func writeJSON(path string, v any) error {
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	return syncDir(filepath.Dir(path))
}

func syncDir(dir string) error {
	f, err := os.Open(dir)
	if err != nil {
		return err
	}
	defer f.Close()
	return f.Sync()
}

func dirEmpty(path string) (bool, error) {
	entries, err := os.ReadDir(path)
	if errors.Is(err, os.ErrNotExist) {
		return true, nil
	}
	if err != nil {
		return false, err
	}
	for _, e := range entries {
		name := e.Name()
		if strings.HasPrefix(name, ".") {
			continue
		}
		return false, nil
	}
	return true, nil
}

func mustPath(path string, err error) string {
	if err != nil {
		return ""
	}
	return path
}
