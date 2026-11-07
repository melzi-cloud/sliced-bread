package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"golang.org/x/term"

	"github.com/melzi-cloud/sliced-bread/internal/config"
	"github.com/melzi-cloud/sliced-bread/internal/controlbin"
	"github.com/melzi-cloud/sliced-bread/internal/labels"
	"github.com/melzi-cloud/sliced-bread/internal/tenant"
)

var domainRE = regexp.MustCompile(`^([a-z0-9]([a-z0-9-]*[a-z0-9])?\.)+[a-z]{2,}$`)

// installsRoot holds per-install state (SSH key, state.json, credentials) on the operator's machine.
func installsRoot() string {
	if d := os.Getenv("SB_STATE_DIR"); d != "" {
		return d
	}
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".config", "slicedbread", "installs")
}

// secretInput reads a secret from the first set env var, or prompts without echo on a terminal.
func secretInput(prompt string, envs ...string) (string, error) {
	for _, e := range envs {
		if v := strings.TrimSpace(os.Getenv(e)); v != "" {
			return v, nil
		}
	}
	if !term.IsTerminal(int(os.Stdin.Fd())) {
		return "", &exitErr{exitUsage, fmt.Sprintf("%s: set %s (no terminal to prompt on)", prompt, envs[0])}
	}
	fmt.Fprintf(os.Stderr, "%s: ", prompt)
	b, err := term.ReadPassword(int(os.Stdin.Fd()))
	fmt.Fprintln(os.Stderr)
	return strings.TrimSpace(string(b)), err
}

func confirm(question string) bool {
	if !term.IsTerminal(int(os.Stdin.Fd())) {
		return false
	}
	fmt.Fprintf(os.Stderr, "%s [y/N]: ", question)
	line, _ := bufio.NewReader(os.Stdin).ReadString('\n')
	return strings.EqualFold(strings.TrimSpace(line), "y") || strings.EqualFold(strings.TrimSpace(line), "yes")
}

func baseConfig(hc, cf string) config.Dev {
	owner := os.Getenv("USER")
	if owner == "" {
		owner = "sb"
	}
	return config.Dev{
		HcloudToken: hc, CloudflareToken: cf,
		Location: config.DefaultLocation, ServerType: config.DefaultServerType, Image: config.DefaultImage,
		LBType: config.DefaultLBType, K3sVersion: config.DefaultK3sVersion,
		CertManagerVersion: config.DefaultCertManagerVersion, HcloudCSIVersion: config.DefaultHcloudCSIVersion,
		ACMEServer: config.LetsEncryptProduction, Owner: strings.ToLower(regexp.MustCompile(`[^a-zA-Z0-9._-]`).ReplaceAllString(owner, "-")),
		TTL: config.DefaultTTL, NoExpiry: true,
	}
}

const initUsage = `sb init --domain <base domain> --email <admin email> [--size 1|3] [--location nbg1] [--server-type cx23]
        [--fallback-issuer zerossl|google] [--yes]

Creates a SlicedBread install in your own Hetzner account. Apps are served at https://<app>.<base domain>.
Secrets (read from the environment, or prompted for on a terminal):
  HCLOUD_TOKEN           Hetzner Cloud API token (read and write). Use a dedicated Hetzner project.
  CLOUDFLARE_API_TOKEN   Cloudflare token with Zone / DNS / Edit on the zone that contains the base domain.
  SB_EAB_KID, SB_EAB_HMAC  external account binding for --fallback-issuer (optional).
Sizes: 1 node (about EUR 4/month, no high availability) or 3 nodes plus a load balancer (about EUR 32/month).`

