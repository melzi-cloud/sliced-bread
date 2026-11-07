//go:build e2e

package hetzner

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/hetznercloud/hcloud-go/v2/hcloud"

	"github.com/melzi-cloud/sliced-bread/internal/config"
	"github.com/melzi-cloud/sliced-bread/internal/labels"
	"github.com/melzi-cloud/sliced-bread/internal/tenant"
)

// TestNodeLoss is acceptance test 1: on a 3-node install running Vaultwarden and Umami
// with data, hard power-off the server that runs Vaultwarden and measure how long until
// each app serves again and until a replacement server has joined, with no manual steps.
// Then check the data survived. Run with: make e2e-node-loss (about 35 minutes).
func TestNodeLoss(t *testing.T) {
	cfg, err := config.LoadDev()
	if err != nil {
		t.Fatalf("config (run through hack/secrets-env): %v", err)
	}
	root := repoRoot(t)
	sb := filepath.Join(root, "bin", "sb")
	if _, err := os.Stat(sb); err != nil {
		t.Fatal("bin/sb missing: run make build")
	}
	p := tenant.New(cfg, filepath.Join(root, tenant.StateRoot))
	p.Logf = func(f string, a ...any) { t.Logf(f, a...) }
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Minute)
	defer cancel()

	// 1. A full install, as `make prototype-up` makes it.
	cmd := exec.CommandContext(ctx, sb, "dev", "prototype", "up")
	cmd.Dir = root
	cmd.Env = append(os.Environ(), "SB_TTL=3h")
	var out bytes.Buffer
	cmd.Stdout, cmd.Stderr = io.MultiWriter(&out, testWriter{t}), testWriter{t}
	err = cmd.Run()
	m := regexp.MustCompile(`run id:\s+(r[a-z0-9]{7})`).FindStringSubmatch(out.String())
	if m == nil {
		m = regexp.MustCompile(`\[(r[a-z0-9]{7})\]`).FindStringSubmatch(out.String())
	}
	if m == nil {
		t.Fatalf("prototype up failed before a run id was known: %v", err)
	}
	runID := m[1]
	defer func() {
		if os.Getenv("SB_KEEP") == "1" {
			t.Logf("keeping %s (make tenant-down RUN=%s)", runID, runID)
			return
		}
		dctx, dcancel := context.WithTimeout(context.Background(), 20*time.Minute)
		defer dcancel()
		if err := p.Down(dctx, runID); err != nil {
			t.Errorf("teardown: %v", err)
		}
		if left, err := p.Inventory(dctx, labels.Selector(runID)); err != nil || len(left) > 0 {
			t.Errorf("resources remain after teardown: %v %v", left, err)
		}
	}()
	if err != nil {
		t.Fatalf("prototype up: %v", err)
	}
	st, err := tenant.LoadState(p.StateRoot, runID)
	if err != nil {
		t.Fatal(err)
	}
	admin := filepath.Join(st.Dir, "cli-admin.json")
	run := func(args ...string) string {
		c := exec.CommandContext(ctx, sb, args...)
		c.Env = append(os.Environ(), "SB_CONFIG="+admin)
		b, err := c.CombinedOutput()
		if err != nil {
			t.Fatalf("sb %s: %v\n%s", strings.Join(args, " "), err, b)
		}
		return string(b)
	}
	try := func(args ...string) error {
		c := exec.CommandContext(ctx, sb, args...)
		c.Env = append(os.Environ(), "SB_CONFIG="+admin)
		return c.Run()
	}

	// 2. Apps with data.
	run("deploy", "vault", "--template", "vaultwarden", "--set", "signups=true")
	run("deploy", "analytics", "--template", "umami")
	run("wait", "vault", "--timeout", "10m")
	run("wait", "analytics", "--timeout", "15m")
	vault := "https://vault." + st.Domain
	user := "nodeloss@example.com"
	if err := vwRegister(ctx, vault, user); err != nil {
		t.Fatalf("register vaultwarden user: %v", err)
	}
	run("action", "analytics", "create_website", "--input", "name=NodeLoss", "--input", "domain=nodeloss.example.com")

	// 3. Which server runs Vaultwarden, and what else is on it.
	pods, err := st.Kubectl(ctx, `get pods -A -o jsonpath='{range .items[*]}{.metadata.namespace}/{.metadata.name} {.spec.nodeName}{"\n"}{end}'`, nil)
	if err != nil {
		t.Fatal(err)
	}
	victim := ""
	for _, l := range strings.Split(string(pods), "\n") {
		if f := strings.Fields(l); len(f) == 2 && strings.HasPrefix(f[0], "app-vault/web-") {
			victim = f[1]
		}
	}
	if victim == "" {
		t.Fatalf("vaultwarden pod not found:\n%s", pods)
	}
	var onVictim []string
	for _, l := range strings.Split(string(pods), "\n") {
		if f := strings.Fields(l); len(f) == 2 && f[1] == victim && !strings.HasPrefix(f[0], "kube-system/") {
			onVictim = append(onVictim, f[0])
		}
	}
	t.Logf("powering off %s, which runs: %s", victim, strings.Join(onVictim, ", "))

	// 4. Hard power-off through the Hetzner API (like pulling the plug).
	srv, _, err := p.HC.Server.GetByName(ctx, victim)
	if err != nil || srv == nil {
		t.Fatalf("server %s: %v", victim, err)
	}
	t0 := time.Now()
	if _, _, err := p.HC.Server.Poweroff(ctx, srv); err != nil {
		t.Fatalf("power off: %v", err)
	}

	// 5. Measure.
	results := map[string]time.Duration{}
	checks := map[string]func() bool{
		// /alive, not a login: Vaultwarden rate-limits logins, and polling would trip it.
		"vaultwarden": func() bool { return httpOK(ctx, vault+"/alive") },
		"umami":       func() bool { return try("check", "analytics", "/api/heartbeat") == nil },
		"dashboard":   func() bool { return httpOK(ctx, "https://auth."+st.Domain+"/login") },
		"replacement": func() bool { return replacementReady(ctx, p.HC, st, victim, srv.ID) },
	}
	// Give the cluster a moment to notice before declaring anything recovered.
	time.Sleep(20 * time.Second)
	deadline := t0.Add(30 * time.Minute)
	for len(results) < len(checks) && time.Now().Before(deadline) {
		for name, f := range checks {
			if _, done := results[name]; !done && f() {
				results[name] = time.Since(t0).Round(time.Second)
				t.Logf("%s back after %s", name, results[name])
			}
		}
		time.Sleep(5 * time.Second)
	}
	for name := range checks {
		if _, ok := results[name]; !ok {
			t.Errorf("%s did not recover within 30 minutes", name)
		}
	}

	// 6. Data survived, and the CLI reaches the new server.
	if err := vwLogin(ctx, vault, user); err != nil {
		t.Errorf("vaultwarden user lost: %v", err)
	}
	if s := run("action", "analytics", "stats"); !strings.Contains(s, "NodeLoss") {
		t.Errorf("umami website lost:\n%s", s)
	}
	if err := st.RefreshNodes(ctx); err != nil {
		t.Errorf("refresh nodes: %v", err)
	}
	for i := range st.Nodes {
		tg, err := st.Target(i)
		if err == nil {
			_, err = tg.Run(ctx, "true", nil)
		}
		if err != nil {
			t.Errorf("ssh to %s with the pinned host key: %v", st.Nodes[i].Name, err)
		}
	}
	report := map[string]any{"run": runID, "victim": victim, "onVictim": onVictim, "poweredOffAt": t0.UTC().Format(time.RFC3339)}
	for k, v := range results {
		report[k+"Seconds"] = int(v.Seconds())
	}
	b, _ := json.MarshalIndent(report, "", "  ")
	dir := filepath.Join(root, "test", "artifacts", runID)
	os.MkdirAll(dir, 0o700)
	os.WriteFile(filepath.Join(dir, "node-loss.json"), b, 0o600)
	for name, args := range map[string]string{
		"events.txt":     "get events -A --sort-by=.lastTimestamp",
		"guard.log":      "-n sb-system logs deploy/sb-guard",
		"cnpg.log":       "-n cnpg-system logs deploy/cloudnative-pg --tail 400",
		"pods-after.txt": "get pods -A -o wide",
	} {
		if out, err := st.Kubectl(ctx, args, nil); err == nil {
			os.WriteFile(filepath.Join(dir, name), out, 0o600)
		}
	}
	t.Logf("node-loss result:\n%s", b)
}

