package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/melzi-cloud/sliced-bread/internal/apps"
)

// runBackupCmd handles backups, backup, restore, restore-test and export. ok=false: not ours.
func (c *client) runBackupCmd(cmd string, fs *flag.FlagSet, rest []string, jsonOut *bool) (bool, error) {
	switch cmd {
	case "backups":
		if len(rest) > 0 && rest[0] == "configure" {
			return true, c.humanOnly("Configuring backups", "/settings")
		}
		if len(rest) > 0 && rest[0] == "password" {
			return true, c.humanOnly("Showing the backup encryption password", "/settings")
		}
		repo := fs.Bool("repository", false, "")
		pos, err := parse(fs, rest)
		if err != nil {
			return true, err
		}
		if *repo {
			return true, c.listRepository(*jsonOut)
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
			fmt.Println("backups: not configured (a human sets up the bucket in the dashboard: Settings)")
			fmt.Println(v.Hint)
			return true, nil
		}
		fmt.Println("target:  ", v.Target)
		fmt.Println("schedule:", v.Schedule)
		fmt.Println("encryption password: held by the user and shown only in the dashboard (Settings); agents never see it")
		for _, a := range v.Apps {
			fmt.Printf("%s: %s\n", a.Name, a.Summary)
			if len(pos) == 1 && a.State != nil {
				for _, h := range a.State.History {
					fmt.Printf("  snapshot %s  %s  %d files  %d db tables  %d KB\n", h.Snapshot, h.StartedAt, h.Files, h.DBTables, h.Bytes/1024)
				}
			}
		}
		if len(v.Apps) == 0 {
			if len(pos) == 1 {
				fmt.Printf("%s has no volume or database, so there is nothing to back up (or it does not exist: see sb apps)\n", pos[0])
			} else {
				fmt.Println("(no apps with volumes or databases)")
			}
		}
		return true, nil

	case "backup", "restore", "restore-test":
		snapshot := fs.String("snapshot", "", "")
		wait := fs.Bool("wait", false, "")
		timeout := fs.Duration("timeout", 30*time.Minute, "")
		recreate := fs.Bool("recreate", false, "")
		all := fs.Bool("all", false, "")
		pos, err := parse(fs, rest)
		if err != nil {
			return true, err
		}
		if cmd == "restore" && *all {
			return true, c.humanOnly("Restoring every app from the backup repository", "/settings")
		}
		if cmd == "restore" && *recreate {
			return true, c.recreate(pos, *snapshot, *all, *wait, *timeout, *jsonOut)
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
		return true, c.waitBackups(app, cmd, *timeout)

	case "export":
		return true, c.humanOnly("Export", "/settings")
	}
	return false, nil
}

func (c *client) waitBackups(app, cmd string, timeout time.Duration) error {
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
					Running     *struct{ Kind string } `json:"running"`
					LastBackup  *runView               `json:"lastBackup"`
					LastTest    *runView               `json:"lastTest"`
					LastRestore *runView               `json:"lastRestore"`
				} `json:"state"`
			} `json:"apps"`
		}
		json.Unmarshal(b, &v)
		if len(v.Apps) == 1 && v.Apps[0].State != nil && v.Apps[0].State.Running == nil {
			st := v.Apps[0].State
			check := func(label string, r *runView) error {
				if r == nil {
					return nil
				}
				fmt.Println(r.line(label))
				if r.Phase == "failed" {
					return &exitErr{exitError, label + " failed: " + r.Message}
				}
				return nil
			}
			switch cmd {
			case "restore":
				return check("restore", st.LastRestore)
			case "restore-test":
				return check("restore test", st.LastTest)
			}
			if err := check("backup", st.LastBackup); err != nil {
				return err
			}
			return check("restore test", st.LastTest)
		}
		time.Sleep(5 * time.Second)
	}
	return &exitErr{exitError, "timed out; check: sb backups " + app}
}

type runView struct {
	ID, Phase, Message, Snapshot, FinishedAt string
	Files, DBTables                          int
	DBRows                                   int64
	SQLite                                   []apps.SQLiteResult
}

func (r runView) line(label string) string {
	snap := r.Snapshot
	if len(snap) > 12 {
		snap = snap[:12]
	}
	s := fmt.Sprintf("%s %s: %s, snapshot %s", label, r.ID, r.Phase, snap)
	if r.Phase == "succeeded" {
		s += fmt.Sprintf(", %d files", r.Files)
		if r.DBTables > 0 {
			s += fmt.Sprintf(", %d db tables, %d rows", r.DBTables, r.DBRows)
		}
	}
	if len(r.SQLite) > 0 { // shown on failure too: which file failed and how
		s += ", " + apps.SQLiteSummary(r.SQLite)
	}
	return s
}

// slow is a client for calls that wait for jobs inside the cluster.
func (c *client) slow() *client {
	cc := *c
	h := *c.http
	h.Timeout = 20 * time.Minute
	cc.http = &h
	return &cc
}

func (c *client) listRepository(jsonOut bool) error {
	fmt.Fprintln(os.Stderr, "reading the backup repository (takes a few seconds)")
	b, _, err := c.slow().call("GET", "/v1/backups/repository", nil)
	if err != nil {
		return err
	}
	if jsonOut {
		c.print(b)
		return nil
	}
	var v struct {
		Apps []struct {
			Name, Latest, LatestAt string
			Snapshots              int
			Exists                 bool
		} `json:"apps"`
	}
	json.Unmarshal(b, &v)
	if len(v.Apps) == 0 {
		fmt.Println("the repository holds no snapshots")
	}
	for _, a := range v.Apps {
		state := "deleted (bring it back with: sb restore " + a.Name + " --recreate)"
		if a.Exists {
			state = "deployed"
		}
		fmt.Printf("%s: %d snapshots, latest %s at %s; %s\n", a.Name, a.Snapshots, a.Latest[:12], a.LatestAt, state)
	}
	return nil
}

func (c *client) recreate(pos []string, snapshot string, all, wait bool, timeout time.Duration, jsonOut bool) error {
	body := map[string]any{"snapshot": snapshot, "all": all}
	if !all {
		if len(pos) != 1 {
			return &exitErr{exitUsage, "usage: sb restore <app> --recreate [--snapshot ID] [--wait] | sb restore --all [--wait]"}
		}
		body["apps"] = []string{pos[0]}
	}
	fmt.Fprintln(os.Stderr, "reading the backup repository and recreating (about a minute)")
	b, _, err := c.slow().call("POST", "/v1/recreate", body)
	if err != nil {
		return err
	}
	var m map[string]any
	json.Unmarshal(b, &m)
	if _, ok := m["approval"]; ok {
		return c.approvalExit(b)
	}
	if jsonOut && !wait {
		c.print(b)
		return nil
	}
	var v struct {
		Recreated []string `json:"recreated"`
		Note      string   `json:"note"`
	}
	json.Unmarshal(b, &v)
	if v.Note != "" {
		fmt.Println(v.Note)
	}
	for _, r := range v.Recreated {
		fmt.Println("recreated", r)
	}
	if !wait {
		fmt.Println("each app starts after its data is restored; follow with: sb backups, then sb wait <app>")
		return nil
	}
	for _, r := range v.Recreated {
		app := strings.Fields(r)[0]
		if err := c.waitBackups(app, "restore", timeout); err != nil {
			return err
		}
		if err := c.wait(app, timeout); err != nil {
			return err
		}
	}
	return nil
}
