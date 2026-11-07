package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"text/tabwriter"
	"time"
)

// Exit codes the skill file documents.
const (
	exitOK       = 0
	exitError    = 1
	exitUsage    = 2
	exitApproval = 3 // change recorded, waiting for a human
)

type exitErr struct {
	code int
	msg  string
}

func (e *exitErr) Error() string { return e.msg }

const userUsage = `sb: deploy and operate apps on SlicedBread.

Install (on your own Hetzner account; run by a human)
  sb init --domain apps.example.com [--size 1|3]   prints a setup link and the backup password; see: sb init --help
  sb setup-link <install-id>          a new one-time link to create the owner account
  sb installs                         installs created from this machine
  sb upgrade <install-id>             roll out the control plane built into this sb
  sb ssh <install-id> [node]          print an SSH command for a node (host key pinned)
  sb destroy <install-id>             delete an install and all its data

Humans do admin work in the dashboard (https://auth.<tenant-domain>): approvals, members,
backup settings and password, restoring every app, alert webhook, export, credentials.
This CLI is for agents (and humans doing agent-level work); its tokens cannot do those things.

Setup
  sb login --api https://api.<tenant-domain>    prints a link; a human approves in the browser
  sb login --api https://api.<tenant-domain> --token-stdin   (token on stdin instead)
  sb whoami

Discover
  sb catalog                          templates, their settings and actions
  sb apps                             all apps with state and URL
  sb status <app>                     details, env keys, actions, notes
  sb check <app> [/path] [--contains text]   fetch a page as a signed-in user would see it (works behind the wall);
                                             exits 1 if --contains is not found

Deploy (exactly one source)
  sb deploy <app> --template <name> [--set key=value]...
  sb deploy <app> --image <image> [--port N]
  sb deploy <app> --dir <local-path> [--port N] [--dry-run]   (uploads source; built with Railpack or its Dockerfile;
                                                                --dry-run lists the files that would be uploaded)
  sb deploy <app> --repo <https-git-url> [--branch B] [--path dir] [--port N]   (public repos)
  sb deploy <app> --template-dir <path>   a template you are writing (template.yaml, actions/, files/); see the skill file
    common flags: --env KEY=VALUE (repeatable), --generate KEY (a random secret made by the server),
                  --request KEY[:description] (a secret the user enters in the dashboard),
                  --database (Postgres; sets DATABASE_URL), --health-path /healthz,
                  --access wall|app-auth|public (default wall), --public-path /path (repeatable)
  sb wait <app> [--timeout 10m]       block until the latest version is serving and ready (exit 1 if its build failed)

Operate
  sb logs <app> [--build] [--tail N]
  sb env <app>                        list env keys (values of secrets are never shown)
  sb env set <app> KEY=VALUE...       plain values only; never put secrets here
  sb env generate <app> KEY...        the server generates a random secret (you never see it)
  sb env request <app> KEY [--description text]   the user enters the value in the dashboard
  sb env unset <app> KEY...
  sb settings <app> key=value...      template settings, e.g. vaultwarden signups=true
  sb access <app> wall|app-auth|public [--public-path /path]...
  sb action <app> <action> [--input key=value]...
  sb redeploy <app> [--dir <path>]    rebuild (upload new source with --dir), or restart image apps
  sb delete <app>

Backups (apps with a volume or database; each backup is followed by a restore test)
  sb backups [<app>]                  target, schedule, last backup and restore test per app (one app: snapshots too)
  sb backup <app> [--wait]            back up now
  sb restore-test <app> [--snapshot ID] [--wait]   restore a snapshot into a scratch namespace and check it
  sb restore <app> [--snapshot ID] [--wait]        replace the app's data with a snapshot (latest by default);
                                                   stops the app meanwhile; agents need human approval
  sb backups --repository             every app in the backup repository, including deleted ones
  sb restore <app> --recreate [--snapshot ID] [--wait]   bring back a deleted app from its backup

Alerts (also on the dashboard at https://auth.<tenant-domain>/)
  sb alerts                           open alerts: failed backups or restore tests, apps down, certificates, servers

Members (people who get past the login wall)
  sb members                          members, invites, whether members can approve

Approvals (privileged changes requested by agents)
  sb approvals [--app <app>] [--wait <id>] [--timeout 30m]   list (optionally one app), or block until a human decides
  (a human approves or denies in the dashboard: https://auth.<tenant-domain>/approvals)

Admin (done by a human in the dashboard; these commands only print the link)
  sb approve <id> | sb deny <id>      https://auth.<tenant-domain>/approvals
  sb backups configure                https://auth.<tenant-domain>/settings (bucket and keys)
  sb backups password                 https://auth.<tenant-domain>/settings (encryption password)
  sb export                           https://auth.<tenant-domain>/settings (downloads a .tar.gz of every app's manifests)
  sb restore --all                    https://auth.<tenant-domain>/settings (restore every app from the bucket)
  sb members invite|remove|role       https://auth.<tenant-domain>/members
  sb alerts configure|test            https://auth.<tenant-domain>/settings (webhook)

Output is human-readable; add --json to any command for JSON. Config: $SB_CONFIG or ~/.config/slicedbread/config.json.
Exit codes: 0 ok; 1 error (failed build, denied or obsolete approval, sb check --contains not found);
2 usage; 3 waiting for human approval (also when sb approvals --wait times out).`

