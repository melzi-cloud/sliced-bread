package tenant

import (
	"archive/tar"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/google/go-containerregistry/pkg/name"
	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/mutate"
	"github.com/google/go-containerregistry/pkg/v1/remote"
	"github.com/google/go-containerregistry/pkg/v1/tarball"

	"github.com/melzi-cloud/sliced-bread/internal/controlbin"
)

const controlBase = "gcr.io/distroless/static-debian12:nonroot"

// ControlBinary returns the linux/amd64 sb-control binary: the one embedded in this
// sb build, or a fresh cross-compile when running from the repository.
func ControlBinary(ctx context.Context, repoRoot string) ([]byte, error) {
	if repoRoot == "" {
		if b, ok := controlbin.Binary(); ok {
			return b, nil
		}
		return nil, fmt.Errorf("this sb build has no embedded sb-control; build it with `make build` or run from the repository")
	}
	out := filepath.Join(repoRoot, ".sb", "build", "sb-control")
	if err := os.MkdirAll(filepath.Dir(out), 0o700); err != nil {
		return nil, err
	}
	cmd := exec.CommandContext(ctx, "go", "build", "-trimpath", "-ldflags=-s -w", "-o", out, "./cmd/sb-control")
	cmd.Dir = repoRoot
	cmd.Env = append(os.Environ(), "CGO_ENABLED=0", "GOOS=linux", "GOARCH=amd64")
	if b, err := cmd.CombinedOutput(); err != nil {
		return nil, fmt.Errorf("go build sb-control: %v\n%s", err, b)
	}
	return os.ReadFile(out)
}

// BuildControlImage packs the sb-control binary into a docker-format image tarball.
// repoRoot "" uses the binary embedded in this sb build.
func BuildControlImage(ctx context.Context, repoRoot string) (string, []byte, error) {
	bin, err := ControlBinary(ctx, repoRoot)
	if err != nil {
		return "", nil, err
	}
	sum := sha256.Sum256(bin)
	tag := hex.EncodeToString(sum[:6])

	var layerBuf bytes.Buffer
	tw := tar.NewWriter(&layerBuf)
	for _, d := range []string{"usr/", "usr/local/", "usr/local/bin/"} {
		tw.WriteHeader(&tar.Header{Name: d, Typeflag: tar.TypeDir, Mode: 0o755, ModTime: time.Unix(0, 0)})
	}
	tw.WriteHeader(&tar.Header{Name: "usr/local/bin/sb-control", Mode: 0o755, Size: int64(len(bin)), ModTime: time.Unix(0, 0)})
	tw.Write(bin)
	tw.Close()
	layerBytes := layerBuf.Bytes()
	layer, err := tarball.LayerFromOpener(func() (io.ReadCloser, error) { return io.NopCloser(bytes.NewReader(layerBytes)), nil })
	if err != nil {
		return "", nil, err
	}
	baseRef, err := name.ParseReference(controlBase)
	if err != nil {
		return "", nil, err
	}
	base, err := remote.Image(baseRef, remote.WithContext(ctx), remote.WithPlatform(v1.Platform{OS: "linux", Architecture: "amd64"}))
	if err != nil {
		return "", nil, fmt.Errorf("pull base image: %w", err)
	}
	img, err := mutate.AppendLayers(base, layer)
	if err != nil {
		return "", nil, err
	}
	cf, err := img.ConfigFile()
	if err != nil {
		return "", nil, err
	}
	cfg := cf.Config.DeepCopy()
	cfg.Entrypoint = []string{"/usr/local/bin/sb-control"}
	cfg.Cmd = nil
	cfg.User = "65532:65532"
	if img, err = mutate.Config(img, *cfg); err != nil {
		return "", nil, err
	}
	ref := "sb.local/sb-control:" + tag
	tagRef, err := name.NewTag(ref)
	if err != nil {
		return "", nil, err
	}
	var tb bytes.Buffer
	if err := tarball.Write(tagRef, img, &tb); err != nil {
		return "", nil, err
	}
	return ref, tb.Bytes(), nil
}

