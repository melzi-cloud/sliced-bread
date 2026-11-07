//go:build e2e

// Package hetzner holds L2 tests that create real resources on Hetzner Cloud.
// Run with: make e2e-hetzner
package hetzner

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/melzi-cloud/sliced-bread/internal/config"
	"github.com/melzi-cloud/sliced-bread/hack/smoke"
	"github.com/melzi-cloud/sliced-bread/internal/tenant"
)

func repoRoot(t *testing.T) string {
	t.Helper()
	dir, _ := os.Getwd()
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatal("go.mod not found")
		}
		dir = parent
	}
}

// TestSmoke creates a 3-node tenant, serves whoami over TLS on
// whoami.<run-id>.<test domain>, checks the CSI driver and firewall,
// then destroys everything and verifies nothing labelled remains.
func TestSmoke(t *testing.T) {
	cfg, err := config.LoadDev()
	if err != nil {
		t.Fatalf("config (run through hack/secrets-env): %v", err)
	}
	root := repoRoot(t)
	p := tenant.New(cfg, filepath.Join(root, tenant.StateRoot))
	p.Logf = func(f string, a ...any) { t.Logf(f, a...) }

	ctx, cancel := context.WithTimeout(context.Background(), 55*time.Minute)
	defer cancel()
	res, err := smoke.Run(ctx, p, smoke.Options{
		ArtifactsRoot: filepath.Join(root, "test", "artifacts"),
		KeepOnFailure: os.Getenv("SB_KEEP_ON_FAILURE") == "1",
	})
	t.Logf("run %s took %s", res.RunID, res.Duration.Round(time.Second))
	if err != nil {
		t.Fatalf("smoke failed (artifacts in test/artifacts/%s): %v", res.RunID, err)
	}
}
