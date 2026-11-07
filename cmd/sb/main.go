// Command sb is the SlicedBread CLI. For now it only has development commands.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"sort"
	"strings"
	"syscall"
	"time"

	"github.com/melzi-cloud/sliced-bread/internal/config"
	"github.com/melzi-cloud/sliced-bread/internal/labels"
	"github.com/melzi-cloud/sliced-bread/internal/smoke"
	"github.com/melzi-cloud/sliced-bread/internal/tenant"
)

const usage = `usage:
  sb dev tenant up              create a tenant (k3s, addons) and print its run id
  sb dev tenant down <run-id>   delete everything belonging to a run
  sb dev tenant list            list labelled resources grouped by run
  sb dev tenant ssh <run-id> [node]  print an ssh command for a node (default 1)
  sb dev reap                   delete runs whose resources have expired
  sb dev smoke [-keep-on-failure]   create, verify and destroy a tenant
  sb dev prototype up           tenant + SlicedBread platform with trusted certificates
  sb dev control push <run-id>  rebuild sb-control and roll it out to a running tenant

Secrets come from the environment. Run through hack/secrets-env.`

func main() {
	args := os.Args[1:]
	if len(args) > 0 {
		switch args[0] {
		case "init":
			os.Exit(handleExit(runInit(args[1:])))
		case "destroy":
			os.Exit(handleExit(runDestroy(args[1:])))
		case "installs":
			os.Exit(handleExit(runInstalls()))
		case "upgrade":
			os.Exit(handleExit(runUpgrade(args[1:])))
		case "ssh":
			os.Exit(handleExit(runSSH(args[1:])))
		case "setup-link":
			os.Exit(handleExit(runSetupLink(args[1:])))
		}
	}
	if len(args) > 0 && args[0] == "dev" {
		if err := run(args); err != nil {
			fmt.Fprintln(os.Stderr, "error:", err)
			os.Exit(1)
		}
		return
	}
	os.Exit(handleExit(runUser(args)))
}

func run(args []string) error {
	if len(args) < 2 || args[0] != "dev" {
		return errors.New(usage)
	}
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()

	root, err := repoRoot()
	if err != nil {
		return err
	}
	cfg, err := config.LoadDev()
	if err != nil {
		return err
	}
	p := tenant.New(cfg, filepath.Join(root, tenant.StateRoot))

	switch args[1] {
	case "reap":
		runs, err := p.Reap(ctx, time.Now())
		fmt.Printf("reaped %d run(s) %v\n", len(runs), runs)
		return err
	case "smoke":
		fs := flag.NewFlagSet("smoke", flag.ContinueOnError)
		keep := fs.Bool("keep-on-failure", false, "leave resources up after a failure")
		if err := fs.Parse(args[2:]); err != nil {
			return err
		}
		res, err := smoke.Run(ctx, p, smoke.Options{ArtifactsRoot: filepath.Join(root, "test", "artifacts"), KeepOnFailure: *keep})
		fmt.Printf("smoke run %s finished in %s\n", res.RunID, res.Duration.Round(time.Second))
		return err
	case "tenant":
		if len(args) < 3 {
			return errors.New(usage)
		}
		return tenantCmd(ctx, p, args[2:])
	case "prototype":
		if len(args) < 3 || args[2] != "up" {
			return errors.New(usage)
		}
		return prototypeUp(ctx, p, root)
	case "control":
		if len(args) < 4 || args[2] != "push" {
			return errors.New(usage)
		}
		st, err := tenant.LoadState(p.StateRoot, args[3])
		if err != nil {
			return err
		}
		st.RefreshNodes(ctx)
		if err := p.DeployControl(ctx, st, root); err != nil {
			return err
		}
		_, err = st.Kubectl(ctx, "-n sb-system rollout status deploy/sb-control --timeout=120s", nil)
		return err
	}
	return errors.New(usage)
}

