package library

import (
	"crypto/md5"
	"encoding/hex"
	"io"
	"os"
)

// PartialMD5 is the book identity KOReader (util.partialMD5), Readest (utils/md5.ts) and BookRoom share:
// MD5 over 1 KiB samples at offsets 1024<<(2i) for i = -1..10. LuaJIT and JavaScript mask the shift count to
// five bits, so i = -1 shifts by 30 and the 32-bit result overflows to 0: the first sample is at offset 0.
// Sampling stops at the first offset past the end of the file; the last sample may be short.
func PartialMD5(r io.ReaderAt, size int64) (string, error) {
	h := md5.New()
	buf := make([]byte, 1024)
	for i := -1; i <= 10; i++ {
		var off int64
		if i >= 0 {
			off = 1024 << (2 * i)
		}
		if off >= size {
			break
		}
		n, err := r.ReadAt(buf[:min(1024, size-off)], off)
		if err != nil && err != io.EOF {
			return "", err
		}
		h.Write(buf[:n])
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// PartialMD5File hashes a file on disk.
func PartialMD5File(path string) (string, int64, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", 0, err
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return "", 0, err
	}
	sum, err := PartialMD5(f, st.Size())
	return sum, st.Size(), err
}

// IsHash reports whether s looks like a partial MD5 (32 lowercase hex characters).
func IsHash(s string) bool {
	if len(s) != 32 {
		return false
	}
	for i := range len(s) {
		c := s[i]
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	return true
}
