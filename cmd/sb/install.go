package main

import (
	"bufio"
	"context"
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

	"github.com/melzi-cloud/sliced-bread/internal/acme"
	"github.com/melzi-cloud/sliced-bread/internal/apps"
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

const initUsage = `sb init --domain <base domain> [--size 1|3] [--location nbg1] [--server-type cx23]
        [--email you@example.com --fallback-issuer zerossl] [--yes]
        [--backup-endpoint https://... --backup-bucket B [--backup-region auto] [--backup-prefix path]]

Creates a SlicedBread install in your own Hetzner account. Apps are served at https://<app>.<base domain>.
At the end it prints a one-time setup link (you create your owner account in the browser) and the
backup encryption password (save it in your password manager). Approvals, members, backup settings
and other admin work happen in that dashboard; the CLI is left logged in with an agent token.
Secrets (read from the environment, or prompted for on a terminal):
  HCLOUD_TOKEN           Hetzner Cloud API token (read and write). Use a dedicated Hetzner project.
  CLOUDFLARE_API_TOKEN   Cloudflare token with Zone / DNS / Edit on the zone that contains the base domain
                         (CF_API_TOKEN is also accepted).
  SB_EAB_KID, SB_EAB_HMAC  ZeroSSL external account binding for --fallback-issuer (optional: without them
                         sb init requests one from ZeroSSL with the --email address).
  SB_BACKUP_PASSWORD     reuse the encryption password of an earlier install (to restore its apps from the
                         same bucket and prefix after setup; otherwise a new password is generated).
  SB_BACKUP_ACCESS_KEY_ID, SB_BACKUP_SECRET_ACCESS_KEY  S3 keys for --backup-bucket (recommended: a bucket
                         in another location, e.g. Cloudflare R2 or Hetzner Object Storage).
Test installs: --expire-after 6h labels every resource to be deleted by the SlicedBread reaper after that time.
Sizes (Hetzner prices incl. VAT, nbg1, cx23): 1 node about EUR 7/month (no high availability); 3 nodes plus a
load balancer about EUR 29/month. Each app volume and database adds about EUR 0.77/month (10 GB volume).`

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
	bEndpoint := fs.String("backup-endpoint", "", "")
	bBucket := fs.String("backup-bucket", "", "")
	bRegion := fs.String("backup-region", "auto", "")
	bPrefix := fs.String("backup-prefix", "", "")
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			fmt.Println(initUsage)
			return nil
		}
		return &exitErr{exitUsage, err.Error() + "\n\n" + initUsage}
	}
	*domain = strings.TrimSuffix(strings.ToLower(strings.TrimSpace(*domain)), ".")
	if !domainRE.MatchString(*domain) || (*email != "" && !strings.Contains(*email, "@")) {
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
			return &exitErr{exitUsage, "--fallback-issuer must be zerossl"}
		}
		if *email == "" {
			return &exitErr{exitUsage, "--fallback-issuer zerossl needs --email (ZeroSSL registers an account with it). Without it, the dashboard sets up ZeroSSL with your owner account's email after setup."}
		}
		kid, hmac := strings.TrimSpace(os.Getenv("SB_EAB_KID")), strings.TrimSpace(os.Getenv("SB_EAB_HMAC"))
		if kid == "" || hmac == "" {
			var err error
			fmt.Fprintln(os.Stderr, "requesting ZeroSSL ACME credentials for", *email)
			if kid, hmac, err = acme.ZeroSSLEAB(context.Background(), *email); err != nil {
				return &exitErr{exitError, "ZeroSSL: " + err.Error() + " (or set SB_EAB_KID and SB_EAB_HMAC)"}
			}
		}
		cfg.Fallback = &config.EAB{Server: server, KeyID: kid, HMAC: hmac}
	}

	var bKey, bSecret string
	if (*bEndpoint == "") != (*bBucket == "") {
		return &exitErr{exitUsage, "--backup-endpoint and --backup-bucket go together"}
	}
	if *bBucket != "" {
		if bKey, err = secretInput("S3 access key id", "SB_BACKUP_ACCESS_KEY_ID"); err != nil {
			return err
		}
		if bSecret, err = secretInput("S3 secret access key", "SB_BACKUP_SECRET_ACCESS_KEY"); err != nil {
			return err
		}
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
		h := apps.Hosts{Domain: *domain}
		if r.Name == "*."+*domain || r.Name == h.API() || r.Name == h.Auth() {
			return &exitErr{exitError, fmt.Sprintf("DNS record %s already exists; use another base domain or remove it", r.Name)}
		}
	}
	cost := "about EUR 29/month with cx23 (3 servers + load balancer), plus EUR 0.77/month per app volume or database"
	if *size == 1 {
		cost = "about EUR 7/month with cx23 (1 server, no high availability), plus EUR 0.77/month per app volume or database"
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
		err = p.DeployControl(ctx, st, root)
	}
	if err == nil {
		err = p.FetchCredentials(ctx, st)
	}
	// The backup encryption password exists from the start, so configuring a bucket
	// later (in the dashboard) never creates a second one to save.
	backupPassword := strings.TrimSpace(os.Getenv("SB_BACKUP_PASSWORD"))
	if backupPassword == "" {
		backupPassword = tenant.HumanCode(32)
	}
	if err == nil {
		err = p.InstallBackupSecret(ctx, st, tenant.BackupSettings{Password: backupPassword,
			Endpoint: *bEndpoint, Bucket: *bBucket, Region: *bRegion, Prefix: *bPrefix, KeyID: bKey, Secret: bSecret})
	}
	var setupLink string
	if err == nil {
		setupLink, err = p.SetupCode(ctx, st)
	}
	if err != nil {
		fmt.Fprintf(os.Stderr, "\ninstall %s failed. Everything it created is labelled sb/run-id=%s; remove it with: sb destroy %s\n", runID, runID, runID)
		if strings.Contains(err.Error(), "certificate not issued") && cfg.Fallback == nil {
			fmt.Fprintln(os.Stderr, "Let's Encrypt did not issue the certificate. To use ZeroSSL instead, destroy this install and rerun sb init with: --email you@example.com --fallback-issuer zerossl")
		}
		return err
	}
	cfgPath := configPath()
	agentCfg := filepath.Join(st.Dir, "cli-agent.json")
	b, _ := os.ReadFile(agentCfg)
	if _, statErr := os.Stat(cfgPath); statErr == nil && os.Getenv("SB_CONFIG") == "" {
		cfgPath = agentCfg
		fmt.Fprintf(os.Stderr, "note: %s already exists, so it was left alone. Use: export SB_CONFIG=%s\n", configPath(), cfgPath)
	} else {
		os.MkdirAll(filepath.Dir(cfgPath), 0o700)
		if err := os.WriteFile(cfgPath, b, 0o600); err != nil {
			return err
		}
	}
	backupLine := "no bucket yet: set one up in the dashboard (Settings). Until then nothing is backed up."
	passwordNote := `   Save it in your password manager now. Your backups are encrypted with it, and if your
   servers are ever lost it is the only way to read them. It is not stored on this machine.`
	if os.Getenv("SB_BACKUP_PASSWORD") != "" {
		passwordNote = `   This is the password you passed in SB_BACKUP_PASSWORD. To bring back the apps of an earlier
   install from this bucket: after setup, open Settings in the dashboard and use
   "Look in the backup repository", then "Recreate all".`
	}
	if *bBucket != "" {
		prefix := *bPrefix
		if prefix == "" {
			prefix = "slicedbread/" + st.Domain
		}
		backupLine = strings.TrimRight(*bEndpoint, "/") + "/" + *bBucket + "/" + prefix + ", daily at 03:00 UTC with a restore test after each backup"
	}
	fmt.Printf(`
SlicedBread is ready.

1. Finish setup in your browser (creates your owner account; works once, for 24 hours):
     %s

2. Backup encryption password:
     %s
%s

  install id:  %s
  apps:        https://<app>.%s
  backups:     %s
  this CLI:    logged in with an agent token (%s). Agents use it too: deploys and
               config go through, making apps public, deleting and restoring wait for your
               approval in the dashboard.%s
  later:       sb setup-link %s (new setup link), sb destroy %s (remove everything)
`, setupLink, backupPassword, passwordNote, st.RunID, st.Domain, backupLine, cfgPath, expiryLine(st), st.RunID, st.RunID)
	if !*yes && term.IsTerminal(int(os.Stdin.Fd())) {
		fmt.Fprint(os.Stderr, "\nPress Enter once you have saved the backup encryption password. ")
		bufio.NewReader(os.Stdin).ReadString('\n')
	}
	return nil
}

