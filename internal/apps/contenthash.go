package apps

import (
	"archive/tar"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"path/filepath"
	"sort"
)

// ContentHash hashes the regular files of a gzip tarball by path and content only,
// ignoring timestamps and ownership, so it matches `sb deploy --dry-run` on an unchanged folder.
func ContentHash(r io.Reader) (string, error) {
	gz, err := gzip.NewReader(r)
	if err != nil {
		return "", err
	}
	tr := tar.NewReader(gz)
	var lines []string
	for {
		h, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return "", err
		}
		if h.Typeflag != tar.TypeReg {
			continue
		}
		fh := sha256.New()
		if _, err := io.Copy(fh, tr); err != nil {
			return "", err
		}
		lines = append(lines, filepath.ToSlash(h.Name)+"\x00"+hex.EncodeToString(fh.Sum(nil)))
	}
	sort.Strings(lines)
	h := sha256.New()
	for _, l := range lines {
		h.Write([]byte(l + "\n"))
	}
	return hex.EncodeToString(h.Sum(nil))[:12], nil
}