type cliConfig struct {
	API   string `json:"api"`
	Token string `json:"token"`
}

func configPath() string {
	if p := os.Getenv("SB_CONFIG"); p != "" {
		return p
	}
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".config", "slicedbread", "config.json")
}

func loadConfig() (cliConfig, error) {
	var c cliConfig
	b, err := os.ReadFile(configPath())
	if err != nil {
		return c, &exitErr{exitError, "not logged in (" + configPath() + " missing). Run: sb login --api https://api.<tenant-domain>"}
	}
	if err := json.Unmarshal(b, &c); err != nil || c.API == "" || c.Token == "" {
		return c, &exitErr{exitError, "invalid config at " + configPath()}
	}
	return c, nil
}

type client struct {
	cfg  cliConfig
	http *http.Client
	json bool
}

type apiErr struct {
	Error string `json:"error"`
	Hint  string `json:"hint"`
}

// call performs a request. For 202 responses it returns the body and an approval exit error after printing.
func (c *client) call(method, path string, body any) ([]byte, int, error) {
	var rdr io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		rdr = bytes.NewReader(b)
	}
	req, err := http.NewRequest(method, strings.TrimSuffix(c.cfg.API, "/")+path, rdr)
	if err != nil {
		return nil, 0, err
	}
	req.Header.Set("Authorization", "Bearer "+c.cfg.Token)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, 0, &exitErr{exitError, "cannot reach " + c.cfg.API + ": " + err.Error()}
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if resp.StatusCode >= 400 {
		var e apiErr
		if json.Unmarshal(b, &e) == nil && e.Error != "" {
			msg := "error: " + e.Error
			if e.Hint != "" {
				msg += "\nhint: " + e.Hint
			}
			return b, resp.StatusCode, &exitErr{exitError, msg}
		}
		return b, resp.StatusCode, &exitErr{exitError, fmt.Sprintf("error: HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(b)))}
	}
	return b, resp.StatusCode, nil
}

func (c *client) print(b []byte) {
	var v any
	if json.Unmarshal(b, &v) != nil {
		os.Stdout.Write(b)
		return
	}
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	enc.SetIndent("", "  ")
	enc.Encode(v)
	os.Stdout.Write(buf.Bytes())
}

// approvalExit prints a pending approval and returns exit code 3.
func (c *client) approvalExit(b []byte) error {
	var ap map[string]any
	json.Unmarshal(b, &ap)
	if c.json || jsonMode {
		c.print(b)
	} else {
		fmt.Printf("pending approval %v: %v\napprove at: %v\nhint: %v\n", ap["approval"], ap["summary"], ap["approveUrl"], ap["hint"])
	}
	return &exitErr{exitApproval, ""}
}

type multi []string

func (m *multi) String() string     { return strings.Join(*m, ",") }
func (m *multi) Set(v string) error { *m = append(*m, v); return nil }

func kvs(items []string, what string) (map[string]string, error) {
	out := map[string]string{}
	for _, it := range items {
		k, v, ok := strings.Cut(it, "=")
		if !ok || k == "" {
			return nil, &exitErr{exitUsage, fmt.Sprintf("%s %q must look like KEY=VALUE", what, it)}
		}
		out[k] = v
	}
	return out, nil
}

// readTemplateDir reads a template directory: template.yaml plus every other file.
func readTemplateDir(dir string) (map[string]string, error) {
	out := map[string]string{}
	total := 0
	err := filepath.WalkDir(dir, func(p string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if n := d.Name(); p != dir && (strings.HasPrefix(n, ".") || n == "node_modules") {
				return filepath.SkipDir
			}
			return nil
		}
		rel, _ := filepath.Rel(dir, p)
		b, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		total += len(b)
		if total > 512<<10 {
			return fmt.Errorf("template directory is larger than 512 KB")
		}
		out[filepath.ToSlash(rel)] = string(b)
		return nil
	})
	if err != nil {
		return nil, err
	}
	if _, ok := out["template.yaml"]; !ok {
		return nil, &exitErr{exitUsage, dir + " has no template.yaml"}
	}
	return out, nil
}

// secretRequests turns KEY[:description] flags into requests.
func secretRequests(rs []string) []map[string]any {
	var out []map[string]any
	for _, r := range rs {
		k, d, _ := strings.Cut(r, ":")
		out = append(out, map[string]any{"name": k, "description": d, "required": true})
	}
	return out
}

// actionInfo is an action as sb catalog and sb status print it.
type actionInfo struct {
	Name, Description string
	Input             map[string]struct {
		Type, Description string
		Required          bool
	}
	On        string
	Approval  bool
	HumanOnly bool
}

func (a actionInfo) line() string {
	var ins []string
	for k, f := range a.Input {
		s := "--input " + k + "=<" + f.Description + ">"
		if !f.Required {
			s = "[" + s + "]"
		}
		ins = append(ins, s)
	}
	sort.Strings(ins)
	out := strings.TrimSpace(a.Name+" "+strings.Join(ins, " ")) + ": " + a.Description
	switch {
	case a.On == "install":
		out += " [runs by itself when the app is installed]"
	case a.HumanOnly:
		out += " [shows credentials: a human runs it on the app page in the dashboard]"
	case a.Approval:
		out += " [needs human approval]"
	}
	return out
}