func runInit(args []string) error {
	fs := flag.NewFlagSet("init", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	domain := fs.String("domain", "", "")
	email := fs.String("email", "", "")
	size := fs.Int("size", 3, "")
	location := fs.String("location", config.DefaultLocation, "")
	serverType := fs.String("server-type", config.DefaultServerType, "")
	fallback := fs.String("fallback-issuer", "", "")
	yes := fs.Bool("yes", false, "")
	ttl := fs.Duration("expire-after", 0, "") // test installs only: label resources to expire
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			fmt.Println(initUsage)
			return nil
		}
		return &exitErr{exitUsage, err.Error() + "\n\n" + initUsage}
	}
	*domain = strings.TrimSuffix(strings.ToLower(strings.TrimSpace(*domain)), ".")
	if !domainRE.MatchString(*domain) || !strings.Contains(*email, "@") {
		return &exitErr{exitUsage, initUsage}
	}
	if *size != 1 && *size != 3 {
		return &exitErr{exitUsage, "--size must be 1 or 3"}
	}
	hc, err := secretInput("Hetzner Cloud API token", "HCLOUD_TOKEN")
	if err != nil {
		return err
	}
	cf, err := secretInput("Cloudflare API token", "CLOUDFLARE_API_TOKEN", "CF_API_TOKEN")
	if err != nil {
		return err
	}
	cfg := baseConfig(hc, cf)
	cfg.Domain, cfg.Nodes, cfg.Location, cfg.ServerType = *domain, *size, *location, *serverType
	if *ttl > 0 {
		cfg.NoExpiry, cfg.TTL = false, *ttl
	}
	if *fallback != "" {
		server, ok := config.FallbackServers[*fallback]
		if !ok {
			return &exitErr{exitUsage, "--fallback-issuer must be zerossl or google"}
		}
		kid, kerr := secretInput("EAB key id", "SB_EAB_KID")
		hmac, herr := secretInput("EAB HMAC key", "SB_EAB_HMAC")
		if kerr != nil || herr != nil {
			return errors.Join(kerr, herr)
		}
		cfg.Fallback = &config.EAB{Server: server, KeyID: kid, HMAC: hmac}
	}

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Minute)
	defer cancel()
	p := tenant.New(cfg, installsRoot())
	p.Logf = func(f string, a ...any) { fmt.Fprintf(os.Stderr, f+"\n", a...) }

	fmt.Fprintln(os.Stderr, "checking tokens")
	if _, _, err := p.HC.Location.GetByName(ctx, *location); err != nil {
		return &exitErr{exitError, "Hetzner token check failed: " + err.Error()}
	}
	zone, err := p.DNS.ZoneFor(ctx, *domain)
	if err != nil {
		return &exitErr{exitError, "Cloudflare: " + err.Error()}
	}
	p.Cfg.DNSZone = zone.Name
	existing, err := p.DNS.ListUnder(ctx, zone.ID, *domain)
	if err != nil {
		return &exitErr{exitError, "Cloudflare token check failed: " + err.Error()}
	}
	for _, r := range existing {
		if r.Name == "*."+*domain || r.Name == "api."+*domain || r.Name == "auth."+*domain {
			return &exitErr{exitError, fmt.Sprintf("DNS record %s already exists; use another base domain or remove it", r.Name)}
		}
	}
	cost := "about EUR 32/month (3 x " + *serverType + " + lb11)"
	if *size == 1 {
		cost = "about EUR 4/month (1 x " + *serverType + ", no high availability)"
	}
	fmt.Fprintf(os.Stderr, "\ninstall: %d node(s) in %s, apps at https://<app>.%s (Cloudflare zone %s), %s\n", *size, *location, *domain, zone.Name, cost)
	if !*yes && !confirm("create it?") {
		return &exitErr{exitError, "aborted (pass --yes to skip this question)"}
	}

	runID := labels.NewRunID()
	root, _ := repoRoot()
	if _, ok := controlbin.Binary(); ok {
		root = "" // prefer the embedded sb-control
	}
	st, err := p.Up(ctx, runID)
	if err == nil {
		err = p.InstallAddons(ctx, st)
	}
	if err == nil {
		err = p.DeployControl(ctx, st, root, *email)
	}
	if err == nil {
		err = p.FetchCredentials(ctx, st)
	}
	if err != nil {
		fmt.Fprintf(os.Stderr, "\ninstall %s failed. Everything it created is labelled sb/run-id=%s; remove it with: sb destroy %s\n", runID, runID, runID)
		return err
	}
	cfgPath := configPath()
	b, _ := os.ReadFile(filepath.Join(st.Dir, "cli-admin.json"))
	if _, statErr := os.Stat(cfgPath); statErr == nil && os.Getenv("SB_CONFIG") == "" {
		cfgPath = filepath.Join(st.Dir, "cli-admin.json")
		fmt.Fprintf(os.Stderr, "note: %s already exists, so it was left alone. Use: export SB_CONFIG=%s\n", configPath(), cfgPath)
	} else {
		os.MkdirAll(filepath.Dir(cfgPath), 0o700)
		if err := os.WriteFile(cfgPath, b, 0o600); err != nil {
			return err
		}
	}
	fmt.Printf(`
SlicedBread is ready.
  install id:  %s
  apps:        https://<app>.%s
  dashboard:   https://auth.%s   (email and password in %s)
  this CLI:    logged in with an admin token (%s)
  your agent:  run "sb login --api https://api.%s" in the agent's environment; you approve it in the browser
  remove:      sb destroy %s
`, st.RunID, st.Domain, st.Domain, filepath.Join(st.Dir, "admin-login.json"), cfgPath, st.Domain, st.RunID)
	return nil
}

