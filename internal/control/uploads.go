package control

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"

	"github.com/melzi-cloud/sliced-bread/internal/apps"
)

const maxUpload = 50 << 20

func (s *Server) uploadDir() string { return filepath.Join(s.cfg.DataDir, "uploads") }

func (s *Server) uploadPath(id string) string {
	return filepath.Join(s.cfg.DataDir, "uploads", id+".tar.gz")
}

// uploadInfo returns size and the content hash of an upload.
func (s *Server) uploadInfo(id string) (*apps.Upload, bool) {
	size, ok := s.uploadSize(id)
	if !ok {
		return nil, false
	}
	f, err := os.Open(s.uploadPath(id))
	if err != nil {
		return nil, false
	}
	defer f.Close()
	sum, err := apps.ContentHash(f)
	if err != nil {
		return nil, false
	}
	return &apps.Upload{ID: id, Bytes: size, SHA256: sum}, true
}

func (s *Server) uploadSize(id string) (int64, bool) {
	if !s.ensureLocal(context.Background(), id) {
		return 0, false
	}
	fi, err := os.Stat(s.uploadPath(id))
	if err != nil {
		return 0, false
	}
	return fi.Size(), true
}

func (s *Server) uploadSig(id string) string {
	m := hmac.New(sha256.New, append([]byte("upload:"), s.auth.key...))
	m.Write([]byte(id))
	return hex.EncodeToString(m.Sum(nil))
}

// uploadURL is the in-cluster URL a build job fetches the source from, or "" for repo apps.
func (s *Server) uploadURL(a *apps.App) string {
	if a.Upload == nil {
		return ""
	}
	return "http://" + s.cfg.InternalHostname + ":8080/internal/uploads/" + a.Upload.ID + "?sig=" + s.uploadSig(a.Upload.ID)
}

// validateTarGz checks the upload is a gzip tarball without absolute or parent paths.
func validateTarGz(path string) (int, error) {
	f, err := os.Open(path)
	if err != nil {
		return 0, err
	}
	defer f.Close()
	gz, err := gzip.NewReader(f)
	if err != nil {
		return 0, errors.New("upload is not gzip")
	}
	tr := tar.NewReader(gz)
	n := 0
	for {
		h, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return n, errors.New("upload is not a valid tar archive")
		}
		name := filepath.ToSlash(h.Name)
		if strings.HasPrefix(name, "/") || strings.Contains("/"+name+"/", "/../") {
			return n, errors.New("upload contains unsafe path " + h.Name)
		}
		if h.Typeflag == tar.TypeSymlink || h.Typeflag == tar.TypeLink {
			if strings.HasPrefix(h.Linkname, "/") || strings.Contains("/"+h.Linkname+"/", "/../") {
				return n, errors.New("upload contains unsafe link " + h.Name)
			}
		}
		n++
	}
	if n == 0 {
		return 0, errors.New("upload is empty")
	}
	return n, nil
}

func (s *Server) apiUpload(w http.ResponseWriter, r *http.Request) {
	if err := os.MkdirAll(filepath.Join(s.cfg.DataDir, "uploads"), 0o700); err != nil {
		fail(w, 500, err.Error(), "")
		return
	}
	id := "u" + randID(10)
	tmp := s.uploadPath(id) + ".part"
	f, err := os.OpenFile(tmp, os.O_CREATE|os.O_WRONLY|os.O_EXCL, 0o600)
	if err != nil {
		fail(w, 500, err.Error(), "")
		return
	}
	n, err := io.Copy(f, io.LimitReader(r.Body, maxUpload+1))
	f.Close()
	if err != nil {
		os.Remove(tmp)
		fail(w, 400, "upload failed: "+err.Error(), "")
		return
	}
	if n > maxUpload {
		os.Remove(tmp)
		fail(w, 413, "upload larger than 50MB", "exclude build output and dependencies (node_modules, dist) from the directory")
		return
	}
	files, err := validateTarGz(tmp)
	if err != nil {
		os.Remove(tmp)
		fail(w, 400, err.Error(), "")
		return
	}
	if err := os.Rename(tmp, s.uploadPath(id)); err != nil {
		fail(w, 500, err.Error(), "")
		return
	}
	if err := s.pushUpload(r.Context(), id); err != nil {
		os.Remove(s.uploadPath(id))
		fail(w, 500, err.Error(), "")
		return
	}
	writeJSON(w, 201, map[string]any{"upload": id, "bytes": n, "files": files})
}

// handleInternalUpload serves an upload to a build job. Requests must carry the HMAC signature.
func (s *Server) handleInternalUpload(w http.ResponseWriter, r *http.Request) {
	id := strings.TrimPrefix(r.URL.Path, "/internal/uploads/")
	if _, ok := s.uploadSize(id); !ok {
		http.NotFound(w, r)
		return
	}
	if !hmac.Equal([]byte(r.URL.Query().Get("sig")), []byte(s.uploadSig(id))) {
		http.Error(w, "bad signature", http.StatusForbidden)
		return
	}
	w.Header().Set("Content-Type", "application/gzip")
	http.ServeFile(w, r, s.uploadPath(id))
}
