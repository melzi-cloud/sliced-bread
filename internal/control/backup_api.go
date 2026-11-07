package control

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"sort"
	"strings"

	"github.com/melzi-cloud/sliced-bread/internal/apps"
	"github.com/melzi-cloud/sliced-bread/internal/kube"
)

// BackupsView is GET /v1/backups.
type BackupsView struct {
	Configured bool              `json:"configured"`
	Target     string            `json:"target,omitempty"` // s3 endpoint/bucket/prefix, no keys
	Schedule   string            `json:"schedule,omitempty"`
	Apps       []AppBackupStatus `json:"apps"`
	Hint       string            `json:"hint,omitempty"`
}

// AppBackupStatus is one app's backups.
type AppBackupStatus struct {
	Name    string            `json:"name"`
	Summary string            `json:"summary"`
	Paused  bool              `json:"paused,omitempty"`
	State   *apps.BackupState `json:"state,omitempty"`
}

func (s *Server) apiBackups(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	c, err := s.backupConfig(ctx)
	if err != nil {
		fail(w, 500, err.Error(), "")
		return
	}
	v := BackupsView{Configured: c != nil, Apps: []AppBackupStatus{}}
	if c != nil {
		v.Target = strings.TrimPrefix(c.Repository(), "s3:")
		v.Schedule = "daily at " + twoDigit(c.Hour) + ":00 UTC, each backup followed by a restore test; keeps the last 10 snapshots plus 14 daily and 8 weekly"
	} else {
		v.Hint = "configure with: sb backups configure --endpoint https://... --bucket <bucket> (admin token)"
	}
	list, err := s.listApps(ctx)
	if err != nil {
		fail(w, 500, err.Error(), "")
		return
	}
	filter := r.URL.Query().Get("app")
	for _, a := range list {
		if !a.HasData() || (filter != "" && a.Name != filter) {
			continue
		}
		v.Apps = append(v.Apps, AppBackupStatus{Name: a.Name, Summary: backupSummary(a), Paused: a.Paused, State: a.Backup})
	}
	sort.Slice(v.Apps, func(i, j int) bool { return v.Apps[i].Name < v.Apps[j].Name })
	writeJSON(w, 200, v)
}

func twoDigit(n int) string {
	if n < 10 {
		return "0" + string(rune('0'+n))
	}
	return string(rune('0'+n/10)) + string(rune('0'+n%10))
}

func (s *Server) apiBackupConfig(w http.ResponseWriter, r *http.Request) {
	if scopeOf(r) != ScopeAdmin {
		fail(w, 403, "backups are configured by a human in the dashboard", "https://"+s.hosts().Auth()+"/settings")
		return
	}
	var c BackupConfig
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<16)).Decode(&c); err != nil {
		fail(w, 400, "bad JSON: "+err.Error(), "")
		return
	}
	generated, err := s.configureBackups(r.Context(), &c, r.URL.Query().Get("hour") != "")
	if err != nil {
		fail(w, 400, err.Error(), "")
		return
	}
	resp := map[string]any{"configured": true, "target": strings.TrimPrefix(c.Repository(), "s3:"), "hour": c.Hour}
	if generated {
		resp["password"] = c.Password
		resp["note"] = "This is the encryption password of your backups. Store it somewhere safe outside this install: without it the backups cannot be read. `sb backups password` shows it again."
	}
	writeJSON(w, 200, resp)
}

func (s *Server) apiBackupPassword(w http.ResponseWriter, r *http.Request) {
	if scopeOf(r) != ScopeAdmin {
		fail(w, 403, "the backup encryption password is shown to a human in the dashboard", "https://"+s.hosts().Auth()+"/settings")
		return
	}
	c, err := s.backupSecret(r.Context())
	if err != nil || c == nil {
		fail(w, 404, "backups are not configured", "")
		return
	}
	writeJSON(w, 200, map[string]any{"password": c.Password, "repository": c.Repository()})
}

func (s *Server) apiStartBackup(w http.ResponseWriter, r *http.Request) {
	s.startRunHTTP(w, r, "backup")
}

func (s *Server) apiStartRestoreTest(w http.ResponseWriter, r *http.Request) {
	s.startRunHTTP(w, r, "test")
}

func (s *Server) startRunHTTP(w http.ResponseWriter, r *http.Request, kind string) {
	var req struct {
		Snapshot string `json:"snapshot"`
	}
	json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<12)).Decode(&req)
	s.mu.Lock()
	defer s.mu.Unlock()
	a, err := s.getApp(r.Context(), r.PathValue("name"))
	if err != nil {
		fail(w, 404, "no app "+r.PathValue("name"), "")
		return
	}
	run, err := s.startBackupRun(r.Context(), a, kind, "manual", req.Snapshot)
	if err != nil {
		code := 400
		if errors.Is(err, errBackupBusy) {
			code = 409
		}
		fail(w, code, err.Error(), "see: sb backups "+a.Name)
		return
	}
	writeJSON(w, 202, map[string]any{"run": run, "hint": "follow with: sb backups " + a.Name + " (or add --wait)"})
}