var jsonMode bool

// parse splits positional arguments from flags so flags may come after positionals.
func parse(fs *flag.FlagSet, args []string) ([]string, error) {
	var pos []string
	defer func() {
		if f := fs.Lookup("json"); f != nil && f.Value.String() == "true" {
			jsonMode = true
		}
	}()
	for len(args) > 0 {
		if err := fs.Parse(args); err != nil {
			if errors.Is(err, flag.ErrHelp) {
				fmt.Println(commandHelp(fs.Name()))
				return nil, &exitErr{exitOK, ""}
			}
			return nil, &exitErr{exitUsage, err.Error() + "\n" + commandHelp(fs.Name())}
		}
		args = fs.Args()
		if len(args) > 0 {
			pos = append(pos, args[0])
			args = args[1:]
		}
	}
	return pos, nil
}

func runUser(args []string) error {
	if len(args) == 0 || args[0] == "help" || args[0] == "-h" || args[0] == "--help" {
		fmt.Println(userUsage)
		return nil
	}
	cmd, rest := args[0], args[1:]
	for _, a := range rest {
		if a == "-h" || a == "--help" || a == "-help" {
			fmt.Println(commandHelp(cmd))
			return nil
		}
	}
	fs := flag.NewFlagSet(cmd, flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	jsonOut := fs.Bool("json", false, "")

	if cmd == "login" {
		api := fs.String("api", "", "")
		tokenStdin := fs.Bool("token-stdin", false, "")
		if _, err := parse(fs, rest); err != nil {
			return err
		}
		if *api == "" {
			return &exitErr{exitUsage, "usage: sb login --api https://api.<tenant-domain> [--token-stdin]"}
		}
		c := cliConfig{API: strings.TrimSuffix(*api, "/")}
		if *tokenStdin {
			tok, _ := io.ReadAll(io.LimitReader(os.Stdin, 4096))
			c.Token = strings.TrimSpace(string(tok))
			if c.Token == "" {
				return &exitErr{exitUsage, "pipe the API token on stdin"}
			}
		} else {
			tok, err := deviceLogin(c.API)
			if err != nil {
				return err
			}
			c.Token = tok
		}
		cl := &client{cfg: c, http: &http.Client{Timeout: 30 * time.Second}}
		b, _, err := cl.call("GET", "/v1/whoami", nil)
		if err != nil {
			return err
		}
		if err := os.MkdirAll(filepath.Dir(configPath()), 0o700); err != nil {
			return err
		}
		cb, _ := json.MarshalIndent(c, "", "  ")
		if err := os.WriteFile(configPath(), cb, 0o600); err != nil {
			return err
		}
		fmt.Println("logged in; config written to", configPath())
		cl.print(b)
		return nil
	}

	cfg, err := loadConfig()
	if err != nil {
		return err
	}
	c := &client{cfg: cfg, http: &http.Client{Timeout: 90 * time.Second}}

	if ok, err := c.runBackupCmd(cmd, fs, rest, jsonOut); ok {
		return err
	}
	if cmd == "alerts" {
		return c.runAlerts(fs, rest, jsonOut)
	}
	if cmd == "members" {
		return c.runMembers(fs, rest, jsonOut)
	}

	switch cmd {
	case "whoami":
		if _, err := parse(fs, rest); err != nil {
			return err
		}
		b, _, err := c.call("GET", "/v1/whoami", nil)
		if err != nil {
			return err
		}
		if *jsonOut {
			c.print(b)
			return nil
		}
		var wi struct {
			Scope, Domain, Dashboard, Approvals string
			Team                                []string
		}
		json.Unmarshal(b, &wi)
		fmt.Printf("tenant domain: %s\napp urls:      https://<app>.%s\ntoken scope:   %s\nteam (admitted by the login wall): %s\ndashboard:     %s\napprovals:     %s\n",
			wi.Domain, wi.Domain, wi.Scope, strings.Join(wi.Team, ", "), wi.Dashboard, wi.Approvals)
		return nil

	case "check":
		path := fs.String("path", "/", "")
		contains := fs.String("contains", "", "")
		pos, err := parse(fs, rest)
		if err != nil {
			return err
		}
		app, err := needApp(pos, "sb check <app> [--path /x] [--contains text]")
		if err != nil {
			return err
		}
		if len(pos) > 1 && strings.HasPrefix(pos[1], "/") {
			*path = pos[1]
		}
		q := "?path=" + url.QueryEscape(*path)
		if *contains != "" {
			q += "&contains=" + url.QueryEscape(*contains)
		}
		b, _, err := c.call("GET", "/v1/apps/"+url.PathEscape(app)+"/check"+q, nil)
		if err != nil {
			return err
		}
		if *jsonOut {
			c.print(b)
			return nil
		}
		var cr struct {
			Path, ContentType, Title, Snippet, Note string
			Status, Bytes                           int
			Contains                                *bool
		}
		json.Unmarshal(b, &cr)
		fmt.Printf("GET %s -> %d (%s, %d bytes)\n", cr.Path, cr.Status, cr.ContentType, cr.Bytes)
		if cr.Title != "" {
			fmt.Println("title:", cr.Title)
		}
		if cr.Contains != nil {
			fmt.Printf("contains %q: %v\n", *contains, *cr.Contains)
		}
		fmt.Println("note:", cr.Note)
		fmt.Println("--- body (first 1500 bytes) ---")
		fmt.Println(cr.Snippet)
		if cr.Contains != nil && !*cr.Contains {
			return &exitErr{exitError, ""}
		}
		return nil

	case "catalog":
		if _, err := parse(fs, rest); err != nil {
			return err
		}
		b, _, err := c.call("GET", "/v1/catalog", nil)
		if err != nil {
			return err
		}
		if *jsonOut {
			c.print(b)
			return nil
		}
		var cat struct {
			Templates []struct {
				Name, Version, Description, Access, Notes string
				Components                                []string
				Database                                  bool
				Secrets                                   []string
				Settings                                  []struct {
					Name, Description, Type, Default string
					Values, ApprovalFor              []string
				}
				Actions []actionInfo
				Setup   []struct{ Name, Description string }
			}
		}
		json.Unmarshal(b, &cat)
		for _, t := range cat.Templates {
			fmt.Printf("%s %s: %s\n  access: %s; components: %s", t.Name, t.Version, t.Description, t.Access, strings.Join(t.Components, ", "))
			if t.Database {
				fmt.Print("; Postgres database")
			}
			fmt.Println()
			for _, st := range t.Settings {
				vals := st.Type
				if len(st.Values) > 0 {
					vals = strings.Join(st.Values, "|")
				}
				def := st.Default
				if def == "" {
					def = "empty"
				}
				fmt.Printf("  setting %s (%s; default: %s)", st.Name, vals, def)
				if len(st.ApprovalFor) > 0 {
					fmt.Printf(" [%s needs human approval]", strings.Join(st.ApprovalFor, ","))
				}
				fmt.Printf(": %s\n", st.Description)
			}
			for _, ac := range t.Actions {
				fmt.Printf("  action %s\n", ac.line())
			}
			for _, sp := range t.Setup {
				fmt.Printf("  setup for the human: %s\n", sp.Description)
			}
			if len(t.Secrets) > 0 {
				fmt.Printf("  secrets (values never shown): %s\n", strings.Join(t.Secrets, ", "))
			}
			fmt.Printf("  notes: %s\n\n", strings.TrimSpace(t.Notes))
		}
		return nil

	case "apps":
		if _, err := parse(fs, rest); err != nil {
			return err
		}
		c.json = *jsonOut
		b, _, err := c.call("GET", "/v1/apps", nil)
		if err != nil {
			return err
		}
		if c.json {
			c.print(b)
			return nil
		}
		var r struct {
			Apps []struct{ Name, State, Access, URL, Source, Message string } `json:"apps"`
		}
		json.Unmarshal(b, &r)
		if len(r.Apps) == 0 {
			fmt.Println("no apps yet; deploy one with: sb deploy <app> --template <name>|--image <image>|--repo <url>")
			return nil
		}
		tw := tabwriter.NewWriter(os.Stdout, 0, 2, 2, ' ', 0)
		fmt.Fprintln(tw, "NAME\tSTATE\tACCESS\tURL\tSOURCE")
		for _, a := range r.Apps {
			fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\n", a.Name, a.State, a.Access, a.URL, a.Source)
		}
		return tw.Flush()

	case "status":
		pos, err := parse(fs, rest)
		if err != nil {
			return err
		}
		app, err := needApp(pos, "sb status <app>")
		if err != nil {
			return err
		}
		b, _, err := c.call("GET", "/v1/apps/"+url.PathEscape(app), nil)
		if err != nil {
			return err
		}
		if *jsonOut {
			c.print(b)
			return nil
		}
		var st map[string]any
		json.Unmarshal(b, &st)
		printStatus(st)
		return nil

	case "deploy":
		var env, pubs, sets multi
		tmpl := fs.String("template", "", "")
		image := fs.String("image", "", "")
		repo := fs.String("repo", "", "")
		dir := fs.String("dir", "", "")
		dryRun := fs.Bool("dry-run", false, "")
		branch := fs.String("branch", "", "")
		path := fs.String("path", "", "")
		port := fs.Int("port", 0, "")
		access := fs.String("access", "", "")
		tmplDir := fs.String("template-dir", "", "")
		var gens, reqs multi
		fs.Var(&gens, "generate", "")
		fs.Var(&reqs, "request", "")
		database := fs.Bool("database", false, "")
		healthPath := fs.String("health-path", "", "")
		fs.Var(&env, "env", "")
		fs.Var(&pubs, "public-path", "")
		fs.Var(&sets, "set", "")
		pos, err := parse(fs, rest)
		if err != nil {
			return err
		}
		c.json = *jsonOut
		app, err := needApp(pos, "sb deploy <app> --template <name> | --image <image> | --repo <url>")
		if err != nil {
			return err
		}
		envMap, err := kvs(env, "--env")
		if err != nil {
			return err
		}
		setMap, err := kvs(sets, "--set")
		if err != nil {
			return err
		}
		req := map[string]any{"name": app}
		if *tmpl != "" {
			req["template"] = *tmpl
		}
		if *image != "" {
			req["image"] = *image
		}
		if *tmplDir != "" {
			files, err := readTemplateDir(*tmplDir)
			if err != nil {
				return err
			}
			req["templateFiles"] = files
		}
		if len(gens) > 0 {
			req["generate"] = []string(gens)
		}
		if len(reqs) > 0 {
			req["request"] = secretRequests(reqs)
		}
		if *repo != "" {
			req["repo"] = map[string]string{"url": *repo, "branch": *branch, "path": *path}
		}
		if *dir != "" {
			if *dryRun {
				return listUpload(*dir)
			}
			id, err := c.upload(*dir)
			if err != nil {
				return err
			}
			req["upload"] = id
		}
		if *port > 0 {
			req["port"] = *port
		}
		if len(envMap) > 0 {
			req["env"] = envMap
		}
		if len(setMap) > 0 {
			req["settings"] = setMap
		}
		if *access != "" {
			req["access"] = *access
		}
		if *database {
			req["database"] = true
		}
		if *healthPath != "" {
			req["healthPath"] = *healthPath
		}
		if len(pubs) > 0 {
			req["publicPaths"] = []string(pubs)
		}
		b, code, err := c.call("POST", "/v1/apps", req)
		if err != nil {
			return err
		}
		if code == http.StatusAccepted {
			return c.approvalExit(b)
		}
		return c.printAppResult(b)

	case "wait":
		timeout := fs.Duration("timeout", 10*time.Minute, "")
		pos, err := parse(fs, rest)
		if err != nil {
			return err
		}
		app, err := needApp(pos, "sb wait <app> [--timeout 10m]")
		if err != nil {
			return err
		}
		return c.wait(app, *timeout)

	case "logs":
		build := fs.Bool("build", false, "")
		tail := fs.Int("tail", 0, "")
		component := fs.String("component", "", "")
		action := fs.String("action", "", "")
		pos, err := parse(fs, rest)
		if err != nil {
			return err
		}
		app, err := needApp(pos, "sb logs <app> [--build] [--tail N]")
		if err != nil {
			return err
		}
		if *tail == 0 {
			*tail = 100
			if *build {
				*tail = 1000
			}
		}
		q := fmt.Sprintf("?tail=%d", *tail)
		if *build {
			q += "&build=1"
		}
		if *component != "" {
			q += "&component=" + url.QueryEscape(*component)
		}
		if *action != "" {
			q += "&action=" + url.QueryEscape(*action)
		}
		b, _, err := c.call("GET", "/v1/apps/"+url.PathEscape(app)+"/logs"+q, nil)
		if err != nil {
			return err
		}
		os.Stdout.Write(b)
		return nil

	case "env":
		desc := fs.String("description", "", "")
		pos, err := parse(fs, rest)
		if err != nil {
			return err
		}
		if len(pos) == 1 {
			b, _, err := c.call("GET", "/v1/apps/"+url.PathEscape(pos[0]), nil)
			if err != nil {
				return err
			}
			var st struct {
				Env        map[string]string `json:"env"`
				SecretKeys []string          `json:"secretKeys"`
				Secrets    []string          `json:"secrets"`
			}
			json.Unmarshal(b, &st)
			if jsonMode {
				out, _ := json.Marshal(map[string]any{"env": st.Env, "secretKeys": st.SecretKeys, "secrets": st.Secrets})
				c.print(out)
				return nil
			}
			keys := make([]string, 0, len(st.Env))
			for k := range st.Env {
				keys = append(keys, k)
			}
			sort.Strings(keys)
			for _, k := range keys {
				fmt.Printf("%s=%s\n", k, st.Env[k])
			}
			for _, k := range st.SecretKeys {
				fmt.Printf("%s=<secret>\n", k)
			}
			return nil
		}
		usage := "usage: sb env <app> | sb env set <app> KEY=VALUE... | sb env unset <app> KEY... | sb env generate <app> KEY... | sb env request <app> KEY [--description text]"
		if len(pos) < 3 {
			return &exitErr{exitUsage, usage}
		}
		app := pos[1]
		req := map[string]any{}
		switch pos[0] {
		case "set":
			set, err := kvs(pos[2:], "env")
			if err != nil {
				return err
			}
			req["set"] = set
		case "unset":
			req["unset"] = pos[2:]
		case "generate":
			req["generate"] = pos[2:]
		case "request":
			var rs []map[string]any
			for _, k := range pos[2:] {
				rs = append(rs, map[string]any{"name": k, "description": *desc, "required": true})
			}
			req["request"] = rs
		default:
			return &exitErr{exitUsage, usage}
		}
		b, _, err := c.call("PATCH", "/v1/apps/"+url.PathEscape(app)+"/env", req)
		if err != nil {
			return err
		}
		if pos[0] == "request" && !jsonMode {
			fmt.Printf("requested %s: the user enters the value(s) on the app page in the dashboard: %s\n", strings.Join(pos[2:], ", "), c.dashboardURL("/apps/"+app))
		}
		return c.printAppResult(b)

	case "settings":
		pos, err := parse(fs, rest)
		if err != nil {
			return err
		}
		if len(pos) < 2 {
			return &exitErr{exitUsage, "usage: sb settings <app> key=value..."}
		}
		set, err := kvs(pos[1:], "setting")
		if err != nil {
			return err
		}
		b, code, err := c.call("POST", "/v1/apps/"+url.PathEscape(pos[0])+"/settings", map[string]any{"settings": set})
		if err != nil {
			return err
		}
		if code == http.StatusAccepted {
			return c.approvalExit(b)
		}
		return c.printAppResult(b)

	case "access":
		var pubs multi
		fs.Var(&pubs, "public-path", "")
		pos, err := parse(fs, rest)
		if err != nil {
			return err
		}
		if len(pos) < 2 {
			return &exitErr{exitUsage, "usage: sb access <app> wall|app-auth|public [--public-path /path]..."}
		}
		req := map[string]any{"access": pos[1]}
		if len(pubs) > 0 {
			req["publicPaths"] = []string(pubs)
		}
		b, code, err := c.call("POST", "/v1/apps/"+url.PathEscape(pos[0])+"/access", req)
		if err != nil {
			return err
		}
		if code == http.StatusAccepted {
			return c.approvalExit(b)
		}
		return c.printAppResult(b)

	case "action":
		var inputs multi
		fs.Var(&inputs, "input", "")
		pos, err := parse(fs, rest)
		if err != nil {
			return err
		}
		if len(pos) < 2 {
			return &exitErr{exitUsage, "usage: sb action <app> <action> [--input key=value]..."}
		}
		in, err := kvs(inputs, "--input")
		if err != nil {
			return err
		}
		b, code, err := c.slow().call("POST", "/v1/apps/"+url.PathEscape(pos[0])+"/actions/"+url.PathEscape(pos[1]), map[string]any{"input": in})
		if err != nil {
			return err
		}
		if code == http.StatusAccepted {
			return c.approvalExit(b)
		}
		if jsonMode {
			c.print(b)
			return nil
		}
		printKV(b)
		return nil

	case "redeploy":
		dir := fs.String("dir", "", "")
		dryRun := fs.Bool("dry-run", false, "")
		pos, err := parse(fs, rest)
		if err != nil {
			return err
		}
		app, err := needApp(pos, "sb redeploy <app> [--dir <path>]")
		if err != nil {
			return err
		}
		var body any
		if *dir != "" && *dryRun {
			return listUpload(*dir)
		}
		if *dir != "" {
			id, err := c.upload(*dir)
			if err != nil {
				return err
			}
			body = map[string]string{"upload": id}
		}
		b, _, err := c.call("POST", "/v1/apps/"+url.PathEscape(app)+"/redeploy", body)
		if err != nil {
			return err
		}
		return c.printAppResult(b)

	case "delete":
		pos, err := parse(fs, rest)
		if err != nil {
			return err
		}
		app, err := needApp(pos, "sb delete <app>")
		if err != nil {
			return err
		}
		b, code, err := c.call("DELETE", "/v1/apps/"+url.PathEscape(app), nil)
		if err != nil {
			return err
		}
		if code == http.StatusAccepted {
			return c.approvalExit(b)
		}
		c.print(b)
		return nil

	case "approvals":
		waitID := fs.String("wait", "", "")
		appFilter := fs.String("app", "", "")
		timeout := fs.Duration("timeout", 30*time.Minute, "")
		if _, err := parse(fs, rest); err != nil {
			return err
		}
		if *waitID != "" {
			return c.waitApproval(*waitID, *timeout)
		}
		b, _, err := c.call("GET", "/v1/approvals", nil)
		if err != nil {
			return err
		}
		if *jsonOut {
			c.print(b)
			return nil
		}
		var al struct {
			Approvals []struct {
				ID, App, Summary, State, Result, RequestedAt, RequestedBy string
			}
			ApproveURL string
		}
		json.Unmarshal(b, &al)
		if len(al.Approvals) == 0 {
			fmt.Println("no approvals")
			return nil
		}
		tw := tabwriter.NewWriter(os.Stdout, 0, 2, 2, ' ', 0)
		fmt.Fprintln(tw, "ID\tSTATE\tAPP\tREQUESTED\tSUMMARY")
		shown := 0
		for _, a := range al.Approvals {
			if *appFilter != "" && a.App != *appFilter {
				continue
			}
			shown++
			sum := a.Summary
			if a.Result != "" && a.State != "pending" {
				sum += " (" + a.Result + ")"
			}
			fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\n", a.ID, a.State, a.App, a.RequestedAt, sum)
		}
		tw.Flush()
		if shown == 0 {
			fmt.Println("(none for", *appFilter+")")
		}
		fmt.Println("approve at:", al.ApproveURL)
		return nil

	case "approve", "deny":
		return c.humanOnly("Approving and denying", "/approvals")
	}
	return &exitErr{exitUsage, "unknown command " + cmd + "\n\n" + userUsage}
}

// printKV prints a JSON object as "key: value" lines; nested values are printed as compact JSON.
func printKV(b []byte) {
	var m map[string]any
	if json.Unmarshal(b, &m) != nil {
		os.Stdout.Write(b)
		return
	}
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		switch v := m[k].(type) {
		case string:
			fmt.Printf("%s: %s\n", k, v)
		case []any:
			fmt.Printf("%s:\n", k)
			for _, it := range v {
				var buf bytes.Buffer
				enc := json.NewEncoder(&buf)
				enc.SetEscapeHTML(false)
				enc.Encode(it)
				fmt.Printf("  - %s", buf.String())
			}
		default:
			var buf bytes.Buffer
			enc := json.NewEncoder(&buf)
			enc.SetEscapeHTML(false)
			enc.Encode(v)
			fmt.Printf("%s: %s", k, buf.String())
		}
	}
}

