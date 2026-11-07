//go:build e2e

package hetzner

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/melzi-cloud/sliced-bread/internal/apps"
	"github.com/melzi-cloud/sliced-bread/internal/config"
	"github.com/melzi-cloud/sliced-bread/internal/guard"
	"github.com/melzi-cloud/sliced-bread/internal/labels"
	"github.com/melzi-cloud/sliced-bread/internal/tenant"
)

// TestOSUpdates checks the nightly OS maintenance end to end on a 3-node install, without
// waiting for Debian to publish an update: a local apt repository on every server carries
// a dummy package (built here with dpkg-deb). Version 1 is installed at setup; publishing
// version 2 is the "update". Its postinst runs the kernel postinst hooks like a kernel
// package, so Debian's unattended-upgrades hook asks for a reboot, and a dummy daemon maps
// a file the update replaces, so needrestart has to restart it. Only this test allows the
// repository's origin in unattended-upgrades.
//
//  1. A failed backup (wrong bucket key) postpones the updates and raises an alert.
//  2. With the key fixed, "Run OS maintenance now" backs up every app with data and waits
//     for the restore tests, installs the update, needrestart restarts the daemon, and the
//     servers reboot one at a time in the same run.
//  3. A backup schedule changed in the dashboard is followed.
//
// Run with: make e2e-os-updates (about 1.5 to 2.5 hours, most of it waiting for the next
// full hour in step 3). SB_RUN=<run-id> reuses a running prototype install (it is kept);
// SB_KEEP=1 keeps a new one.
func TestOSUpdates(t *testing.T) {
	cfg, err := config.LoadDev()
	if err != nil {
		t.Fatalf("config (run through hack/secrets-env): %v", err)
	}
	s3 := s3Client{endpoint: os.Getenv("SB_S3_ENDPOINT"), region: os.Getenv("SB_S3_REGION"), bucket: os.Getenv("SB_S3_BUCKET"),
		key: os.Getenv("SB_S3_ACCESS_KEY"), secret: os.Getenv("SB_S3_SECRET_KEY")}
	if s3.endpoint == "" || s3.bucket == "" || s3.key == "" || s3.secret == "" {
		t.Fatal("SB_S3_ENDPOINT, SB_S3_BUCKET (dev.env) and SB_S3_ACCESS_KEY, SB_S3_SECRET_KEY (keychain) are needed")
	}
	root := repoRoot(t)
	sb := filepath.Join(root, "bin", "sb")
	if _, err := os.Stat(sb); err != nil {
		t.Fatal("bin/sb missing: run make build")
	}
	p := tenant.New(cfg, filepath.Join(root, tenant.StateRoot))
	p.Logf = func(f string, a ...any) { t.Logf(f, a...) }
	ctx, cancel := context.WithTimeout(context.Background(), 170*time.Minute)
	defer cancel()

	runID, created := os.Getenv("SB_RUN"), false
	if runID == "" {
		cmd := exec.CommandContext(ctx, sb, "dev", "prototype", "up")
		cmd.Dir = root
		cmd.Env = append(os.Environ(), "SB_TTL=4h")
		var out bytes.Buffer
		cmd.Stdout, cmd.Stderr = io.MultiWriter(&out, testWriter{t}), testWriter{t}
		err = cmd.Run()
		m := regexp.MustCompile(`run id:\s+(r[a-z0-9]{7})`).FindStringSubmatch(out.String())
		if m == nil {
			m = regexp.MustCompile(`\[(r[a-z0-9]{7})\]`).FindStringSubmatch(out.String())
		}
		if m == nil {
			t.Fatalf("prototype up failed before a run id was known: %v", err)
		}
		runID, created = m[1], true
		if err != nil {
			defer teardown(t, p, runID)
			t.Fatalf("prototype up: %v", err)
		}
	}
	prefix := "test/" + runID
	defer func() {
		dctx, dcancel := context.WithTimeout(context.Background(), 10*time.Minute)
		defer dcancel()
		if n, err := s3.deletePrefix(dctx, prefix+"/"); err != nil {
			t.Errorf("delete backup prefix %s: %v", prefix, err)
		} else {
			t.Logf("deleted %d objects under %s", n, prefix)
		}
	}()
	st, err := tenant.LoadState(p.StateRoot, runID)
	if err != nil {
		t.Fatal(err)
	}
	h := &osHarness{t: t, ctx: ctx, st: st}
	defer func() {
		if t.Failed() {
			h.artifacts(root)
		}
		if created && os.Getenv("SB_KEEP") != "1" {
			teardown(t, p, runID)
		} else {
			t.Logf("keeping %s (make tenant-down RUN=%s)", runID, runID)
		}
	}()
	admin := filepath.Join(st.Dir, "cli-admin.json")
	run := func(args ...string) string {
		c := exec.CommandContext(ctx, sb, args...)
		c.Env = append(os.Environ(), "SB_CONFIG="+admin)
		b, err := c.CombinedOutput()
		if err != nil {
			t.Fatalf("sb %s: %v\n%s", strings.Join(args, " "), err, b)
		}
		return string(b)
	}

	// --- setup: apps with data, backups configured in the dashboard, the test repository ---
	existing := map[string]bool{}
	for _, a := range h.apps() {
		existing[a.Name] = true
	}
	if !existing["vault"] {
		run("deploy", "vault", "--template", "vaultwarden")
	}
	if !existing["analytics"] {
		run("deploy", "analytics", "--template", "umami")
	}
	run("wait", "vault", "--timeout", "10m")
	run("wait", "analytics", "--timeout", "15m")

	var login struct{ URL, Email, Password string }
	b, err := os.ReadFile(filepath.Join(st.Dir, "admin-login.json"))
	if err != nil || json.Unmarshal(b, &login) != nil {
		t.Fatalf("admin-login.json: %v", err)
	}
	d, err := newDashboard(ctx, login.URL, login.Email, login.Password)
	if err != nil {
		t.Fatalf("dashboard login: %v", err)
	}
	target := func(secret string) url.Values {
		return url.Values{"endpoint": {s3.endpoint}, "bucket": {s3.bucket}, "region": {s3.region}, "prefix": {prefix},
			"key": {s3.key}, "secret": {secret}}
	}
	h.mustPost(d, "/settings/backups", target(s3.secret), "Backups configured")
	h.step("backups configured in the dashboard to %s/%s", s3.bucket, prefix)
	h.waitFor("first backup and restore test of every app with data", 20*time.Minute, func() (bool, string) {
		var waiting []string
		for _, a := range h.apps() {
			if g, why := apps.UpdateGate(a, time.Now().Add(-24*time.Hour), time.Now()); g != apps.GateOK && a.HasData() {
				waiting = append(waiting, a.Name+": "+why)
			}
		}
		return len(waiting) == 0, strings.Join(waiting, "; ")
	})

	h.onNodes("setup", func(i int, out string) {
		if !strings.Contains(out, "active") {
			t.Fatalf("node %d: dummy daemon not running after setup:\n%s", i+1, out)
		}
	})
	before := h.nodeStates()
	for i, s := range before {
		if s.Version != "1" || !s.Active || len(s.Starts) != 1 {
			t.Fatalf("node %d after setup: %+v", i+1, s)
		}
	}
	h.onNodes("publish", nil)
	h.step("published sbdummy 2 in the local repository of every server")

	// --- 1. a failed backup postpones the updates and raises an alert ---
	bad := make([]byte, 12)
	rand.Read(bad)
	h.mustPost(d, "/settings/backups", target("wrong-"+hex.EncodeToString(bad)), "Backups configured")
	t1 := time.Now().UTC().Truncate(time.Second)
	h.mustPost(d, "/settings/maintenance", nil, "OS maintenance requested")
	h.step("bucket key broken; Run OS maintenance now pressed at %s", t1.Format(time.RFC3339))
	var m *guard.MaintState
	h.waitFor("the run to postpone the updates", 25*time.Minute, func() (bool, string) {
		m = h.maint()
		return ranSince(m, t1) && !m.Running && m.Postponed != nil, "phase " + m.Phase + " " + lastLog(m)
	})
	if !strings.Contains(m.Postponed.Reason, "backup failed") {
		t.Errorf("postponed for the wrong reason: %s", m.Postponed.Reason)
	}
	h.step("updates postponed: %s", m.Postponed.Reason)
	failedForUpdates := 0
	for _, a := range h.apps() {
		if b := a.Backup; a.HasData() && b != nil && b.LastBackup != nil && b.LastBackup.Trigger == "before OS updates" && b.LastBackup.Phase == "failed" {
			failedForUpdates++
		}
	}
	if failedForUpdates == 0 {
		t.Error("no backup was started before the updates (and failed)")
	}
	h.waitFor("the postponed alert", 3*time.Minute, func() (bool, string) {
		a := h.openAlerts()
		return a["maintenance-postponed"] != "", fmt.Sprint(a)
	})
	h.step("alert open: %s", h.openAlerts()["maintenance-postponed"])
	for i, s := range h.nodeStates() {
		if s.Version != "1" || s.Boot != before[i].Boot {
			t.Errorf("node %d changed although the updates were postponed: %+v", i+1, s)
		}
	}
	h.step("nothing installed and nothing rebooted while backups failed")

	// --- 2. backups first, then updates, needrestart and one reboot at a time ---
	h.mustPost(d, "/settings/backups", target(s3.secret), "Backups configured")
	t2 := time.Now().UTC().Truncate(time.Second)
	h.mustPost(d, "/settings/maintenance", nil, "OS maintenance requested")
	h.step("bucket key fixed; Run OS maintenance now pressed at %s", t2.Format(time.RFC3339))
	var atUpdating []*apps.App
	h.waitFor("backups and restore tests before the updates", 60*time.Minute, func() (bool, string) {
		m = h.maint()
		if ranSince(m, t2) && m.Phase == "updating" {
			atUpdating = h.apps()
			return true, ""
		}
		if ranSince(m, t2) && !m.Running {
			t.Fatalf("run ended without installing: %+v", m.Postponed)
		}
		w := ""
		if m.Backups != nil {
			w = strings.Join(m.Backups.Waiting, "; ")
		}
		return false, "phase " + m.Phase + " " + w
	})
	for _, a := range atUpdating {
		if !a.HasData() {
			continue
		}
		b := a.Backup
		latest := b.History[0]
		started, _ := time.Parse(time.RFC3339, latest.StartedAt)
		if latest.Trigger != "before OS updates" || started.Before(t2) {
			t.Errorf("%s: latest backup %s (%s) was not taken for this run", a.Name, latest.StartedAt, latest.Trigger)
		}
		if b.LastTest == nil || b.LastTest.Snapshot != latest.Snapshot || b.LastTest.Phase != "succeeded" {
			t.Errorf("%s: restore test of snapshot %s did not pass before the updates: %+v", a.Name, latest.Snapshot, b.LastTest)
			continue
		}
		h.step("%s: backup %s (%s, snapshot %s) and restore test passed %s, before any update", a.Name, latest.StartedAt, latest.Trigger, short12(latest.Snapshot), b.LastTest.FinishedAt)
	}
	h.waitFor("installs and reboots", 75*time.Minute, func() (bool, string) {
		m = h.maint()
		return ranSince(m, t2) && !m.Running, "phase " + m.Phase + " " + lastLog(m)
	})
	if m.Postponed != nil {
		t.Fatalf("postponed: %+v", m.Postponed)
	}
	for _, l := range m.Log {
		if at := logTime(l); !at.Before(t2) {
			t.Logf("  maintenance log: %s", l)
		}
	}
	// One server at a time: each reboot ends before the next starts.
	type span struct {
		node       string
		start, end time.Time
	}
	var spans []span
	installs := 0
	var backupsDone time.Time
	for _, l := range m.Log {
		at := logTime(l)
		if at.Before(t2) {
			continue
		}
		msg := l[strings.Index(l, " ")+1:]
		node, rest, _ := strings.Cut(msg, ": ")
		switch {
		case strings.HasPrefix(msg, "backups done"):
			backupsDone = at
		case rest == "installing updates":
			installs++
			if backupsDone.IsZero() || at.Before(backupsDone) {
				t.Errorf("%s installed updates before the backups were done", node)
			}
		case strings.HasPrefix(rest, "reboot needed"):
			spans = append(spans, span{node: node, start: at})
		case strings.HasPrefix(rest, "rebooted and back"):
			for i := range spans {
				if spans[i].node == node && spans[i].end.IsZero() {
					spans[i].end = at
				}
			}
		}
	}
	if installs != 3 || len(spans) != 3 {
		t.Errorf("want 3 installs and 3 reboots in the run, got %d and %d", installs, len(spans))
	}
	sort.Slice(spans, func(i, j int) bool { return spans[i].start.Before(spans[j].start) })
	for i, s := range spans {
		if s.end.IsZero() {
			t.Errorf("%s: reboot did not finish", s.node)
			continue
		}
		if i > 0 && s.start.Before(spans[i-1].end) {
			t.Errorf("%s rebooted while %s was still rebooting", s.node, spans[i-1].node)
		}
		h.step("%s rebooted %s to %s", s.node, s.start.Format("15:04:05"), s.end.Format("15:04:05"))
	}
	for name, nm := range m.Nodes {
		if !strings.Contains(nm.Installed, "sbdummy") || !strings.Contains(nm.Restarted, "sbdummy.service") || !strings.Contains(nm.RebootFor, "sbdummy") || nm.Error != "" {
			t.Errorf("%s: installed %q, restarted %q, reboot for %q, error %q", name, nm.Installed, nm.Restarted, nm.RebootFor, nm.Error)
		}
	}
	after := h.nodeStates()
	for i, s := range after {
		if s.Version != "2" || s.Boot == before[i].Boot || !s.Active {
			t.Errorf("node %d after the run: version %s, boot changed %v, active %v", i+1, s.Version, s.Boot != before[i].Boot, s.Active)
		}
		// needrestart restarted the daemon with version 2 before the reboot.
		restarted := false
		for _, l := range s.Starts {
			if strings.Contains(l, "version=2 boot="+before[i].Boot) {
				restarted = true
			}
		}
		if !restarted {
			t.Errorf("node %d: the daemon was not restarted before the reboot: %v", i+1, s.Starts)
		}
		h.step("node %d: sbdummy %s, daemon starts: %s", i+1, s.Version, strings.Join(s.Starts, " | "))
	}
	h.waitFor("the postponed alert to clear", 3*time.Minute, func() (bool, string) {
		a := h.openAlerts()
		return a["maintenance-postponed"] == "", fmt.Sprint(a)
	})

	// --- 3. the backup schedule set in the dashboard is followed ---
	last := time.Time{}
	for _, a := range h.apps() {
		if a.Backup != nil && a.Backup.LastBackup != nil {
			if t0, _ := time.Parse(time.RFC3339, a.Backup.LastBackup.StartedAt); t0.After(last) {
				last = t0
			}
		}
	}
	slot := last.Truncate(time.Hour).Add(time.Hour)
	if slot.Hour() == 3 { // the old schedule's own slot proves nothing
		slot = slot.Add(time.Hour)
	}
	if wait := time.Until(slot.Add(90 * time.Second)); wait > 0 {
		h.step("waiting %s for %s, the next full hour after the last backup", wait.Round(time.Second), slot.Format("15:04"))
		select {
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		case <-time.After(wait):
		}
	}
	now := time.Now().UTC()
	slot = now.Truncate(time.Hour)
	for _, a := range h.apps() {
		if b := a.Backup; b != nil && b.LastBackup != nil {
			if t0, _ := time.Parse(time.RFC3339, b.LastBackup.StartedAt); !t0.Before(slot) {
				t.Errorf("%s was backed up at %s under the old daily 03:00 schedule", a.Name, b.LastBackup.StartedAt)
			}
		}
	}
	h.step("no backups at %s under the daily 03:00 schedule", slot.Format("15:04"))
	h.mustPost(d, "/settings/backup-schedule", url.Values{"hour": {fmt.Sprint(slot.Hour())}, "every": {"6"}, "weekday": {"0"}}, "Backups now run every 6 hours")
	page, err := d.get(ctx, "/settings")
	if err != nil || !strings.Contains(page, fmt.Sprintf("OS maintenance runs daily at %02d:00 UTC", slot.Hour()%6+1)) {
		t.Errorf("settings page does not show the new maintenance hour (%v)", err)
	}
	h.step("schedule set in the dashboard: every 6 hours from %02d:00 UTC", slot.Hour())
	h.waitFor("scheduled backups at the new slot", 5*time.Minute, func() (bool, string) {
		var waiting []string
		for _, a := range h.apps() {
			b := a.Backup
			if b == nil || b.LastBackup == nil || b.LastBackup.Trigger != "schedule" {
				waiting = append(waiting, a.Name)
				continue
			}
			if t0, _ := time.Parse(time.RFC3339, b.LastBackup.StartedAt); t0.Before(slot) {
				waiting = append(waiting, a.Name)
			}
		}
		return len(waiting) == 0, strings.Join(waiting, ", ")
	})
	for _, a := range h.apps() {
		h.step("%s: scheduled backup started %s", a.Name, a.Backup.LastBackup.StartedAt)
	}
}