// apiRestore replaces an app's data with a snapshot. Agents need human approval.
func (s *Server) apiRestore(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Snapshot string `json:"snapshot"`
	}
	json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<12)).Decode(&req)
	ctx := r.Context()
	a, err := s.getApp(ctx, r.PathValue("name"))
	if err != nil {
		fail(w, 404, "no app "+r.PathValue("name"), "")
		return
	}
	snap := req.Snapshot
	if snap == "" && a.Backup != nil && a.Backup.LastBackup != nil && a.Backup.LastBackup.Phase == "succeeded" {
		snap = a.Backup.LastBackup.Snapshot
	}
	if snap == "" {
		fail(w, 400, "no successful backup to restore", "run: sb backup "+a.Name)
		return
	}
	if scopeOf(r) != ScopeAdmin {
		ap, err := s.requestApproval(ctx, "restore", a.Name, "restore app "+a.Name+" from snapshot "+snap+" (replaces its current data)", nil, scopeOf(r))
		if err != nil {
			fail(w, 500, err.Error(), "")
			return
		}
		ap.Snapshot = snap
		s.saveApproval(ctx, ap)
		s.approvalResponse(w, ap)
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	run, err := s.startBackupRun(ctx, a, "restore", "manual", snap)
	if err != nil {
		fail(w, 409, err.Error(), "")
		return
	}
	writeJSON(w, 202, map[string]any{"run": run, "hint": "the app is stopped while its data is replaced; follow with: sb backups " + a.Name})
}

// apiExport returns every app's spec and its rendered Kubernetes objects.
func (s *Server) apiExport(w http.ResponseWriter, r *http.Request) {
	if scopeOf(r) != ScopeAdmin {
		fail(w, 403, "export is done by a human in the dashboard", "https://"+s.hosts().Auth()+"/settings")
		return
	}
	out, err := s.exportData(r.Context(), r.URL.Query().Get("secrets") == "true")
	if err != nil {
		fail(w, 500, err.Error(), "")
		return
	}
	writeJSON(w, 200, out)
}

type exported struct {
	Spec      *apps.App `json:"spec"`
	Manifests kube.Obj  `json:"manifests"`
}

// exportData collects every app's spec and rendered Kubernetes objects.
func (s *Server) exportData(ctx context.Context, withSecrets bool) (map[string]any, error) {
	list, err := s.listApps(ctx)
	if err != nil {
		return nil, err
	}
	all := map[string]exported{}
	for _, a := range list {
		objs, err := apps.Render(a, s.params(a, ""))
		if err != nil {
			return nil, fmt.Errorf("render %s: %w", a.Name, err)
		}
		items := make([]any, 0, len(objs)+1)
		for _, o := range objs {
			items = append(items, o)
		}
		if vals, err := s.appSecret(ctx, a); err == nil && len(vals) > 0 {
			data := map[string]any{}
			for k, v := range vals {
				if withSecrets {
					data[k] = v
				} else {
					data[k] = "REDACTED"
				}
			}
			items = append(items, kube.Obj{"apiVersion": "v1", "kind": "Secret",
				"metadata": kube.Obj{"name": apps.AppSecret, "namespace": apps.Namespace(a.Name)}, "stringData": data})
		}
		spec := *a
		spec.Backup, spec.Build, spec.Hooks = nil, nil, nil
		all[a.Name] = exported{Spec: &spec, Manifests: kube.Obj{"apiVersion": "v1", "kind": "List", "items": items}}
	}
	out := map[string]any{"domain": s.cfg.Domain, "apps": all}
	if c, _ := s.backupConfig(ctx); c != nil {
		out["backups"] = map[string]any{"repository": c.Repository(), "region": c.Region, "note": "restic repository; the encryption password was printed by sb init and is shown in the dashboard settings"}
	}
	return out, nil
}

// configureBackups validates and stores the backup target. The encryption password is the
// one installed by sb init (or kept from an earlier configuration); a new one is generated
// only if none exists. Returns whether it was generated here.
func (s *Server) configureBackups(ctx context.Context, c *BackupConfig, hourSet bool) (bool, error) {
	if c.Prefix == "" {
		c.Prefix = "slicedbread/" + s.cfg.Domain
	}
	if c.Region == "" {
		c.Region = "auto"
	}
	if c.Hour == 0 && !hourSet {
		c.Hour = 3
	}
	if err := c.validate(); err != nil {
		return false, err
	}
	prev, err := s.backupSecret(ctx)
	if err != nil {
		return false, err
	}
	generated := false
	if c.Password == "" {
		if prev != nil && prev.Password != "" {
			c.Password = prev.Password
		} else {
			c.Password, generated = newResticPassword(), true
		}
	}
	return generated, s.saveBackupConfig(ctx, *c)
}
