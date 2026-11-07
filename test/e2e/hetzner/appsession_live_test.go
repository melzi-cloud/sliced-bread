//go:build e2e

package hetzner

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// TestAppSessionLive checks the browser sign-in helper against a running prototype:
// SB_RUN_DIR=.sb/runs/<id> SB_APP_URL=https://<app>.<domain> go test -tags e2e -run TestAppSessionLive ./test/e2e/hetzner
func TestAppSessionLive(t *testing.T) {
	dir, app := os.Getenv("SB_RUN_DIR"), os.Getenv("SB_APP_URL")
	if dir == "" || app == "" {
		t.Skip("set SB_RUN_DIR and SB_APP_URL")
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	c, err := appSession(ctx, filepath.Join(dir, "admin-login.json"), app)
	if err != nil {
		t.Fatal(err)
	}
	if err := vwRegister(ctx, c, app, "helper-"+time.Now().Format("150405")+"@example.com"); err != nil {
		t.Fatal(err)
	}
}