func teardown(t *testing.T, p *tenant.Provisioner, runID string) {
	dctx, dcancel := context.WithTimeout(context.Background(), 20*time.Minute)
	defer dcancel()
	if err := p.Down(dctx, runID); err != nil {
		t.Errorf("teardown: %v", err)
	}
	if left, err := p.Inventory(dctx, labels.Selector(runID)); err != nil || len(left) > 0 {
		t.Errorf("resources remain after teardown: %v %v", left, err)
	}
}

func ranSince(m *guard.MaintState, t0 time.Time) bool {
	last, _ := time.Parse(time.RFC3339, m.LastRun)
	return !last.Before(t0)
}

func lastLog(m *guard.MaintState) string {
	if len(m.Log) == 0 {
		return ""
	}
	return m.Log[len(m.Log)-1]
}

func logTime(l string) time.Time {
	at, _ := time.Parse(time.RFC3339, strings.SplitN(l, " ", 2)[0])
	return at
}

func short12(s string) string {
	if len(s) > 12 {
		return s[:12]
	}
	return s
}

type osHarness struct {
	t   *testing.T
	ctx context.Context
	st  *tenant.State
}

func (h *osHarness) step(f string, a ...any) {
	h.t.Logf("[%s] "+f, append([]any{time.Now().UTC().Format("15:04:05")}, a...)...)
}