type testWriter struct{ t *testing.T }

func (w testWriter) Write(p []byte) (int, error) {
	for _, l := range strings.Split(strings.TrimRight(string(p), "\n"), "\n") {
		w.t.Log(l)
	}
	return len(p), nil
}

var httpc = &http.Client{Timeout: 10 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}

func httpOK(ctx context.Context, u string) bool {
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	resp, err := httpc.Do(req)
	if err != nil {
		return false
	}
	resp.Body.Close()
	return resp.StatusCode == 200
}

func replacementReady(ctx context.Context, hc *hcloud.Client, st *tenant.State, name string, oldID int64) bool {
	srv, _, err := hc.Server.GetByName(ctx, name)
	if err != nil || srv == nil || srv.ID == oldID || srv.Status != hcloud.ServerStatusRunning {
		return false
	}
	out, err := st.Kubectl(ctx, "get nodes --no-headers", nil)
	if err != nil {
		// The first server in local state may be the dead one.
		st.RefreshNodes(ctx)
		return false
	}
	ready := 0
	for _, l := range strings.Split(string(out), "\n") {
		if f := strings.Fields(l); len(f) > 1 && f[1] == "Ready" {
			ready++
		}
	}
	return ready == 3 && strings.Contains(string(out), name)
}

// Vaultwarden accounts without a mail server: the verification token comes back directly.
func vwHash(email string) string { return base64.StdEncoding.EncodeToString([]byte("hash-" + email)) }