// printAppResult prints an app status response in a compact form.
func (c *client) printAppResult(b []byte) error {
	if c.json || jsonMode {
		c.print(b)
		return nil
	}
	var r struct {
		App  map[string]any `json:"app"`
		Hint string         `json:"hint"`
	}
	if json.Unmarshal(b, &r) != nil || r.App == nil {
		c.print(b)
		return nil
	}
	printStatus(r.App)
	if r.Hint != "" {
		fmt.Println("hint:", r.Hint)
	}
	return nil
}

func printStatus(st map[string]any) {
	for _, k := range []string{"name", "url", "state", "message", "access", "source", "servingBuild", "database"} {
		if v, ok := st[k]; ok && v != "" && v != nil {
			fmt.Printf("%s: %v\n", k, v)
		}
	}
	if pp, ok := st["publicPaths"].([]any); ok && len(pp) > 0 {
		fmt.Printf("public paths: %v\n", pp)
	}
	if b, ok := st["build"].(map[string]any); ok {
		fmt.Printf("build: %v %v", b["id"], b["phase"])
		if m, _ := b["message"].(string); m != "" {
			fmt.Printf(" (%s)", m)
		}
		fmt.Println()
	}
	if s, ok := st["settings"].(map[string]any); ok && len(s) > 0 {
		var kv []string
		for k, v := range s {
			kv = append(kv, fmt.Sprintf("%s=%v", k, v))
		}
		sort.Strings(kv)
		fmt.Println("settings:", strings.Join(kv, " "))
	}
	var keys []string
	if e, ok := st["envKeys"].([]any); ok {
		for _, k := range e {
			keys = append(keys, fmt.Sprint(k))
		}
	}
	if e, ok := st["secretKeys"].([]any); ok {
		for _, k := range e {
			keys = append(keys, fmt.Sprint(k)+"(secret)")
		}
	}
	if len(keys) > 0 {
		fmt.Println("env:", strings.Join(keys, " "))
	}
	if cs, ok := st["components"].([]any); ok && len(cs) > 1 {
		fmt.Printf("components: %v (logs: sb logs <app> --component <name>)\n", cs)
	}
	if wp, ok := st["wallPaths"].([]any); ok && len(wp) > 0 {
		fmt.Printf("paths behind the login wall: %v\n", wp)
	}
	if sec, ok := st["secrets"].([]any); ok && len(sec) > 0 {
		fmt.Printf("secrets (values never shown): %v\n", sec)
	}
	if acts, ok := st["actions"].([]any); ok && len(acts) > 0 {
		b, _ := json.Marshal(acts)
		var list []actionInfo
		json.Unmarshal(b, &list)
		for _, a := range list {
			fmt.Printf("action: %s\n", a.line())
		}
	}
	if steps, ok := st["setup"].([]any); ok {
		for _, x := range steps {
			m, _ := x.(map[string]any)
			if m["done"] == true {
				fmt.Printf("setup done: %v\n", m["description"])
				continue
			}
			fmt.Printf("setup for the user (not done yet): %v", m["description"])
			if l, _ := m["link"].(string); l != "" {
				fmt.Printf(" -> give the user this link: %s", l)
			}
			fmt.Println()
		}
	}
	if p, ok := st["pendingApprovals"].([]any); ok {
		for _, x := range p {
			fmt.Println("pending approval:", x)
		}
	}
	if v, _ := st["storage"].(string); v != "" {
		fmt.Println("storage:", v)
	}
	if u, _ := st["update"].(string); u != "" {
		fmt.Println("update:", u)
	}
	if n, _ := st["notes"].(string); n != "" {
		fmt.Println("notes:", n)
	}
}