// runSetupLink installs a fresh one-time setup code and prints the link.
func runSetupLink(args []string) error {
	if len(args) != 1 {
		return &exitErr{exitUsage, "usage: sb setup-link <install-id|domain>\nPrints a new one-time link to create the owner account (works once, for 24 hours). Earlier links stop working."}
	}
	st, err := loadInstall(args[0])
	if err != nil {
		return err
	}
	p := tenant.New(baseConfig("", ""), installsRoot())
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	link, err := p.SetupCode(ctx, st)
	if err != nil {
		return err
	}
	fmt.Println(link)
	fmt.Fprintln(os.Stderr, "Open it in your browser to create the owner account. If the install is already set up, the page says so.")
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
		exp := ""
		if !st.ExpiresAt.IsZero() {
			exp = "  expires " + st.ExpiresAt.UTC().Format(time.RFC3339)
		}
		rows = append(rows, fmt.Sprintf("%s  %d node(s)  https://<app>.%s%s", st.RunID, st.Size, st.Domain, exp))
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
	if err := p.DeployControl(ctx, st, root); err != nil {
		return err
	}
	if _, err := st.Kubectl(ctx, "-n sb-system rollout status deploy/sb-control --timeout=180s", nil); err != nil {
		return err
	}
	fmt.Println("upgraded", st.RunID)
	return nil
}