// ImportImage loads an image tarball into containerd on every node.
func (p *Provisioner) ImportImage(ctx context.Context, st *State, tarBytes []byte) error {
	var wg sync.WaitGroup
	errs := make([]error, len(st.Nodes))
	for i := range st.Nodes {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			t, err := st.Target(i)
			if err != nil {
				errs[i] = err
				return
			}
			cctx, cancel := context.WithTimeout(ctx, 5*time.Minute)
			defer cancel()
			_, errs[i] = t.Run(cctx, "k3s ctr -n k8s.io images import -", bytes.NewReader(tarBytes))
		}(i)
	}
	wg.Wait()
	for i, err := range errs {
		if err != nil {
			return fmt.Errorf("import on %s: %w", st.Nodes[i].Name, err)
		}
	}
	return nil
}

// DeployControl builds sb-control, imports it on all nodes and (re)applies the platform manifest.
func (p *Provisioner) DeployControl(ctx context.Context, st *State, repoRoot, adminEmail string) error {
	p.Logf("[%s] building sb-control image", st.RunID)
	ref, tb, err := BuildControlImage(ctx, repoRoot)
	if err != nil {
		return err
	}
	p.Logf("[%s] importing %s (%d MB) on %d nodes", st.RunID, ref, len(tb)>>20, len(st.Nodes))
	if err := p.ImportImage(ctx, st, tb); err != nil {
		return err
	}
	m, err := PlatformManifest(PlatformParams{Domain: st.Domain, AdminEmail: adminEmail, ControlImage: ref})
	if err != nil {
		return err
	}
	if _, err := st.Kubectl(ctx, "apply -f -", []byte(m)); err != nil {
		return fmt.Errorf("apply platform: %w", err)
	}
	p.Logf("[%s] waiting for platform components", st.RunID)
	return poll(ctx, 10*time.Minute, 10*time.Second, func(ctx context.Context) (bool, error) {
		_, err := st.Kubectl(ctx, "-n sb-system rollout status deploy/sb-control --timeout=15s && "+
			"k3s kubectl -n sb-system rollout status deploy/sb-registry --timeout=15s && "+
			"k3s kubectl -n sb-builds rollout status deploy/buildkitd --timeout=15s && "+
			"k3s kubectl -n cnpg-system rollout status deploy/cloudnative-pg --timeout=15s", nil)
		return err == nil, nil
	})
}

// CLIConfig is what the user CLI reads.
type CLIConfig struct {
	API   string `json:"api"`
	Token string `json:"token"`
}

// FetchCredentials reads the one-time bootstrap secret, writes CLI configs and
// the admin login to the run directory (0600), and deletes the secret from the cluster.
func (p *Provisioner) FetchCredentials(ctx context.Context, st *State) error {
	var raw []byte
	err := poll(ctx, 3*time.Minute, 5*time.Second, func(ctx context.Context) (bool, error) {
		b, err := st.Kubectl(ctx, "-n sb-system get secret sb-bootstrap -o jsonpath={.data}", nil)
		if err != nil {
			return false, nil
		}
		raw = b
		return true, nil
	})
	if err != nil {
		if _, statErr := os.Stat(filepath.Join(st.Dir, "cli-agent.json")); statErr == nil {
			return nil // already fetched earlier
		}
		return fmt.Errorf("bootstrap credentials not found: %w", err)
	}
	var enc map[string]string
	if err := json.Unmarshal(raw, &enc); err != nil {
		return fmt.Errorf("parse bootstrap secret: %w", err)
	}
	dec := map[string]string{}
	for k, v := range enc {
		b, err := base64.StdEncoding.DecodeString(v)
		if err != nil {
			return err
		}
		dec[k] = string(b)
	}
	api := "https://api." + st.Domain
	write := func(name string, v any) error {
		b, _ := json.MarshalIndent(v, "", "  ")
		return os.WriteFile(filepath.Join(st.Dir, name), b, 0o600)
	}
	if err := write("cli-agent.json", CLIConfig{API: api, Token: dec["agent-token"]}); err != nil {
		return err
	}
	if err := write("cli-admin.json", CLIConfig{API: api, Token: dec["admin-token"]}); err != nil {
		return err
	}
	if err := write("admin-login.json", map[string]string{"url": "https://auth." + st.Domain, "email": dec["admin-email"], "password": dec["admin-password"]}); err != nil {
		return err
	}
	if _, err := st.Kubectl(ctx, "-n sb-system delete secret sb-bootstrap", nil); err != nil && !strings.Contains(err.Error(), "NotFound") {
		return err
	}
	return nil
}