func (h *osHarness) kubectl(args string) []byte {
	var lastErr error
	for try := 0; try < 20; try++ {
		// Any server will do; one of them may be rebooting.
		for i := range h.st.Nodes {
			tg, err := h.st.Target((try + i) % len(h.st.Nodes))
			if err != nil {
				lastErr = err
				continue
			}
			cctx, cancel := context.WithTimeout(h.ctx, 30*time.Second)
			out, err := tg.Run(cctx, "k3s kubectl "+args, nil)
			cancel()
			if err == nil {
				return out
			}
			lastErr = err
		}
		time.Sleep(5 * time.Second)
	}
	h.t.Fatalf("kubectl %s: %v", args, lastErr)
	return nil
}

func (h *osHarness) apps() []*apps.App {
	var list struct {
		Items []struct {
			Metadata struct{ Name string }
			Spec     apps.App
		}
	}
	if err := json.Unmarshal(h.kubectl("get apps.slicedbread.dev -o json"), &list); err != nil {
		h.t.Fatal(err)
	}
	var out []*apps.App
	for _, it := range list.Items {
		a := it.Spec
		a.Name = it.Metadata.Name
		out = append(out, &a)
	}
	return out
}

func (h *osHarness) maint() *guard.MaintState {
	m := &guard.MaintState{}
	json.Unmarshal(h.kubectl(`-n sb-system get configmap sb-maintenance -o jsonpath={.data.state\.json}`), m)
	return m
}