// wait polls an app until its latest version is serving, a build fails, or the timeout passes.
func (c *client) wait(app string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	last := ""
	for {
		b, _, err := c.call("GET", "/v1/apps/"+url.PathEscape(app), nil)
		if err != nil {
			return err
		}
		var st struct {
			State, Message, URL, ServingBuild string
		}
		json.Unmarshal(b, &st)
		line := st.State
		if st.Message != "" {
			line += ": " + st.Message
		}
		if line != last {
			fmt.Printf("%s  %s\n", time.Now().Format("15:04:05"), line)
			last = line
		}
		switch st.State {
		case "running":
			if st.ServingBuild != "" {
				fmt.Printf("ready: %s (serving build %s)\n", st.URL, st.ServingBuild)
			} else {
				fmt.Println("ready:", st.URL)
			}
			return nil
		case "build-failed", "update-failed":
			return &exitErr{exitError, "build failed; read the logs with: sb logs " + app + " --build"}
		}
		if time.Now().After(deadline) {
			return &exitErr{exitError, fmt.Sprintf("timed out after %s (state %s); check: sb status %s and sb logs %s", timeout, st.State, app, app)}
		}
		time.Sleep(5 * time.Second)
	}
}

// deviceLogin runs the browser approval flow and returns an agent token.
func deviceLogin(api string) (string, error) {
	hc := &http.Client{Timeout: 30 * time.Second}
	host, _ := os.Hostname()
	resp, err := hc.Post(api+"/v1/device?label="+url.QueryEscape(host), "application/json", nil)
	if err != nil {
		return "", &exitErr{exitError, "cannot reach " + api + ": " + err.Error()}
	}
	var start struct {
		Code, Poll, VerifyURL string
		ExpiresIn             int
	}
	json.NewDecoder(resp.Body).Decode(&start)
	resp.Body.Close()
	if start.Poll == "" {
		return "", &exitErr{exitError, fmt.Sprintf("login could not start (HTTP %d)", resp.StatusCode)}
	}
	fmt.Printf("Ask the user to open this link, sign in, and check the code matches:\n\n  %s\n  code: %s\n\nWaiting for approval (up to %d minutes)...\n", start.VerifyURL, start.Code, start.ExpiresIn/60)
	deadline := time.Now().Add(time.Duration(start.ExpiresIn) * time.Second)
	for time.Now().Before(deadline) {
		time.Sleep(3 * time.Second)
		r, err := hc.Get(api + "/v1/device/" + start.Poll)
		if err != nil {
			continue
		}
		var p struct{ State, Token, Error string }
		json.NewDecoder(r.Body).Decode(&p)
		r.Body.Close()
		switch {
		case r.StatusCode == 200 && p.Token != "":
			return p.Token, nil
		case r.StatusCode == 403:
			return "", &exitErr{exitError, "the user denied the login"}
		case r.StatusCode == 404:
			return "", &exitErr{exitError, "login code expired; run sb login again"}
		}
	}
	return "", &exitErr{exitError, "login code expired; run sb login again"}
}

