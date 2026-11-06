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
  sb login --api https://api.<tenant-domain>    (reads the API token from stdin)
  sb whoami

Discover
  sb catalog                          templates, their settings and actions
  sb apps                             all apps with state and URL
  sb status <app>                     details, env keys, actions, notes

Deploy (exactly one source)
  sb deploy <app> --template <name> [--set key=value]...
  sb deploy <app> --image <image> [--port N]
  sb deploy <app> --dir <local-path> [--port N]     (uploads source; built with Railpack or its Dockerfile)
  sb deploy <app> --repo <https-git-url> [--branch B] [--path dir] [--port N]   (public repos)
    common flags: --env KEY=VALUE (repeatable), --secrets-stdin (KEY=VALUE lines on stdin),
                  --access wall|app-auth|public (default wall), --public-path /path (repeatable)
  sb wait <app> [--timeout 10m]       block until running (or failed)

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
  sb approvals
  sb approve <id> | sb deny <id>      admin token only

Global: --json prints raw JSON. Config: $SB_CONFIG or ~/.config/slicedbread/config.json.
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
	if c.json {
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

// parse splits positional arguments from flags so flags may come after positionals.
func parse(fs *flag.FlagSet, args []string) ([]string, error) {
	var pos []string
	for len(args) > 0 {
		if err := fs.Parse(args); err != nil {
			return nil, &exitErr{exitUsage, err.Error()}
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
	fs := flag.NewFlagSet(cmd, flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	jsonOut := fs.Bool("json", false, "")

	if cmd == "login" {
		api := fs.String("api", "", "")
		if _, err := parse(fs, rest); err != nil {
			return err
		}
		if *api == "" {
			return &exitErr{exitUsage, "usage: echo <token> | sb login --api https://api.<tenant-domain>"}
		}
		tok, _ := io.ReadAll(io.LimitReader(os.Stdin, 4096))
		c := cliConfig{API: strings.TrimSuffix(*api, "/"), Token: strings.TrimSpace(string(tok))}
		if c.Token == "" {
			return &exitErr{exitUsage, "pipe the API token on stdin"}
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
		c.print(b)
		return nil

	case "catalog":
		if _, err := parse(fs, rest); err != nil {
			return err
		}
		b, _, err := c.call("GET", "/v1/catalog", nil)
		if err != nil {
			return err
		}
		c.print(b)
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
		c.print(b)
		return nil

	case "deploy":
		var env, pubs, sets multi
		tmpl := fs.String("template", "", "")
		image := fs.String("image", "", "")
		repo := fs.String("repo", "", "")
		dir := fs.String("dir", "", "")
		branch := fs.String("branch", "", "")
		path := fs.String("path", "", "")
		port := fs.Int("port", 0, "")
		access := fs.String("access", "", "")
		secretsStdin := fs.Bool("secrets-stdin", false, "")
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
		tail := fs.Int("tail", 100, "")
		pos, err := parse(fs, rest)
		if err != nil {
			return err
		}
		app, err := needApp(pos, "sb logs <app> [--build] [--tail N]")
		if err != nil {
			return err
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
				EnvKeys    []string `json:"envKeys"`
				SecretKeys []string `json:"secretKeys"`
			}
			json.Unmarshal(b, &st)
			for _, k := range st.EnvKeys {
				fmt.Println(k)
			}
			for _, k := range st.SecretKeys {
				fmt.Println(k, "(secret)")
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
		c.print(b)
		return nil

	case "redeploy":
		dir := fs.String("dir", "", "")
		pos, err := parse(fs, rest)
		if err != nil {
			return err
		}
		app, err := needApp(pos, "sb redeploy <app> [--dir <path>]")
		if err != nil {
			return err
		}
		var body any
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
		if _, err := parse(fs, rest); err != nil {
			return err
		}
		b, _, err := c.call("GET", "/v1/approvals", nil)
		if err != nil {
			return err
		}
		c.print(b)
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

// printAppResult prints an app status response in a compact form.
func (c *client) printAppResult(b []byte) error {
	if c.json {
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
	keys := []string{"name", "url", "state", "message", "access", "source"}
	for _, k := range keys {
		if v, ok := st[k]; ok && v != "" {
			fmt.Printf("%s: %v\n", k, v)
		}
	}
	var extra []string
	for k := range st {
		extra = append(extra, k)
	}
	sort.Strings(extra)
	_ = extra
}

// wait polls an app until it is running, failed, or the timeout passes.
func (c *client) wait(app string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	last := ""
	for {
		b, _, err := c.call("GET", "/v1/apps/"+url.PathEscape(app), nil)
		if err != nil {
			return err
		}
		var st struct {
			State, Message, URL string
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
			fmt.Println("ready:", st.URL)
			return nil
		case "build-failed":
			return &exitErr{exitError, "build failed; read the logs with: sb logs " + app + " --build"}
		}
		if time.Now().After(deadline) {
			return &exitErr{exitError, fmt.Sprintf("timed out after %s (state %s); check: sb status %s and sb logs %s", timeout, st.State, app, app)}
		}
		time.Sleep(5 * time.Second)
	}
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