// openAlerts maps open alert ids to titles.
func (h *osHarness) openAlerts() map[string]string {
	var st struct {
		Entries map[string]struct {
			Open  bool
			Alert struct{ Title string }
		}
	}
	json.Unmarshal(h.kubectl(`-n sb-system get configmap sb-alerts -o jsonpath={.data.state}`), &st)
	out := map[string]string{}
	for id, e := range st.Entries {
		if e.Open {
			out[id] = e.Alert.Title
		}
	}
	return out
}

func (h *osHarness) waitFor(what string, max time.Duration, ok func() (bool, string)) {
	h.step("waiting for %s", what)
	deadline, lastNote := time.Now().Add(max), ""
	for {
		done, note := ok()
		if done {
			return
		}
		if note != lastNote {
			h.step("  ... %s", note)
			lastNote = note
		}
		if time.Now().After(deadline) {
			h.t.Fatalf("%s: not within %s (%s)", what, max, note)
		}
		select {
		case <-h.ctx.Done():
			h.t.Fatal(h.ctx.Err())
		case <-time.After(10 * time.Second):
		}
	}
}

func (h *osHarness) mustPost(d *dashboard, path string, form url.Values, want string) {
	page, err := d.post(h.ctx, path, form)
	if err != nil {
		h.t.Fatalf("POST %s: %v", path, err)
	}
	if !strings.Contains(page, want) {
		msg := regexp.MustCompile(`<p class="err">([^<]*)</p>`).FindStringSubmatch(page)
		h.t.Fatalf("POST %s: page does not say %q (error: %v)", path, want, msg)
	}
}

