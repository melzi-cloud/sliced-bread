package control

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/melzi-cloud/sliced-bread/internal/apps"
	"github.com/melzi-cloud/sliced-bread/internal/kube"
)

// Every backup carries the app's spec, its secret values and (for apps deployed from a
// local directory) the uploaded source in /sb/meta. That makes a backup enough to bring
// an app back after it was deleted, or to rebuild a whole install from its bucket.

const backupMetaSecret = "sb-backup-meta"

// metaSpec is the part of an app that a backup should recreate.
func metaSpec(a *apps.App) *apps.App {
	m := *a
	m.Backup, m.Build, m.Paused, m.Generation = nil, nil, false, 0
	return &m
}

// writeBackupMeta places the spec and secret values where the backup job copies them from.
func (s *Server) writeBackupMeta(ctx context.Context, a *apps.App) error {
	spec, _ := json.Marshal(metaSpec(a))
	vals, err := s.appSecret(ctx, a)
	if err != nil {
		return err
	}
	env, _ := json.Marshal(vals)
	return s.kube.Apply(ctx, kube.Obj{
		"apiVersion": "v1", "kind": "Secret",
		"metadata":   kube.Obj{"name": backupMetaSecret, "namespace": apps.Namespace(a.Name), "labels": kube.Obj{apps.ManagedByLabel: "slicedbread"}},
		"type":       "Opaque",
		"stringData": kube.Obj{"app.json": string(spec), "env.json": string(env)},
	})
}

// --- reading the repository ---

// RepoSnapshot is one snapshot in the backup repository.
type RepoSnapshot struct {
	ID   string    `json:"id"`
	App  string    `json:"app"`
	Time time.Time `json:"time"`
}

// RepoApp summarises one app found in the repository.
type RepoApp struct {
	Name      string `json:"name"`
	Snapshots int    `json:"snapshots"`
	Latest    string `json:"latest"`
	LatestAt  string `json:"latestAt"`
	Exists    bool   `json:"exists"` // an app with this name is deployed now
}

func parseSnapshots(raw []byte) ([]RepoSnapshot, error) {
	var list []struct {
		ID   string    `json:"id"`
		Time time.Time `json:"time"`
		Tags []string  `json:"tags"`
	}
	if err := json.Unmarshal(raw, &list); err != nil {
		return nil, err
	}
	var out []RepoSnapshot
	for _, s := range list {
		for _, t := range s.Tags {
			if app, ok := strings.CutPrefix(t, "app="); ok {
				out = append(out, RepoSnapshot{ID: s.ID, App: app, Time: s.Time})
			}
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Time.After(out[j].Time) })
	return out, nil
}

// runJob applies a job, waits for it, and returns its SB_ output lines.
func (s *Server) runJob(ctx context.Context, job kube.Obj, timeout time.Duration) ([]string, error) {
	ns, name := kube.Str(job, "metadata", "namespace"), kube.Str(job, "metadata", "name")
	if err := s.kube.Apply(ctx, job); err != nil {
		return nil, err
	}
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		var j kube.Obj
		if err := s.kube.Get(ctx, "batch/v1", "Job", ns, name, &j); err == nil {
			ok, bad := kube.Num(j, "status", "succeeded") > 0, kube.Num(j, "status", "failed") > 0
			if ok || bad {
				lines := s.jobLines(ctx, ns, name)
				if bad {
					msg := "job " + name + " failed"
					for _, l := range lines {
						if m, found := strings.CutPrefix(l, "SB_ERROR "); found {
							msg = m
						}
					}
					return lines, errors.New(msg)
				}
				return lines, nil
			}
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(2 * time.Second):
		}
	}
	return nil, fmt.Errorf("job %s did not finish in %s", name, timeout)
}

