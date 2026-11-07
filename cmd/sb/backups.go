package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// runBackupCmd handles backups, backup, restore, restore-test and export. ok=false: not ours.
func (c *client) runBackupCmd(cmd string, fs *flag.FlagSet, rest []string, jsonOut *bool) (bool, error) {
	switch cmd {
	case "backups":
		if len(rest) > 0 && rest[0] == "configure" {
			return true, c.backupsConfigure(fs, rest[1:], jsonOut)
		}
		if len(rest) > 0 && rest[0] == "password" {
			b, _, err := c.call("GET", "/v1/backups/password", nil)
			if err != nil {
				return true, err
			}
			c.print(b)
			return true, nil
		}
		pos, err := parse(fs, rest)
		if err != nil {
			return true, err
		}
		path := "/v1/backups"
		if len(pos) == 1 {
			path += "?app=" + url.QueryEscape(pos[0])
		}
		b, _, err := c.call("GET", path, nil)
		if err != nil {
			return true, err
		}
		if *jsonOut {
			c.print(b)
			return true, nil
		}
		var v struct {
			Configured bool   `json:"configured"`
			Target     string `json:"target"`
			Schedule   string `json:"schedule"`
			Hint       string `json:"hint"`
			Apps       []struct {
				Name    string `json:"name"`
				Summary string `json:"summary"`
				State   *struct {
					History []struct {
						Snapshot, StartedAt string
						Files, DBTables     int
						Bytes               int64
					} `json:"history"`
				} `json:"state"`
			} `json:"apps"`
		}
		json.Unmarshal(b, &v)
		if !v.Configured {
			fmt.Println("backups: not configured")
			fmt.Println(v.Hint)
			return true, nil
		}
		fmt.Println("target:  ", v.Target)
		fmt.Println("schedule:", v.Schedule)
		for _, a := range v.Apps {
			fmt.Printf("%s: %s\n", a.Name, a.Summary)
			if len(pos) == 1 && a.State != nil {
				for _, h := range a.State.History {
					fmt.Printf("  snapshot %s  %s  %d files  %d db tables  %d KB\n", h.Snapshot, h.StartedAt, h.Files, h.DBTables, h.Bytes/1024)
				}
			}
		}
		if len(v.Apps) == 0 {
			fmt.Println("(no apps with volumes or databases)")
		}
		return true, nil

	case "backup", "restore", "restore-test":
		snapshot := fs.String("snapshot", "", "")
		wait := fs.Bool("wait", false, "")
		timeout := fs.Duration("timeout", 30*time.Minute, "")
		pos, err := parse(fs, rest)
		if err != nil {
			return true, err
		}
		app, err := needApp(pos, "sb "+cmd+" <app>")
		if err != nil {
			return true, err
		}
		b, code, err := c.call("POST", "/v1/apps/"+url.PathEscape(app)+"/"+cmd, map[string]string{"snapshot": *snapshot})
		if err != nil {
			return true, err
		}
		_ = code
		var m map[string]any
		json.Unmarshal(b, &m)
		if _, ok := m["approval"]; ok { // restore by an agent needs a human
			return true, c.approvalExit(b)
		}
		if *jsonOut && !*wait {
			c.print(b)
			return true, nil
		}
		var r struct {
			Run struct{ ID, Kind, Snapshot string } `json:"run"`
		}
		json.Unmarshal(b, &r)
		fmt.Printf("started %s %s for %s", r.Run.Kind, r.Run.ID, app)
		if r.Run.Snapshot != "" {
			fmt.Printf(" (snapshot %s)", r.Run.Snapshot)
		}
		fmt.Println()
		if !*wait {
			fmt.Println("follow with: sb backups", app)
			return true, nil
		}
		return true, c.waitBackups(app, *timeout)

	case "export":
		secrets := fs.Bool("include-secrets", false, "")
		pos, err := parse(fs, rest)
		if err != nil {
			return true, err
		}
		if len(pos) != 1 {
			return true, &exitErr{exitUsage, "usage: sb export <directory> [--include-secrets]"}
		}
		path := "/v1/export"
		if *secrets {
			path += "?secrets=true"
		}
		b, _, err := c.call("GET", path, nil)
		if err != nil {
			return true, err
		}
		var v struct {
			Domain  string                     `json:"domain"`
			Apps    map[string]json.RawMessage `json:"apps"`
			Backups json.RawMessage            `json:"backups"`
		}
		if err := json.Unmarshal(b, &v); err != nil {
			return true, err
		}
		dir := pos[0]
		for name, raw := range v.Apps {
			var e struct {
				Spec      json.RawMessage `json:"spec"`
				Manifests json.RawMessage `json:"manifests"`
			}
			json.Unmarshal(raw, &e)
			d := filepath.Join(dir, "apps", name)
			if err := os.MkdirAll(d, 0o700); err != nil {
				return true, err
			}
			if err := writeIndented(filepath.Join(d, "slicedbread-app.json"), e.Spec); err != nil {
				return true, err
			}
			if err := writeIndented(filepath.Join(d, "kubernetes.json"), e.Manifests); err != nil {
				return true, err
			}
		}
		if len(v.Backups) > 0 {
			writeIndented(filepath.Join(dir, "backups.json"), v.Backups)
		}
		readme := fmt.Sprintf(`Export of the SlicedBread install %s, %s.

apps/<name>/slicedbread-app.json  the app as SlicedBread stores it
apps/<name>/kubernetes.json       the Kubernetes objects SlicedBread applies for it (a v1 List):
                                  kubectl apply -f apps/<name>/kubernetes.json on any cluster with
                                  Traefik and CloudNativePG; the login wall middlewares need sb-control.
Secret values are %s.
App data lives in the backup repository (backups.json) and is read with restic and the
password from: sb backups password
`, v.Domain, time.Now().UTC().Format(time.RFC3339), map[bool]string{true: "included: keep this directory private", false: "redacted (use --include-secrets to include them)"}[*secrets])
		os.WriteFile(filepath.Join(dir, "README.txt"), []byte(readme), 0o600)
		fmt.Printf("exported %d apps to %s\n", len(v.Apps), dir)
		return true, nil
	}
	return false, nil
}