// onNodes runs the test repository script with an action on every server.
func (h *osHarness) onNodes(action string, check func(i int, out string)) {
	for i := range h.st.Nodes {
		tg, err := h.st.Target(i)
		if err != nil {
			h.t.Fatal(err)
		}
		out, err := tg.Run(h.ctx, "sh -s -- "+action, strings.NewReader(testRepoScript))
		if err != nil {
			h.t.Fatalf("node %d %s: %v\n%s", i+1, action, err, out)
		}
		if check != nil {
			check(i, string(out))
		}
	}
}

type dummyState struct {
	Version, Boot string
	Active        bool
	Starts        []string
}

func (h *osHarness) nodeStates() []dummyState {
	h.st.RefreshNodes(h.ctx)
	var out []dummyState
	for i := range h.st.Nodes {
		tg, err := h.st.Target(i)
		if err != nil {
			h.t.Fatal(err)
		}
		b, err := tg.Run(h.ctx, "sh -s -- state", strings.NewReader(testRepoScript))
		if err != nil {
			h.t.Fatalf("node %d state: %v", i+1, err)
		}
		var s dummyState
		for _, l := range strings.Split(string(b), "\n") {
			k, v, _ := strings.Cut(l, " ")
			switch k {
			case "VERSION":
				s.Version = v
			case "BOOT":
				s.Boot = v
			case "ACTIVE":
				s.Active = v == "active"
			case "LOG":
				s.Starts = append(s.Starts, v)
			}
		}
		out = append(out, s)
	}
	return out
}

