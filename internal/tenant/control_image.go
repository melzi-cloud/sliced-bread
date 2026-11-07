package tenant

import (
	"archive/tar"
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
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
	// Named for the in-cluster registry: servers that join later (replacements) pull it
	// from there; existing servers have it imported.
	ref := RegistryPullHost + "/sb-control:" + tag
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
			cctx, cancel := context.WithTimeout(ctx, 3*time.Minute)
			defer cancel()
			_, errs[i] = t.Run(cctx, "k3s ctr -n k8s.io images import -", bytes.NewReader(tarBytes))
		}(i)
	}
	wg.Wait()
	// A server that is down (being replaced) is skipped: it pulls the image from the
	// in-cluster registry when it joins. At least one server must have it.
	ok := 0
	for i, err := range errs {
		if err != nil {
			p.Logf("[%s] could not import on %s (skipped; it pulls from the registry when back): %v", st.RunID, st.Nodes[i].Name, err)
			continue
		}
		ok++
	}
	if ok == 0 {
		return fmt.Errorf("import failed on every server: %w", errs[0])
	}
	return nil
}

// DeployControl builds sb-control, imports it on all nodes and (re)applies the platform manifest.
func (p *Provisioner) DeployControl(ctx context.Context, st *State, repoRoot string) error {
	p.Logf("[%s] building sb-control image", st.RunID)
	ref, tb, err := BuildControlImage(ctx, repoRoot)
	if err != nil {
		return err
	}
	p.Logf("[%s] importing %s (%d MB) on %d nodes", st.RunID, ref, len(tb)>>20, len(st.Nodes))
	if err := p.ImportImage(ctx, st, tb); err != nil {
		return err
	}
	m, err := PlatformManifest(PlatformParams{Domain: st.Domain, ControlImage: ref})
	if err != nil {
		return err
	}
	if _, err := st.Kubectl(ctx, "apply -f -", []byte(m)); err != nil {
		return fmt.Errorf("apply platform: %w", err)
	}
	// Remove components older versions installed (the shared privileged BuildKit daemon).
	if _, err := st.Kubectl(ctx, "-n sb-builds delete deployment/buildkitd service/buildkitd networkpolicy/buildkitd-only-builds --ignore-not-found", nil); err != nil {
		return fmt.Errorf("remove old buildkitd: %w", err)
	}
	p.Logf("[%s] waiting for platform components", st.RunID)
	if err := poll(ctx, 10*time.Minute, 10*time.Second, func(ctx context.Context) (bool, error) {
		_, err := st.Kubectl(ctx, "-n sb-system rollout status deploy/sb-control --timeout=15s && "+
			"k3s kubectl -n sb-system rollout status deploy/sb-registry --timeout=15s && "+
			"k3s kubectl -n sb-system rollout status deploy/sb-guard --timeout=15s && "+
			"k3s kubectl -n cnpg-system rollout status deploy/cloudnative-pg --timeout=15s", nil)
		return err == nil, nil
	}); err != nil {
		return err
	}
	// Push the image into the in-cluster registry for servers that join later.
	push := strings.Replace(ref, RegistryPullHost, RegistryIP+":5000", 1)
	t0, err := st.Target(0)
	if err != nil {
		return err
	}
	return poll(ctx, 3*time.Minute, 10*time.Second, func(ctx context.Context) (bool, error) {
		_, err := t0.Run(ctx, "k3s ctr -n k8s.io images tag --force "+ref+" "+push+" >/dev/null && k3s ctr -n k8s.io images push --plain-http "+push+" >/dev/null", nil)
		if err != nil {
			p.Logf("[%s] pushing sb-control to the registry: %v (retrying)", st.RunID, err)
		}
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
	if _, err := st.Kubectl(ctx, "-n sb-system delete secret sb-bootstrap", nil); err != nil && !strings.Contains(err.Error(), "NotFound") {
		return err
	}
	return nil
}

// SetupCode installs a fresh one-time setup code and returns the setup link. The owner
// account is created by whoever opens the link first, so the code only ever goes to the
// terminal of the person running sb init. It expires after 24 hours and replaces any
// earlier code.
func (p *Provisioner) SetupCode(ctx context.Context, st *State) (string, error) {
	code := HumanCode(12)
	sum := sha256.Sum256([]byte(code))
	secret := fmt.Sprintf(`apiVersion: v1
kind: Secret
metadata: {name: sb-setup, namespace: sb-system}
type: Opaque
stringData:
  sha256: %q
  expires: %q
`, hex.EncodeToString(sum[:]), strconv.FormatInt(time.Now().Add(24*time.Hour).Unix(), 10))
	if _, err := st.Kubectl(ctx, "apply -f -", []byte(secret)); err != nil {
		return "", fmt.Errorf("install setup code: %w", err)
	}
	return "https://auth." + st.Domain + "/setup?code=" + code, nil
}

// HumanCode returns a random code in groups of 4 from an alphabet without look-alikes.
func HumanCode(n int) string {
	const alphabet = "ABCDEFGHJKMNPQRSTUVWXYZ23456789"
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	var out []byte
	for i, c := range b {
		if i > 0 && i%4 == 0 {
			out = append(out, '-')
		}
		out = append(out, alphabet[int(c)%len(alphabet)])
	}
	return string(out)
}

// BackupSettings is what sb init writes into the backup secret.
type BackupSettings struct {
	Password                                        string
	Endpoint, Bucket, Region, Prefix, KeyID, Secret string
}

// InstallBackupSecret writes the backup encryption password (and the bucket, if given)
// into the cluster over stdin. Configuring the bucket later keeps this password.
func (p *Provisioner) InstallBackupSecret(ctx context.Context, st *State, b BackupSettings) error {
	data := map[string]string{"password": b.Password, "hour": "3"}
	if b.Bucket != "" {
		region := b.Region
		if region == "" {
			region = "auto"
		}
		prefix := b.Prefix
		if prefix == "" {
			prefix = "slicedbread/" + st.Domain
		}
		data["endpoint"], data["bucket"], data["region"], data["prefix"] = b.Endpoint, b.Bucket, region, prefix
		data["access_key_id"], data["secret_access_key"] = b.KeyID, b.Secret
	}
	var lines []string
	for k, v := range data {
		lines = append(lines, fmt.Sprintf("  %s: %q", k, v))
	}
	sort.Strings(lines)
	secret := "apiVersion: v1\nkind: Secret\nmetadata: {name: sb-backup, namespace: sb-system, labels: {app.kubernetes.io/managed-by: slicedbread}}\ntype: Opaque\nstringData:\n" + strings.Join(lines, "\n") + "\n"
	if _, err := st.Kubectl(ctx, "apply -f -", []byte(secret)); err != nil {
		return fmt.Errorf("install backup settings: %w", err)
	}
	return nil
}