func (s *Server) jobLines(ctx context.Context, ns, job string) []string {
	pods, err := s.kube.List(ctx, "v1", "Pod", ns, "job-name="+job)
	if err != nil || len(pods) == 0 {
		return nil
	}
	l, _ := s.kube.Logs(ctx, ns, kube.Str(pods[0], "metadata", "name"), "restic", 5000)
	var out []string
	for _, line := range strings.Split(l, "\n") {
		if strings.HasPrefix(line, "SB_") {
			out = append(out, strings.TrimSpace(line))
		}
	}
	return out
}

func repoJob(name, script string, env []any) kube.Obj {
	c := container("restic", resticImage, script, env, nil, 0)
	c["envFrom"] = []any{envFromSecret()}
	return kube.Obj{
		"apiVersion": "batch/v1", "kind": "Job",
		"metadata": kube.Obj{"name": name, "namespace": restoreTestNS, "labels": kube.Obj{apps.ManagedByLabel: "slicedbread"}},
		"spec": kube.Obj{
			"backoffLimit": 0, "activeDeadlineSeconds": 900, "ttlSecondsAfterFinished": 3600,
			"template": kube.Obj{"spec": kube.Obj{
				"restartPolicy": "Never", "automountServiceAccountToken": false,
				"containers": []any{c},
			}},
		},
	}
}

func (s *Server) prepareRepoNamespace(ctx context.Context) (*BackupConfig, error) {
	c, err := s.backupConfig(ctx)
	if err != nil {
		return nil, err
	}
	if c == nil {
		return nil, errors.New("backups are not configured; run: sb backups configure (with SB_BACKUP_PASSWORD to read an existing repository)")
	}
	if err := s.kube.Apply(ctx, kube.Obj{"apiVersion": "v1", "kind": "Namespace", "metadata": kube.Obj{"name": restoreTestNS,
		"labels": kube.Obj{"pod-security.kubernetes.io/enforce": "baseline", apps.ManagedByLabel: "slicedbread"}}}); err != nil {
		return nil, err
	}
	return c, s.envSecret(ctx, c, restoreTestNS)
}

// repoSnapshots lists every snapshot in the backup repository.
func (s *Server) repoSnapshots(ctx context.Context) ([]RepoSnapshot, error) {
	if _, err := s.prepareRepoNamespace(ctx); err != nil {
		return nil, err
	}
	script := `set -eu
` + resticFn + `r snapshots --json --host slicedbread > /tmp/s.json 2>/tmp/err || { tail -2 /tmp/err; echo "SB_ERROR cannot read the backup repository (wrong password or bucket?)"; exit 1; }
echo "SB_SNAPSHOTS $(base64 -w0 /tmp/s.json)"
`
	lines, err := s.runJob(ctx, repoJob("catalog-"+randID(6), script, nil), 5*time.Minute)
	if err != nil {
		return nil, err
	}
	for _, l := range lines {
		if b64, ok := strings.CutPrefix(l, "SB_SNAPSHOTS "); ok {
			raw, err := base64.StdEncoding.DecodeString(b64)
			if err != nil {
				return nil, err
			}
			return parseSnapshots(raw)
		}
	}
	return nil, errors.New("the catalog job printed no snapshot list")
}

// restoredMeta is what a snapshot says about its app.
type restoredMeta struct {
	Spec   *apps.App
	Env    map[string]string
	Upload string // id of the restored source upload, if any
}

func (s *Server) sourceSig(app string) string {
	m := hmac.New(sha256.New, append([]byte("restore-source:"), s.auth.key...))
	m.Write([]byte(app))
	return hex.EncodeToString(m.Sum(nil))
}

