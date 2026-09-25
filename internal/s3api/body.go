package s3api

import (
	"bufio"
	"crypto/md5"
	"crypto/sha1"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"hash"
	"hash/crc32"
	"io"
	"net/http"
	"sort"
	"strconv"
	"strings"

	"storage/internal/auth"
)

var (
	errBadDigest   = errors.New("bad digest")
	errIncomplete  = errors.New("incomplete body")
	errTooLarge    = errors.New("object too large")
	errChunkFormat = errors.New("malformed chunked body")
)

// S3 clients send aws-chunked frames well under this. A larger frame is rejected
// instead of being buffered.
const maxChunk = 16 << 20

type bodyCloser struct {
	io.Reader
	io.Closer
}

// openBody returns the raw object bytes. Header signatures are already verified;
// this checks the bytes against the digests the client declared.
func openBody(r *http.Request, ar *auth.Result, max int64) (io.ReadCloser, error) {
	if r.Body == nil {
		r.Body = http.NoBody
	}
	var src io.Reader = r.Body
	expectN := int64(-1)
	if ar.Streaming() {
		decoded, err := strconv.ParseInt(r.Header.Get("X-Amz-Decoded-Content-Length"), 10, 64)
		if err != nil || decoded < 0 {
			return nil, errChunkFormat
		}
		if max > 0 && decoded > max {
			return nil, errTooLarge
		}
		src = newChunkReader(r.Body, ar, r.Header.Get("X-Amz-Trailer"))
		expectN = decoded
	} else if r.ContentLength >= 0 {
		if max > 0 && r.ContentLength > max {
			return nil, errTooLarge
		}
		expectN = r.ContentLength
	}
	vr := &verifyReader{r: src, max: max, expectN: expectN}
	if isHexDigest(ar.PayloadHash) {
		vr.sha = sha256.New()
		vr.expectSHA = strings.ToLower(ar.PayloadHash)
	}
	addHeaderChecksums(r.Header, vr)
	return bodyCloser{Reader: vr, Closer: r.Body}, nil
}

func addHeaderChecksums(h http.Header, vr *verifyReader) {
	if v := h.Get("Content-Md5"); v != "" {
		vr.checks = append(vr.checks, digestCheck{h: md5.New(), want: v})
	}
	for _, name := range []string{"crc32", "crc32c", "sha1", "sha256"} {
		val := h.Get("X-Amz-Checksum-" + name)
		if val == "" {
			continue
		}
		sum, ok := newChecksum(name)
		if !ok {
			continue
		}
		vr.checks = append(vr.checks, digestCheck{h: sum, want: val})
	}
}

func isHexDigest(s string) bool {
	if len(s) != 64 {
		return false
	}
	_, err := hex.DecodeString(s)
	return err == nil
}

type digestCheck struct {
	h    hash.Hash
	want string
}

type verifyReader struct {
	r         io.Reader
	sha       hash.Hash
	expectSHA string
	checks    []digestCheck
	written   int64
	max       int64
	expectN   int64
	err       error
}

func (v *verifyReader) Read(p []byte) (int, error) {
	if v.err != nil {
		return 0, v.err
	}
	n, err := v.r.Read(p)
	if n > 0 {
		v.written += int64(n)
		if v.max > 0 && v.written > v.max {
			v.err = errTooLarge
			return n, v.err
		}
		if v.sha != nil {
			_, _ = v.sha.Write(p[:n])
		}
		for _, c := range v.checks {
			_, _ = c.h.Write(p[:n])
		}
	}
	if errors.Is(err, io.EOF) {
		if e := v.finish(); e != nil {
			v.err = e
			return n, e
		}
	} else if err != nil {
		v.err = err
	}
	return n, err
}

func (v *verifyReader) finish() error {
	if v.expectN >= 0 && v.written != v.expectN {
		return errIncomplete
	}
	if v.expectSHA != "" {
		if !hexEqual(hex.EncodeToString(v.sha.Sum(nil)), v.expectSHA) {
			return errBadDigest
		}
	}
	for _, c := range v.checks {
		if base64.StdEncoding.EncodeToString(c.h.Sum(nil)) != c.want {
			return errBadDigest
		}
	}
	return nil
}

func hexEqual(a, b string) bool {
	ab, err1 := hex.DecodeString(a)
	bb, err2 := hex.DecodeString(b)
	if err1 != nil || err2 != nil || len(ab) != len(bb) || len(ab) == 0 {
		return false
	}
	diff := 0
	for i := range ab {
		diff |= int(ab[i] ^ bb[i])
	}
	return diff == 0
}

func newChecksum(name string) (hash.Hash, bool) {
	switch name {
	case "crc32":
		return crc32.NewIEEE(), true
	case "crc32c":
		return crc32.New(crc32.MakeTable(crc32.Castagnoli)), true
	case "sha1":
		return sha1.New(), true
	case "sha256":
		return sha256.New(), true
	default:
		return nil, false
	}
}

type chunkReader struct {
	in       *bufio.Reader
	left     int
	chunkSum hash.Hash
	payload  map[string]hash.Hash
	eof      bool
	err      error
	signed   bool
	trailers bool
	key      []byte
	dateTime string
	scope    string
	prev     string
	sig      string
	hasSig   bool
	trailer  string
}