func writeIndented(path string, raw json.RawMessage) error {
	var v any
	if err := json.Unmarshal(raw, &v); err != nil {
		return err
	}
	b, _ := json.MarshalIndent(v, "", "  ")
	return os.WriteFile(path, append(b, '\n'), 0o600)
}

func (c *client) waitBackups(app string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		b, _, err := c.call("GET", "/v1/backups?app="+url.QueryEscape(app), nil)
		if err != nil {
			return err
		}
		var v struct {
			Apps []struct {
				Summary string `json:"summary"`
				State   *struct {
					Running     *struct{ Kind string }           `json:"running"`
					LastBackup  *struct{ Phase, Message string } `json:"lastBackup"`
					LastTest    *struct{ Phase, Message string } `json:"lastTest"`
					LastRestore *struct{ Phase, Message string } `json:"lastRestore"`
				} `json:"state"`
			} `json:"apps"`
		}
		json.Unmarshal(b, &v)
		if len(v.Apps) == 1 && v.Apps[0].State != nil && v.Apps[0].State.Running == nil {
			st := v.Apps[0].State
			fmt.Println(v.Apps[0].Summary)
			if st.LastRestore != nil && st.LastRestore.Phase == "failed" {
				return &exitErr{exitError, "restore failed: " + st.LastRestore.Message}
			}
			for _, r := range []*struct{ Phase, Message string }{st.LastBackup, st.LastTest} {
				if r != nil && r.Phase == "failed" {
					return &exitErr{exitError, "failed: " + r.Message}
				}
			}
			return nil
		}
		time.Sleep(5 * time.Second)
	}
	return &exitErr{exitError, "timed out; check: sb backups " + app}
}

func (c *client) backupsConfigure(fs *flag.FlagSet, args []string, jsonOut *bool) error {
	endpoint := fs.String("endpoint", "", "")
	bucket := fs.String("bucket", "", "")
	region := fs.String("region", "auto", "")
	prefix := fs.String("prefix", "", "")
	hour := fs.Int("hour", 3, "")
	if _, err := parse(fs, args); err != nil {
		return err
	}
	if *endpoint == "" || *bucket == "" {
		return &exitErr{exitUsage, backupsConfigureUsage}
	}
	key, err := secretInput("S3 access key id", "SB_BACKUP_ACCESS_KEY_ID")
	if err != nil {
		return err
	}
	sec, err := secretInput("S3 secret access key", "SB_BACKUP_SECRET_ACCESS_KEY")
	if err != nil {
		return err
	}
	body := map[string]any{"endpoint": *endpoint, "bucket": *bucket, "region": *region, "prefix": *prefix, "hour": *hour,
		"accessKeyId": key, "secretAccessKey": sec, "password": strings.TrimSpace(os.Getenv("SB_BACKUP_PASSWORD"))}
	b, _, err := c.call("PUT", "/v1/backups/config?hour="+fmt.Sprint(*hour), body)
	if err != nil {
		return err
	}
	if *jsonOut {
		c.print(b)
		return nil
	}
	var r map[string]any
	json.Unmarshal(b, &r)
	fmt.Println("backups configured:", r["target"])
	if p, ok := r["password"].(string); ok {
		fmt.Println("encryption password (store it outside this install; without it backups cannot be read):")
		fmt.Println(p)
	}
	return nil
}

const backupsConfigureUsage = `usage: sb backups configure --endpoint https://<s3 endpoint> --bucket <bucket> [--region auto] [--prefix path] [--hour 3]
Keys come from SB_BACKUP_ACCESS_KEY_ID and SB_BACKUP_SECRET_ACCESS_KEY (or a prompt). To reuse an existing
backup repository set SB_BACKUP_PASSWORD. Admin token only.`