// waitApproval polls one approval until a human decides.
func (c *client) waitApproval(id string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	fmt.Printf("waiting for a human to decide on %s (timeout %s)\n", id, timeout)
	for {
		b, _, err := c.call("GET", "/v1/approvals/"+url.PathEscape(id), nil)
		if err != nil {
			return err
		}
		var ap struct{ State, Result, Summary string }
		json.Unmarshal(b, &ap)
		switch ap.State {
		case "approved":
			fmt.Printf("approved: %s (%s)\n", ap.Summary, ap.Result)
			return nil
		case "denied", "obsolete":
			return &exitErr{exitError, fmt.Sprintf("%s: %s %s", ap.State, ap.Summary, ap.Result)}
		}
		if time.Now().After(deadline) {
			return &exitErr{exitApproval, "still pending after " + timeout.String() + "; ask the user to approve, then check again with: sb approvals --wait " + id}
		}
		time.Sleep(10 * time.Second)
	}
}

func commandHelp(cmd string) string {
	lines := strings.Split(userUsage, "\n")
	var out []string
	for i := 0; i < len(lines); i++ {
		t := strings.TrimSpace(lines[i])
		if strings.HasPrefix(t, "sb "+cmd+" ") || t == "sb "+cmd || strings.Contains(t, "| sb "+cmd+" ") {
			out = append(out, "  "+t)
			// continuation lines are indented deeper than command lines
			for j := i + 1; j < len(lines) && strings.HasPrefix(lines[j], "    ") && !strings.HasPrefix(strings.TrimSpace(lines[j]), "sb "); j++ {
				out = append(out, "    "+strings.TrimSpace(lines[j]))
				i = j
			}
		}
	}
	if len(out) == 0 {
		return "run `sb help` for all commands"
	}
	return "usage:\n" + strings.Join(out, "\n") + "\n\n" + exitCodesHelp
}