// fetchMeta reads the spec, secrets and source of one snapshot per app.
func (s *Server) fetchMeta(ctx context.Context, picks map[string]string) (map[string]*restoredMeta, error) {
	if _, err := s.prepareRepoNamespace(ctx); err != nil {
		return nil, err
	}
	var items []string
	for app, snap := range picks {
		items = append(items, app+":"+snap+":"+s.sourceSig(app))
	}
	sort.Strings(items)
	script := `set -eu
` + resticFn + `for item in $SB_ITEMS; do
  app=${item%%:*}; rest=${item#*:}; snap=${rest%%:*}; sig=${rest#*:}
  if ! r dump "$snap" /sb/meta/app.json > /tmp/app.json 2>/dev/null; then
    echo "SB_ERROR snapshot $snap of $app has no app spec (taken before backups carried one); restore it with sb restore into an existing app"; exit 1
  fi
  echo "SB_APPJSON $app $(base64 -w0 /tmp/app.json)"
  r dump "$snap" /sb/meta/env.json > /tmp/env.json 2>/dev/null || echo '{}' > /tmp/env.json
  echo "SB_ENVJSON $app $(base64 -w0 /tmp/env.json)"
  if r dump "$snap" /sb/meta/source.tar.gz > /tmp/src.tgz 2>/dev/null && [ -s /tmp/src.tgz ]; then
    # busybox wget posts its body as a string (it stops at the first zero byte), so send base64.
    base64 -w0 /tmp/src.tgz > /tmp/src.b64
    id=$(wget -qO- --header "Content-Type: text/plain" --post-file=/tmp/src.b64 "$SB_CONTROL/internal/restore-source/$app?sig=$sig&encoding=base64") || { echo "SB_ERROR could not hand the restored source of $app to sb-control"; exit 1; }
    echo "SB_SOURCE $app $id"
  fi
done
`
	env := []any{
		kube.Obj{"name": "SB_ITEMS", "value": strings.Join(items, " ")},
		kube.Obj{"name": "SB_CONTROL", "value": "http://" + s.cfg.InternalHostname + ":8080"},
	}
	lines, err := s.runJob(ctx, repoJob("fetch-"+randID(6), script, env), 10*time.Minute)
	if err != nil {
		return nil, err
	}
	out := map[string]*restoredMeta{}
	get := func(app string) *restoredMeta {
		if out[app] == nil {
			out[app] = &restoredMeta{}
		}
		return out[app]
	}
	for _, l := range lines {
		f := strings.Fields(l)
		if len(f) != 3 {
			continue
		}
		switch f[0] {
		case "SB_APPJSON":
			raw, _ := base64.StdEncoding.DecodeString(f[2])
			var spec apps.App
			if err := json.Unmarshal(raw, &spec); err != nil {
				return nil, fmt.Errorf("app spec of %s: %w", f[1], err)
			}
			get(f[1]).Spec = &spec
		case "SB_ENVJSON":
			raw, _ := base64.StdEncoding.DecodeString(f[2])
			json.Unmarshal(raw, &get(f[1]).Env)
		case "SB_SOURCE":
			get(f[1]).Upload = f[2]
		}
	}
	for app := range picks {
		if out[app] == nil || out[app].Spec == nil {
			return nil, fmt.Errorf("snapshot of %s returned no app spec", app)
		}
	}
	return out, nil
}

// handleRestoreSource receives a restored source tarball from a fetch job.
func (s *Server) handleRestoreSource(w http.ResponseWriter, r *http.Request) {
	app := strings.TrimPrefix(r.URL.Path, "/internal/restore-source/")
	if r.Method != http.MethodPost || !hmac.Equal([]byte(r.URL.Query().Get("sig")), []byte(s.sourceSig(app))) {
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}
	if err := os.MkdirAll(s.uploadDir(), 0o700); err != nil {
		http.Error(w, err.Error(), 500)
		return
	}
	id := "u" + randID(10)
	tmp := s.uploadPath(id) + ".part"
	f, err := os.OpenFile(tmp, os.O_CREATE|os.O_WRONLY|os.O_EXCL, 0o600)
	if err != nil {
		http.Error(w, err.Error(), 500)
		return
	}
	var body io.Reader = r.Body
	if r.URL.Query().Get("encoding") == "base64" {
		body = base64.NewDecoder(base64.StdEncoding, r.Body)
	}
	n, err := io.Copy(f, io.LimitReader(body, maxUpload+1))
	f.Close()
	if err == nil && n > maxUpload {
		err = errors.New("too large")
	}
	if err == nil {
		_, err = validateTarGz(tmp)
	}
	if err == nil {
		err = os.Rename(tmp, s.uploadPath(id))
	}
	if err != nil {
		os.Remove(tmp)
		s.logf("restore source for %s rejected after %d bytes: %v", app, n, err)
		http.Error(w, err.Error(), 400)
		return
	}
	io.WriteString(w, id)
}

