package control

import (
	"context"
	"fmt"
	"io"
	"os"
	"time"

	"github.com/google/go-containerregistry/pkg/name"
	"github.com/google/go-containerregistry/pkg/v1/empty"
	"github.com/google/go-containerregistry/pkg/v1/mutate"
	"github.com/google/go-containerregistry/pkg/v1/remote"
	"github.com/google/go-containerregistry/pkg/v1/static"
	"github.com/google/go-containerregistry/pkg/v1/types"

	"github.com/melzi-cloud/sliced-bread/internal/tenant"
)

// Uploaded source lives in the in-cluster registry (repository sb-uploads, one tag per
// upload), so any sb-control replica can serve it and none needs a volume. The local
// directory is only a cache.

const uploadsRepo = "sb-uploads"

func uploadRef(id string) (name.Reference, error) {
	return name.NewTag(tenant.RegistryIP+":5000/"+uploadsRepo+":"+id, name.Insecure)
}

// pushUpload stores a validated upload in the registry.
func (s *Server) pushUpload(ctx context.Context, id string) error {
	if s.kube == nil {
		return nil // unit tests: local files only
	}
	b, err := os.ReadFile(s.uploadPath(id))
	if err != nil {
		return err
	}
	img, err := mutate.AppendLayers(empty.Image, static.NewLayer(b, types.OCILayer))
	if err != nil {
		return err
	}
	ref, err := uploadRef(id)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	if err := remote.Write(ref, img, remote.WithContext(ctx)); err != nil {
		return fmt.Errorf("store upload in the registry: %w", err)
	}
	return nil
}

// ensureLocal makes sure an upload is in the local cache, fetching it from the registry.
func (s *Server) ensureLocal(ctx context.Context, id string) bool {
	if !validUploadID(id) {
		return false
	}
	if _, err := os.Stat(s.uploadPath(id)); err == nil {
		return true
	}
	if s.kube == nil {
		return false
	}
	ref, err := uploadRef(id)
	if err != nil {
		return false
	}
	ctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	img, err := remote.Image(ref, remote.WithContext(ctx))
	if err != nil {
		return false
	}
	layers, err := img.Layers()
	if err != nil || len(layers) != 1 {
		return false
	}
	rc, err := layers[0].Compressed()
	if err != nil {
		return false
	}
	defer rc.Close()
	if err := os.MkdirAll(s.uploadDir(), 0o700); err != nil {
		return false
	}
	tmp := s.uploadPath(id) + ".fetch"
	f, err := os.OpenFile(tmp, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		return false
	}
	_, err = io.Copy(f, io.LimitReader(rc, maxUpload+1))
	f.Close()
	if err != nil || os.Rename(tmp, s.uploadPath(id)) != nil {
		os.Remove(tmp)
		return false
	}
	return true
}

func validUploadID(id string) bool {
	if len(id) != 11 || id[0] != 'u' {
		return false
	}
	for _, c := range id[1:] {
		if !(c >= 'a' && c <= 'z' || c >= '0' && c <= '9') {
			return false
		}
	}
	return true
}