func (h *osHarness) artifacts(root string) {
	dir := filepath.Join(root, "test", "artifacts", h.st.RunID)
	os.MkdirAll(dir, 0o700)
	actx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	for name, args := range map[string]string{
		"sb-control.log":  "-n sb-system logs -l app=sb-control --tail 2000 --prefix",
		"sb-guard.log":    "-n sb-system logs -l app=sb-guard --tail 2000 --prefix",
		"apps.txt":        "get apps.slicedbread.dev -o yaml",
		"maintenance.txt": "-n sb-system get configmap sb-maintenance -o yaml",
		"jobs.txt":        "get jobs -A -o wide",
		"node-ops.log":    "-n sb-node-ops logs -l job-name --tail 400 --prefix --max-log-requests 20",
	} {
		if out, err := h.st.Kubectl(actx, args, nil); err == nil {
			os.WriteFile(filepath.Join(dir, name), out, 0o600)
		}
	}
	h.t.Logf("artifacts in test/artifacts/%s", h.st.RunID)
}

// --- dashboard, as a browser ---

type dashboard struct {
	base string
	c    *http.Client
}

var csrfRe = regexp.MustCompile(`name="csrf" value="([^"]+)"`)

func newDashboard(ctx context.Context, base, email, password string) (*dashboard, error) {
	jar, _ := cookiejar.New(nil)
	d := &dashboard{base: strings.TrimRight(base, "/"), c: &http.Client{Jar: jar, Timeout: 10 * time.Minute}}
	page, err := d.get(ctx, "/login")
	if err != nil {
		return nil, err
	}
	m := csrfRe.FindStringSubmatch(page)
	if m == nil {
		return nil, fmt.Errorf("no login form")
	}
	page, err = d.send(ctx, "/login", url.Values{"csrf": {m[1]}, "email": {email}, "password": {password}, "rd": {""}})
	if err != nil {
		return nil, err
	}
	if !strings.Contains(page, "Signed in as") {
		return nil, fmt.Errorf("login failed")
	}
	return d, nil
}