// --- recreating apps ---

// recreateApp creates an app from a snapshot: paused, with a restore queued, so it starts
// only once its volumes and database hold the backed-up data.
func (s *Server) recreateApp(ctx context.Context, name, snapshot string, m *restoredMeta) (*apps.App, error) {
	if _, err := s.getApp(ctx, name); err == nil {
		return nil, fmt.Errorf("an app named %s exists; restore its data with: sb restore %s", name, name)
	}
	a := m.Spec
	a.Name = name
	if a.Upload != nil {
		if m.Upload == "" {
			return nil, fmt.Errorf("snapshot of %s has no copy of its source; redeploy it with sb deploy --dir, then sb restore", name)
		}
		up, ok := s.uploadInfo(m.Upload)
		if !ok {
			return nil, fmt.Errorf("restored source of %s is missing", name)
		}
		a.Upload = up
	}
	if err := a.Validate(); err != nil {
		return nil, err
	}
	a.Paused = true
	// The restored snapshot counts as the latest backup, so the app is not paused again
	// for a fresh one right after it starts; the next scheduled backup runs as usual.
	restored := apps.BackupRun{ID: "restored", Kind: "backup", Phase: "succeeded", Trigger: "found in backup repository", Snapshot: snapshot, StartedAt: now(), FinishedAt: now()}
	a.Backup = &apps.BackupState{
		History:    []apps.BackupRun{restored},
		LastBackup: &restored,
		Running:    &apps.BackupRun{ID: randID(6), Kind: "restore", Phase: "stopping", Trigger: "recreate from backup", Snapshot: snapshot, StartedAt: now()},
	}
	// The namespace and the app's secret values must exist before the first reconcile,
	// or it would generate new secrets the restored data does not match.
	if err := s.kube.Apply(ctx, kube.Obj{"apiVersion": "v1", "kind": "Namespace", "metadata": kube.Obj{"name": apps.Namespace(name),
		"labels": kube.Obj{"pod-security.kubernetes.io/enforce": "baseline", apps.ManagedByLabel: "slicedbread", apps.AppLabel: name}}}); err != nil {
		return nil, err
	}
	if len(m.Env) > 0 {
		if err := s.writeAppSecret(ctx, a, m.Env); err != nil {
			return nil, err
		}
	}
	if err := s.saveApp(ctx, a); err != nil {
		return nil, err
	}
	cur, err := s.getApp(ctx, name)
	if err == nil {
		err = s.reconcile(ctx, cur)
	}
	if err != nil {
		s.deleteApp(ctx, name) // don't leave a half-created app behind
		return nil, err
	}
	return cur, nil
}

// recreate brings back the named apps from their latest (or the given) snapshot.
func (s *Server) recreate(ctx context.Context, want map[string]string) ([]string, error) {
	return s.recreateAll(ctx, want, false)
}

// recreateLocked is recreate for callers that already hold s.mu (approvals).
func (s *Server) recreateLocked(ctx context.Context, want map[string]string) ([]string, error) {
	return s.recreateAll(ctx, want, true)
}