func newChunkReader(in io.Reader, ar *auth.Result, trailer string) *chunkReader {
	c := &chunkReader{
		in:       bufio.NewReaderSize(in, 64<<10),
		signed:   ar.SignedChunks(),
		trailers: ar.HasTrailers(),
		key:      ar.SigningKey,
		dateTime: ar.DateTime,
		scope:    ar.Scope,
		prev:     ar.Signature,
		trailer:  trailer,
	}
	if ar.HasTrailers() {
		c.payload = map[string]hash.Hash{}
		for _, name := range splitCSV(trailer) {
			algo := strings.TrimPrefix(strings.ToLower(strings.TrimSpace(name)), "x-amz-checksum-")
			if h, ok := newChecksum(algo); ok {
				c.payload[algo] = h
			}
		}
		if len(c.payload) == 0 {
			for _, name := range []string{"crc32", "crc32c", "sha1", "sha256"} {
				h, _ := newChecksum(name)
				c.payload[name] = h
			}
		}
	}
	return c
}

func (c *chunkReader) Read(p []byte) (int, error) {
	if c.err != nil {
		return 0, c.err
	}
	if c.eof {
		return 0, io.EOF
	}
	if c.left == 0 {
		if err := c.openChunk(); err != nil {
			c.err = err
			return 0, err
		}
		if c.eof {
			return 0, io.EOF
		}
	}
	n := len(p)
	if n > c.left {
		n = c.left
	}
	n, err := c.in.Read(p[:n])
	if n > 0 {
		_, _ = c.chunkSum.Write(p[:n])
		for _, h := range c.payload {
			_, _ = h.Write(p[:n])
		}
		c.left -= n
	}
	if err != nil {
		c.err = err
		return n, err
	}
	if c.left == 0 {
		if err := c.finishChunk(); err != nil {
			c.err = err
			return n, err
		}
	}
	return n, nil
}

func (c *chunkReader) openChunk() error {
	line, err := c.readLine()
	if err != nil {
		return errChunkFormat
	}
	size, sig, hasSig, err := parseChunkHeader(line)
	if err != nil || size < 0 || size > maxChunk {
		return errChunkFormat
	}
	c.sig = sig
	c.hasSig = hasSig
	c.chunkSum = sha256.New()
	c.left = int(size)
	if size == 0 {
		// A completion chunk is only the size line. Trailers follow it directly.
		if err := c.verifyChunk(); err != nil {
			return err
		}
		if c.trailers {
			if err := c.readTrailers(); err != nil {
				return err
			}
		}
		c.eof = true
	}
	return nil
}

func (c *chunkReader) finishChunk() error {
	if err := c.readCRLF(); err != nil {
		return err
	}
	return c.verifyChunk()
}

func (c *chunkReader) verifyChunk() error {
	if !c.signed {
		return nil
	}
	if !c.hasSig {
		return errChunkFormat
	}
	want := auth.ChunkSignatureHash(c.key, c.dateTime, c.scope, c.prev, c.chunkSum.Sum(nil))
	if !hexEqual(want, c.sig) {
		return auth.ErrSignature
	}
	c.prev = strings.ToLower(c.sig)
	return nil
}

func (c *chunkReader) readTrailers() error {
	var lines [][2]string
	for {
		line, err := c.readLine()
		if err != nil {
			return errChunkFormat
		}
		if line == "" {
			break
		}
		name, val, ok := strings.Cut(line, ":")
		if !ok {
			return errChunkFormat
		}
		lines = append(lines, [2]string{strings.ToLower(strings.TrimSpace(name)), strings.TrimSpace(val)})
	}
	if c.signed {
		canon, sig := trailerCanonical(lines)
		if sig == "" || !hexEqual(auth.TrailerSignature(c.key, c.dateTime, c.scope, c.prev, canon), sig) {
			return auth.ErrSignature
		}
	}
	for _, ln := range lines {
		algo := strings.TrimPrefix(ln[0], "x-amz-checksum-")
		if algo == ln[0] {
			continue
		}
		h, ok := c.payload[algo]
		if !ok {
			continue
		}
		if base64.StdEncoding.EncodeToString(h.Sum(nil)) != ln[1] {
			return errBadDigest
		}
	}
	return nil
}

func trailerCanonical(lines [][2]string) (string, string) {
	var sig string
	vals := map[string]string{}
	var names []string
	for _, ln := range lines {
		if ln[0] == "x-amz-trailer-signature" {
			sig = ln[1]
			continue
		}
		names = append(names, ln[0])
		vals[ln[0]] = ln[1]
	}
	sort.Strings(names)
	var b strings.Builder
	for _, n := range names {
		b.WriteString(n)
		b.WriteByte(':')
		b.WriteString(vals[n])
		b.WriteByte('\n')
	}
	return b.String(), sig
}

func splitCSV(s string) []string {
	if strings.TrimSpace(s) == "" {
		return nil
	}
	return strings.Split(s, ",")
}

func parseChunkHeader(line string) (size int64, sig string, hasSig bool, err error) {
	line = strings.TrimSpace(line)
	head := line
	if i := strings.IndexByte(line, ';'); i >= 0 {
		head = line[:i]
		rest := line[i+1:]
		const prefix = "chunk-signature="
		if strings.HasPrefix(rest, prefix) {
			sig = rest[len(prefix):]
			if j := strings.IndexByte(sig, ';'); j >= 0 {
				sig = sig[:j]
			}
			hasSig = true
		}
	}
	size, err = strconv.ParseInt(head, 16, 64)
	return size, sig, hasSig, err
}

func (c *chunkReader) readLine() (string, error) {
	line, err := c.in.ReadString('\n')
	if err != nil {
		return "", err
	}
	return strings.TrimRight(line, "\r\n"), nil
}

func (c *chunkReader) readCRLF() error {
	b, err := c.in.ReadByte()
	if err != nil || b != '\r' {
		return errChunkFormat
	}
	b, err = c.in.ReadByte()
	if err != nil || b != '\n' {
		return errChunkFormat
	}
	return nil
}
