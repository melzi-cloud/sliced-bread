package main

import (
	"bufio"
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
    common flags: --env KEY=VALUE (repeatable), --secrets-stdin (KEY=VALUE lines on stdin),
                  --database (Postgres; sets DATABASE_URL), --health-path /healthz,
                  --access wall|app-auth|public (default wall), --public-path /path (repeatable)
  sb wait <app> [--timeout 10m]       block until the latest version is serving and ready (exit 1 if its build failed)

Operate
  sb logs <app> [--build] [--tail N]
  sb env <app>                        list env keys (values of secrets are never shown)
  sb env set <app> KEY=VALUE... [--secrets-stdin]
  sb env unset <app> KEY...
  sb settings <app> key=value...      template settings, e.g. vaultwarden signups=true
  sb access <app> wall|app-auth|public [--public-path /path]...
  sb action <app> <action> [--input key=value]...
  sb redeploy <app> [--dir <path>]    rebuild (upload new source with --dir), or restart image apps
  sb delete <app>

Approvals (privileged changes requested by agents)
  sb approvals [--wait <id>] [--timeout 30m]   list, or block until a human decides
  sb approve <id> | sb deny <id>      humans only (admin token); agents cannot approve

Output is human-readable; add --json to any command for JSON. Config: $SB_CONFIG or ~/.config/slicedbread/config.json.
Exit codes: 0 ok, 1 error, 2 usage, 3 waiting for human approval.`

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

func readSecretsStdin() (map[string]string, error) {
	out := map[string]string{}
	sc := bufio.NewScanner(os.Stdin)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		k, v, ok := strings.Cut(line, "=")
		if !ok {
			return nil, &exitErr{exitUsage, "--secrets-stdin expects KEY=VALUE lines"}
		}
		out[strings.TrimSpace(k)] = v
	}
	return out, sc.Err()
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

	needApp := func(pos []string, usage string) (string, error) {
		if len(pos) < 1 {
			return "", &exitErr{exitUsage, "usage: " + usage}
		}
		return pos[0], nil
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
				Name, Description, Access, Notes string
				Settings                         []struct {
					Name, Description, Default string
					Values, ApprovalFor        []string
				}
				Actions []struct {
					Name, Description string
					Input             map[string]string
					AdminOnly         bool
				}
			}
		}
		json.Unmarshal(b, &cat)
		for _, t := range cat.Templates {
			fmt.Printf("%s: %s\n  access: %s\n", t.Name, t.Description, t.Access)
			for _, st := range t.Settings {
				vals := strings.Join(st.Values, "|")
				if vals == "" {
					vals = "free text"
				}
				def := st.Default
				if def == "" {
					def = "empty"
				}
				fmt.Printf("  setting %s (values: %s; default: %s)", st.Name, vals, def)
				if len(st.ApprovalFor) > 0 {
					fmt.Printf(" [%s needs human approval]", strings.Join(st.ApprovalFor, ","))
				}
				fmt.Printf(": %s\n", st.Description)
			}
			for _, ac := range t.Actions {
				var ins []string
				for k, v := range ac.Input {
					ins = append(ins, "--input "+k+"=<"+v+">")
				}
				sort.Strings(ins)
				admin := ""
				if ac.AdminOnly {
					admin = " [admin token only: ask the user to run it]"
				}
				fmt.Printf("  action %s %s: %s%s\n", ac.Name, strings.Join(ins, " "), ac.Description, admin)
			}
			fmt.Printf("  notes: %s\n\n", t.Notes)
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
		secretsStdin := fs.Bool("secrets-stdin", false, "")
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
		if *secretsStdin {
			sec, err := readSecretsStdin()
			if err != nil {
				return err
			}
			req["secrets"] = sec
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
		b, _, err := c.call("GET", "/v1/apps/"+url.PathEscape(app)+"/logs"+q, nil)
		if err != nil {
			return err
		}
		os.Stdout.Write(b)
		return nil

	case "env":
		secretsStdin := fs.Bool("secrets-stdin", false, "")
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
			}
			json.Unmarshal(b, &st)
			if jsonMode {
				out, _ := json.Marshal(map[string]any{"env": st.Env, "secretKeys": st.SecretKeys})
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
		if len(pos) < 2 || (pos[0] != "set" && pos[0] != "unset") {
			return &exitErr{exitUsage, "usage: sb env <app> | sb env set <app> KEY=VALUE... [--secrets-stdin] | sb env unset <app> KEY..."}
		}
		app := pos[1]
		req := map[string]any{}
		if pos[0] == "set" {
			set, err := kvs(pos[2:], "env")
			if err != nil {
				return err
			}
			if len(set) > 0 {
				req["set"] = set
			}
			if *secretsStdin {
				sec, err := readSecretsStdin()
				if err != nil {
					return err
				}
				req["secrets"] = sec
			}
			if len(req) == 0 {
				return &exitErr{exitUsage, "nothing to set"}
			}
		} else {
			if len(pos) < 3 {
				return &exitErr{exitUsage, "usage: sb env unset <app> KEY..."}
			}
			req["unset"] = pos[2:]
		}
		b, _, err := c.call("PATCH", "/v1/apps/"+url.PathEscape(app)+"/env", req)
		if err != nil {
			return err
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
		b, _, err := c.call("POST", "/v1/apps/"+url.PathEscape(pos[0])+"/actions/"+url.PathEscape(pos[1]), map[string]any{"input": in})
		if err != nil {
			return err
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
		for _, a := range al.Approvals {
			sum := a.Summary
			if a.Result != "" && a.State != "pending" {
				sum += " (" + a.Result + ")"
			}
			fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\n", a.ID, a.State, a.App, a.RequestedAt, sum)
		}
		tw.Flush()
		fmt.Println("approve at:", al.ApproveURL)
		return nil

	case "approve", "deny":
		pos, err := parse(fs, rest)
		if err != nil {
			return err
		}
		if len(pos) < 1 {
			return &exitErr{exitUsage, "usage: sb " + cmd + " <approval-id>"}
		}
		b, _, err := c.call("POST", "/v1/approvals/"+url.PathEscape(pos[0])+"/"+cmd, nil)
		if err != nil {
			return err
		}
		c.print(b)
		return nil
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
	if acts, ok := st["actions"].([]any); ok && len(acts) > 0 {
		for _, a := range acts {
			m, _ := a.(map[string]any)
			admin := ""
			if m["adminOnly"] == true {
				admin = " [admin token only]"
			}
			fmt.Printf("action: %v: %v%s\n", m["name"], m["description"], admin)
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
	for _, line := range strings.Split(userUsage, "\n") {
		t := strings.TrimSpace(line)
		if strings.HasPrefix(t, "sb "+cmd+" ") || t == "sb "+cmd || strings.HasPrefix(t, "sb "+cmd+"\t") {
			return "usage: " + t
		}
	}
	return "run `sb help` for all commands"
}

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