func vwRegister(ctx context.Context, base, email string) error {
	post := func(path string, v any) (string, error) {
		b, _ := json.Marshal(v)
		req, _ := http.NewRequestWithContext(ctx, http.MethodPost, base+path, bytes.NewReader(b))
		req.Header.Set("Content-Type", "application/json")
		resp, err := httpc.Do(req)
		if err != nil {
			return "", err
		}
		defer resp.Body.Close()
		body, _ := io.ReadAll(resp.Body)
		if resp.StatusCode != 200 {
			return "", fmt.Errorf("%s: HTTP %d %s", path, resp.StatusCode, body)
		}
		return strings.Trim(strings.TrimSpace(string(body)), `"`), nil
	}
	tok, err := post("/identity/accounts/register/send-verification-email", map[string]string{"email": email, "name": "x"})
	if err != nil {
		return err
	}
	r := func(n int) string { b := make([]byte, n); rand.Read(b); return base64.StdEncoding.EncodeToString(b) }
	_, err = post("/identity/accounts/register/finish", map[string]any{
		"email": email, "kdf": 0, "kdfIterations": 600000, "masterPasswordHash": vwHash(email),
		"key": "2." + r(16) + "|" + r(32) + "|" + r(32), "emailVerificationToken": tok,
		"keys": map[string]string{"publicKey": "pk", "encryptedPrivateKey": "2.a|b|c"},
	})
	return err
}

func vwLogin(ctx context.Context, base, email string) error {
	form := url.Values{"grant_type": {"password"}, "username": {email}, "password": {vwHash(email)}, "scope": {"api offline_access"},
		"client_id": {"web"}, "deviceType": {"9"}, "deviceIdentifier": {"5b6a1f3e-0c55-4c7e-9a43-0d1c2b3a4d5e"}, "deviceName": {"chrome"}}
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, base+"/identity/connect/token", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := httpc.Do(req)
	if err != nil {
		return err
	}
	resp.Body.Close()
	if resp.StatusCode != 200 {
		return fmt.Errorf("login HTTP %d", resp.StatusCode)
	}
	return nil
}