func (s *Server) recreateAll(ctx context.Context, want map[string]string, locked bool) ([]string, error) {
	snaps, err := s.repoSnapshots(ctx)
	if err != nil {
		return nil, err
	}
	picks := map[string]string{}
	for app, snap := range want {
		for _, sn := range snaps {
			if sn.App == app && (snap == "" || strings.HasPrefix(sn.ID, snap)) {
				picks[app] = sn.ID
				break // newest first
			}
		}
		if picks[app] == "" {
			return nil, fmt.Errorf("no snapshot of %s in the backup repository (see: sb backups --repository)", app)
		}
	}
	metas, err := s.fetchMeta(ctx, picks)
	if err != nil {
		return nil, err
	}
	if !locked {
		s.mu.Lock()
		defer s.mu.Unlock()
	}
	var done []string
	for app, snap := range picks {
		if _, err := s.recreateApp(ctx, app, snap, metas[app]); err != nil {
			return done, fmt.Errorf("%s: %w", app, err)
		}
		done = append(done, app+" from "+short(snap))
	}
	sort.Strings(done)
	return done, nil
}

// --- API ---

// apiRepository lists apps found in the backup repository.
func (s *Server) apiRepository(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 6*time.Minute)
	defer cancel()
	snaps, err := s.repoSnapshots(ctx)
	if err != nil {
		fail(w, 500, err.Error(), "")
		return
	}
	byApp := map[string]*RepoApp{}
	for _, sn := range snaps {
		ra := byApp[sn.App]
		if ra == nil {
			ra = &RepoApp{Name: sn.App, Latest: sn.ID, LatestAt: sn.Time.UTC().Format(time.RFC3339)}
			if _, err := s.getApp(ctx, sn.App); err == nil {
				ra.Exists = true
			}
			byApp[sn.App] = ra
		}
		ra.Snapshots++
	}
	out := []*RepoApp{}
	for _, ra := range byApp {
		out = append(out, ra)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	writeJSON(w, 200, map[string]any{"apps": out})
}

// apiRecreate brings back deleted apps from the repository. Admin tokens act at once;
// agent tokens get an approval request.
func (s *Server) apiRecreate(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Apps     []string `json:"apps"`
		All      bool     `json:"all"`
		Snapshot string   `json:"snapshot"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<14)).Decode(&req); err != nil {
		fail(w, 400, "bad JSON", "")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 15*time.Minute)
	defer cancel()
	if scopeOf(r) != ScopeAdmin {
		if req.All || len(req.Apps) != 1 {
			fail(w, 403, "restoring several apps at once is done by a human in the dashboard", "https://"+s.hosts().Auth()+"/settings")
			return
		}
		ap, err := s.requestApproval(ctx, "recreate", req.Apps[0], "recreate deleted app "+req.Apps[0]+" from its backup"+map[bool]string{true: " (snapshot " + req.Snapshot + ")", false: ""}[req.Snapshot != ""], nil, scopeOf(r))
		if err != nil {
			fail(w, 500, err.Error(), "")
			return
		}
		ap.Snapshot = req.Snapshot
		s.saveApproval(ctx, ap)
		s.approvalResponse(w, ap)
		return
	}
	want := map[string]string{}
	if req.All {
		snaps, err := s.repoSnapshots(ctx)
		if err != nil {
			fail(w, 500, err.Error(), "")
			return
		}
		for _, sn := range snaps {
			if _, err := s.getApp(ctx, sn.App); err != nil {
				want[sn.App] = ""
			}
		}
		if len(want) == 0 {
			writeJSON(w, 200, map[string]any{"recreated": []string{}, "note": "every app in the repository already exists"})
			return
		}
	} else {
		for _, a := range req.Apps {
			want[a] = req.Snapshot
		}
	}
	done, err := s.recreate(ctx, want)
	if err != nil {
		fail(w, 500, err.Error(), "")
		return
	}
	writeJSON(w, 202, map[string]any{"recreated": done, "hint": "each app starts after its data is restored; follow with: sb backups (or sb wait <app>)"})
}