func (d *dashboard) do(req *http.Request) (string, error) {
	req.Header.Set("Accept", "text/html")
	resp, err := d.c.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 200 {
		return string(b), fmt.Errorf("%s %s: HTTP %d", req.Method, req.URL.Path, resp.StatusCode)
	}
	return string(b), nil
}

func (d *dashboard) get(ctx context.Context, path string) (string, error) {
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, d.base+path, nil)
	return d.do(req)
}

func (d *dashboard) send(ctx context.Context, path string, form url.Values) (string, error) {
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, d.base+path, strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	return d.do(req)
}

// post submits a settings form with the session's CSRF token.
func (d *dashboard) post(ctx context.Context, path string, form url.Values) (string, error) {
	page, err := d.get(ctx, "/settings")
	if err != nil {
		return "", err
	}
	m := csrfRe.FindStringSubmatch(page)
	if m == nil {
		return "", fmt.Errorf("no form on the settings page")
	}
	f := url.Values{}
	for k, v := range form {
		f[k] = v
	}
	f.Set("csrf", m[1])
	return d.send(ctx, path, f)
}

// testRepoScript manages the local apt repository and the dummy package on a server.
// Actions: setup (repository, version 1 installed and running), publish (version 2),
// state (installed version, boot id, daemon starts).
const testRepoScript = `set -eu
export DEBIAN_FRONTEND=noninteractive LANG=C LC_ALL=C
R=/srv/sbtest-repo
aptq() { apt-get -o DPkg::Lock::Timeout=600 -qq "$@"; }
build() {
  v=$1; d=$(mktemp -d)
  mkdir -p "$d/DEBIAN" "$d/usr/lib/sbdummy" "$d/lib/systemd/system"
  printf 'Package: sbdummy\nVersion: %s\nArchitecture: all\nMaintainer: SlicedBread tests <tests@slicedbread.dev>\nDepends: python3\nDescription: SlicedBread OS update test package\n' "$v" > "$d/DEBIAN/control"
  echo "sbdummy library $v" > "$d/usr/lib/sbdummy/libsbdummy.so"
  cat > "$d/usr/lib/sbdummy/sbdummyd" <<'EOF'
# Maps the library executable, like a process using a shared library, so needrestart
# notices when an update replaces it. Logs each start with the version it loaded.
import mmap, time
f = open("/usr/lib/sbdummy/libsbdummy.so", "rb")
m = mmap.mmap(f.fileno(), 0, flags=mmap.MAP_PRIVATE, prot=mmap.PROT_READ | mmap.PROT_EXEC)
version = bytes(m[:]).decode().split()[-1]
boot = open("/proc/sys/kernel/random/boot_id").read().strip()
with open("/var/log/sbdummy.log", "a") as log:
    log.write(time.strftime("%Y-%m-%dT%H:%M:%SZ", time.gmtime()) + " start version=" + version + " boot=" + boot + "\n")
while True:
    time.sleep(3600)
EOF
  cat > "$d/lib/systemd/system/sbdummy.service" <<'EOF'
[Unit]
Description=SlicedBread OS update test daemon
[Service]
ExecStart=/usr/bin/python3 /usr/lib/sbdummy/sbdummyd
[Install]
WantedBy=multi-user.target
EOF
  cat > "$d/DEBIAN/postinst" <<'EOF'
#!/bin/sh
set -e
if [ "$1" = configure ]; then
  systemctl daemon-reload || true
  if [ -z "$2" ]; then
    systemctl enable --now sbdummy.service
  else
    # Like a kernel package: run the kernel postinst hooks; Debian's unattended-upgrades
    # hook among them asks for a reboot. The daemon is not restarted here (a library
    # package does not restart its users); needrestart has to notice it.
    run-parts --report --exit-on-error --arg="$(uname -r)" --arg="/boot/vmlinuz-$(uname -r)" /etc/kernel/postinst.d
  fi
fi
EOF
  chmod 755 "$d/DEBIAN/postinst"
  mkdir -p "$R/pool"
  dpkg-deb --root-owner-group -Zgzip -b "$d" "$R/pool/sbdummy_${v}_all.deb" >/dev/null
  rm -rf "$d"
}
index() {
  cd "$R"
  : > Packages
  for f in pool/*.deb; do
    dpkg-deb -f "$f" >> Packages
    printf 'Filename: ./%s\nSize: %s\nSHA256: %s\n\n' "$f" "$(stat -c%s "$f")" "$(sha256sum "$f" | cut -d' ' -f1)" >> Packages
  done
  printf 'Origin: SlicedBreadTest\nLabel: SlicedBreadTest\nSuite: sbtest\nCodename: sbtest\nDate: %s\nArchitectures: all amd64\nSHA256:\n %s %s Packages\n' \
    "$(date -u '+%a, %d %b %Y %H:%M:%S UTC')" "$(sha256sum Packages | cut -d' ' -f1)" "$(stat -c%s Packages)" > Release
}
case "$1" in
setup)
  systemctl stop sbdummy.service 2>/dev/null || true
  aptq purge -y sbdummy >/dev/null 2>&1 || true
  rm -rf "$R" /var/log/sbdummy.log
  build 1
  index
  echo 'deb [trusted=yes] file:/srv/sbtest-repo ./' > /etc/apt/sources.list.d/sbtest.list
  printf '// Test installs only: unattended-upgrades may install the test package.\nUnattended-Upgrade::Origins-Pattern { "origin=SlicedBreadTest"; };\n' > /etc/apt/apt.conf.d/51sbtest-origin
  aptq update
  aptq install -y sbdummy >/dev/null
  sleep 2
  systemctl is-active sbdummy.service
  ;;
publish)
  build 2
  index
  ;;
state)
  echo "VERSION $(dpkg-query -W -f '${Version}' sbdummy 2>/dev/null || true)"
  echo "BOOT $(cat /proc/sys/kernel/random/boot_id)"
  echo "ACTIVE $(systemctl is-active sbdummy.service || true)"
  sed 's/^/LOG /' /var/log/sbdummy.log 2>/dev/null || true
  ;;
esac
`