func loadInstall(id string) (*tenant.State, error) {
	if st, err := tenant.LoadState(installsRoot(), id); err == nil {
		return st, nil
	}
	ents, _ := os.ReadDir(installsRoot())
	for _, e := range ents {
		if st, err := tenant.LoadState(installsRoot(), e.Name()); err == nil && st.Domain == id {
			return st, nil
		}
	}
	return nil, &exitErr{exitError, "no local install " + id + " (see: sb installs)"}
}

func runInstalls() error {
	ents, _ := os.ReadDir(installsRoot())
	var rows []string
	for _, e := range ents {
		st, err := tenant.LoadState(installsRoot(), e.Name())
		if err != nil {
			continue
		}
		rows = append(rows, fmt.Sprintf("%s  %d node(s)  https://<app>.%s", st.RunID, st.Size, st.Domain))
	}
	sort.Strings(rows)
	if len(rows) == 0 {
		fmt.Println("no installs on this machine (create one with: sb init)")
	}
	for _, r := range rows {
		fmt.Println(r)
	}
	return nil
}

func runDestroy(args []string) error {
	fs := flag.NewFlagSet("destroy", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	yes := fs.Bool("yes", false, "")
	pos, err := parse(fs, args)
	if err != nil {
		return err
	}
	if len(pos) != 1 {
		return &exitErr{exitUsage, "usage: sb destroy <install-id|domain> [--yes]\nDeletes the servers, load balancer, volumes (all app data) and DNS records of an install."}
	}
	st, err := loadInstall(pos[0])
	if err != nil {
		return err
	}
	hc, err := secretInput("Hetzner Cloud API token", "HCLOUD_TOKEN")
	if err != nil {
		return err
	}
	cf, err := secretInput("Cloudflare API token", "CLOUDFLARE_API_TOKEN", "CF_API_TOKEN")
	if err != nil {
		return err
	}
	cfg := baseConfig(hc, cf)
	cfg.Domain, cfg.DNSZone = st.Domain, st.DNSZone
	p := tenant.New(cfg, installsRoot())
	p.Logf = func(f string, a ...any) { fmt.Fprintf(os.Stderr, f+"\n", a...) }
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Minute)
	defer cancel()
	res, err := p.Inventory(ctx, labels.Selector(st.RunID))
	if err != nil {
		return err
	}
	fmt.Fprintf(os.Stderr, "this deletes install %s (%s) and ALL its app data:\n", st.RunID, st.Domain)
	for _, r := range res {
		fmt.Fprintf(os.Stderr, "  %s\n", r)
	}
	if !*yes && !confirm("type y to delete everything") {
		return &exitErr{exitError, "aborted"}
	}
	if err := p.Down(ctx, st.RunID); err != nil {
		return err
	}
	if left, err := p.Inventory(ctx, labels.Selector(st.RunID)); err == nil && len(left) > 0 {
		return &exitErr{exitError, fmt.Sprintf("some resources remain: %v", left)}
	}
	_ = os.RemoveAll(st.Dir)
	fmt.Println("destroyed", st.RunID)
	return nil
}

// runUpgrade rolls out the sb-control embedded in this sb binary to an install.
func runUpgrade(args []string) error {
	if len(args) != 1 {
		return &exitErr{exitUsage, "usage: sb upgrade <install-id|domain>\nRolls out the control plane built into this sb binary. Your apps keep running."}
	}
	st, err := loadInstall(args[0])
	if err != nil {
		return err
	}
	root, _ := repoRoot()
	if _, ok := controlbin.Binary(); ok {
		root = ""
	}
	p := tenant.New(baseConfig("", ""), installsRoot())
	p.Logf = func(f string, a ...any) { fmt.Fprintf(os.Stderr, f+"\n", a...) }
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Minute)
	defer cancel()
	email := "admin@" + st.Domain
	if b, err := os.ReadFile(filepath.Join(st.Dir, "admin-login.json")); err == nil {
		var l struct{ Email string }
		if json.Unmarshal(b, &l) == nil && l.Email != "" {
			email = l.Email
		}
	}
	if err := p.DeployControl(ctx, st, root, email); err != nil {
		return err
	}
	if _, err := st.Kubectl(ctx, "-n sb-system rollout status deploy/sb-control --timeout=180s", nil); err != nil {
		return err
	}
	fmt.Println("upgraded", st.RunID)
	return nil
}