func tenantCmd(ctx context.Context, p *tenant.Provisioner, args []string) error {
	switch args[0] {
	case "up":
		runID := labels.NewRunID()
		st, err := p.Up(ctx, runID)
		if err == nil {
			err = p.InstallAddons(ctx, st)
		}
		if err != nil {
			fmt.Fprintf(os.Stderr, "tenant %s failed; resources expire in %s, or run: sb dev tenant down %s\n", runID, p.Cfg.TTL, runID)
			return err
		}
		fmt.Printf("run id:  %s\ndomain:  *.%s\nlb ip:   %s\nexpires: %s\nstate:   %s\n", st.RunID, st.Domain, st.LBIP, st.ExpiresAt.Local().Format(time.RFC3339), st.Dir)
		return nil
	case "down":
		if len(args) < 2 {
			return errors.New("sb dev tenant down <run-id>")
		}
		return p.Down(ctx, args[1])
	case "list":
		res, err := p.Inventory(ctx, "")
		if err != nil {
			return err
		}
		byRun := map[string][]tenant.Resource{}
		for _, r := range res {
			byRun[r.Labels[labels.RunID]] = append(byRun[r.Labels[labels.RunID]], r)
		}
		runs := make([]string, 0, len(byRun))
		for k := range byRun {
			runs = append(runs, k)
		}
		sort.Strings(runs)
		if len(runs) == 0 {
			fmt.Println("no labelled resources")
		}
		for _, id := range runs {
			fmt.Printf("%s (expired=%v)\n", id, labels.Expired(byRun[id][0].Labels, time.Now()))
			for _, r := range byRun[id] {
				fmt.Printf("  %s\n", r)
			}
		}
		return nil
	case "ssh":
		if len(args) < 2 {
			return errors.New("sb dev tenant ssh <run-id> [node]")
		}
		st, err := tenant.LoadState(p.StateRoot, args[1])
		if err != nil {
			return err
		}
		st.RefreshNodes(ctx)
		i := 0
		if len(args) > 2 {
			fmt.Sscanf(args[2], "%d", &i)
			i--
		}
		if i < 0 || i >= len(st.Nodes) {
			return fmt.Errorf("node out of range")
		}
		kh := filepath.Join(st.Dir, "known_hosts")
		var lines []string
		for _, n := range st.Nodes {
			lines = append(lines, n.PublicIP+" "+n.HostKey)
		}
		if err := os.WriteFile(kh, []byte(strings.Join(lines, "\n")+"\n"), 0o600); err != nil {
			return err
		}
		fmt.Printf("ssh -i %s -o UserKnownHostsFile=%s -o StrictHostKeyChecking=yes root@%s\n",
			filepath.Join(st.Dir, "id_ed25519"), kh, st.Nodes[i].PublicIP)
		return nil
	}
	return errors.New(usage)
}

// prototypeUp creates a tenant with production certificates and the full platform.
func prototypeUp(ctx context.Context, p *tenant.Provisioner, root string) error {
	p.Cfg.ACMEServer = "https://acme-v02.api.letsencrypt.org/directory" // trusted certs for browsers and Bitwarden clients
	if os.Getenv("SB_TTL") == "" {
		p.Cfg.TTL = 12 * time.Hour
	}
	runID := labels.NewRunID()
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
	if err == nil {
		err = devSetup(ctx, p, st)
	}
	if err != nil {
		if st != nil && len(st.Nodes) > 0 {
			p.CollectArtifacts(context.Background(), st, filepath.Join(root, "test", "artifacts", runID))
		}
		fmt.Fprintf(os.Stderr, "prototype %s failed; tear down with: make tenant-down RUN=%s\n", runID, runID)
		return err
	}
	fmt.Printf(`prototype ready
  run id:      %s
  dashboard:   https://auth.%s   (login: %s)
  api:         https://api.%s
  agent cli:   SB_CONFIG=%s sb apps
  admin cli:   SB_CONFIG=%s sb approvals   (dev only: a 1-day admin token created in the dashboard)
  expires:     %s (tear down with: make tenant-down RUN=%s)
`, st.RunID, st.Domain, filepath.Join(st.Dir, "admin-login.json"), st.Domain,
		filepath.Join(st.Dir, "cli-agent.json"), filepath.Join(st.Dir, "cli-admin.json"),
		st.ExpiresAt.Local().Format(time.RFC3339), st.RunID)
	return nil
}

func repoRoot() (string, error) {
	dir, err := os.Getwd()
	if err != nil {
		return "", err
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir, nil
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return "", errors.New("run sb from inside the repository")
		}
		dir = parent
	}
}