// runSSH prints an SSH command for a node of an install, with its host key pinned.
func runSSH(args []string) error {
	if len(args) < 1 {
		return &exitErr{exitUsage, "usage: sb ssh <install-id|domain> [node number]\nPrints an SSH command (host key pinned). SSH is open only to the IP address sb init ran from."}
	}
	st, err := loadInstall(args[0])
	if err != nil {
		return err
	}
	i := 0
	if len(args) > 1 {
		fmt.Sscanf(args[1], "%d", &i)
		i--
	}
	if i < 0 || i >= len(st.Nodes) {
		return &exitErr{exitUsage, fmt.Sprintf("node must be 1-%d", len(st.Nodes))}
	}
	kh := filepath.Join(st.Dir, "known_hosts")
	var lines []string
	for _, n := range st.Nodes {
		lines = append(lines, n.PublicIP+" "+n.HostKey)
	}
	if err := os.WriteFile(kh, []byte(strings.Join(lines, "\n")+"\n"), 0o600); err != nil {
		return err
	}
	fmt.Printf("ssh -i %s -o UserKnownHostsFile=%s -o StrictHostKeyChecking=yes root@%s\n", filepath.Join(st.Dir, "id_ed25519"), kh, st.Nodes[i].PublicIP)
	return nil
}

func expiryLine(st *tenant.State) string {
	if st.ExpiresAt.IsZero() {
		return ""
	}
	return "\n  expires:     " + st.ExpiresAt.UTC().Format(time.RFC3339) + " (test install: everything is deleted then)"
}