const exitCodesHelp = `exit codes: 0 ok; 1 error (including a failed build, a denied or obsolete approval, or sb check --contains not found);
2 usage; 3 waiting for human approval (also when sb approvals --wait times out)`

func handleExit(err error) int {
	if err == nil {
		return exitOK
	}
	var ee *exitErr
	if errors.As(err, &ee) {
		if ee.msg != "" {
			fmt.Fprintln(os.Stderr, ee.msg)
		}
		return ee.code
	}
	fmt.Fprintln(os.Stderr, "error:", err)
	return exitError
}

func needApp(pos []string, usage string) (string, error) {
	if len(pos) < 1 {
		return "", &exitErr{exitUsage, "usage: " + usage}
	}
	return pos[0], nil
}

// dashboardURL is the dashboard page for this CLI's install.
func (c *client) dashboardURL(page string) string {
	return strings.Replace(strings.TrimSuffix(c.cfg.API, "/"), "://api.", "://auth.", 1) + page
}

// humanOnly explains that a command moved to the dashboard, where humans sign in.
func (c *client) humanOnly(what, page string) error {
	return &exitErr{exitError, what + " is done by a human in the dashboard: " + c.dashboardURL(page) +
		"\nAgents: give the user this link. API tokens, including this one, cannot do it."}
}
