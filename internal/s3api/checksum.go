package s3api

import (
	"encoding/base64"
	"hash/crc32"
)

func crc32Sum(b []byte) string {
	h := crc32.NewIEEE()
	_, _ = h.Write(b)
	return base64.StdEncoding.EncodeToString(h.Sum(nil))
}
