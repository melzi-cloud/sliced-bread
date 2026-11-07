package control

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/melzi-cloud/sliced-bread/internal/apps"
	"github.com/melzi-cloud/sliced-bread/internal/kube"
	"github.com/melzi-cloud/sliced-bread/internal/schedule"
)

func js(v any) string { b, _ := json.Marshal(v); return string(b) }

func TestBackupJobs(t *testing.T) {
	a := &apps.App{Name: "vault", Volumes: []apps.Volume{{Name: "data", Path: "/data"}}, BackupRequire: []string{"data/db.sqlite3"}}
	run := &apps.BackupRun{ID: "r1", Kind: "backup"}
	j := js(backupJob(a, run, "node-2", ""))
	for _, want := range []string{`"claimName":"data-data"`, `"mountPath":"/sb/vol/data"`, `"nodeName":"node-2"`, `"name":"sb-backup-env"`, resticImage, `"namespace":"app-vault"`} {
		if !strings.Contains(j, want) {
			t.Errorf("backup job missing %s", want)
		}
	}
	if strings.Contains(j, postgresImage) {
		t.Error("no database: no dump container")
	}
	a.Database = true
	j = js(backupJob(a, run, "", ""))
	if !strings.Contains(j, `"name":"dump"`) || !strings.Contains(j, `"key":"uri"`) || !strings.Contains(j, `"runAsUser":999`) {
		t.Error("database backup needs a non-root pg_dump with the app's credentials")
	}
	test := js(restoreTestJob(a, &apps.BackupRun{ID: "t1", Kind: "test", Snapshot: "abc"}, 7, 3))
	for _, want := range []string{`"namespace":"sb-restore-test"`, `"value":"abc"`, `"value":"7"`, `"value":"data/db.sqlite3"`, "--verify", "pg_restore"} {
		if !strings.Contains(test, want) {
			t.Errorf("restore test missing %s", want)
		}
	}
	if strings.Contains(test, "claimName") {
		t.Error("restore tests must not touch the app's volumes")
	}
	rs := js(restoreJob(a, &apps.BackupRun{ID: "x1", Kind: "restore", Snapshot: "abc"}))
	if !strings.Contains(rs, `"claimName":"data-data"`) || !strings.Contains(rs, "--clean --if-exists") {
		t.Error("restore must write the volume and reload the database")
	}
}

func TestBackupSlotOpen(t *testing.T) {
	at := func(v string) time.Time { x, _ := time.Parse(time.RFC3339, v); return x }
	backedUp := func(ts, phase string) *apps.App {
		return &apps.App{Name: "x", Backup: &apps.BackupState{LastBackup: &apps.BackupRun{StartedAt: ts, Phase: phase}}}
	}
	daily := &BackupConfig{Hour: 3}
	sixHours := &BackupConfig{Hour: 3, Every: 6}
	cases := []struct {
		c    *BackupConfig
		a    *apps.App
		now  string
		want bool
	}{
		{daily, &apps.App{Name: "new"}, "2026-11-07T10:00:00Z", true},
		{daily, backedUp("2026-11-07T03:00:20Z", "succeeded"), "2026-11-07T10:00:00Z", false},
		{daily, backedUp("2026-11-06T03:00:20Z", "succeeded"), "2026-11-07T02:59:00Z", false},
		{daily, backedUp("2026-11-06T03:00:20Z", "succeeded"), "2026-11-07T03:00:10Z", true},
		{sixHours, backedUp("2026-11-07T03:00:20Z", "succeeded"), "2026-11-07T08:59:00Z", false},
		{sixHours, backedUp("2026-11-07T03:00:20Z", "succeeded"), "2026-11-07T09:00:10Z", true},
		{sixHours, backedUp("2026-11-07T09:00:20Z", "failed"), "2026-11-07T09:30:00Z", false},
		{sixHours, backedUp("2026-11-07T09:00:20Z", "failed"), "2026-11-07T10:01:00Z", true},
	}
	for i, c := range cases {
		if got := backupSlotOpen(c.a, c.c, at(c.now)); got != c.want {
			t.Errorf("case %d (%s at %s): %v, want %v", i, c.c.Schedule(), c.now, got, c.want)
		}
	}
}

func TestScheduleForm(t *testing.T) {
	form := func(v url.Values) *http.Request {
		r := httptest.NewRequest("POST", "/settings/backup-schedule", strings.NewReader(v.Encode()))
		r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		return r
	}
	b, err := scheduleFromForm(form(url.Values{"hour": {"20"}, "every": {"6"}, "weekday": {"3"}}))
	if err != nil || b != (schedule.Backup{Hour: 20, Every: 6}) || b.MaintenanceHour() != 3 {
		t.Fatalf("%+v %v", b, err)
	}
	b, err = scheduleFromForm(form(url.Values{"hour": {"2"}, "every": {"168"}, "weekday": {"3"}}))
	if err != nil || b.Weekday != time.Wednesday || b.String() != "weekly on Wednesday at 02:00 UTC" {
		t.Fatalf("%+v %v", b, err)
	}
	for _, bad := range []url.Values{{"hour": {"24"}, "every": {"6"}}, {"hour": {"3"}, "every": {"5"}}, {"every": {"6"}}} {
		if _, err := scheduleFromForm(form(bad)); err == nil {
			t.Errorf("accepted %v", bad)
		}
	}
}

func TestSettingsPageShowsSchedule(t *testing.T) {
	v := &settingsView{BackupsConfigured: true, BackupTarget: "s3/b/p", BackupHour: 20, BackupEvery: 6,
		BackupSchedule: "every 6 hours (02:00, 08:00, 14:00, 20:00 UTC)", MaintenanceHour: 3, Hours: []int{2, 20},
		Frequencies: []frequencyChoice{{6, "every 6 hours"}, {24, "daily"}}, Weekdays: []string{"Sunday"},
		MaintPostponed: "2026-11-07T04:01:00Z: backup failed (vault: backup failed: denied)"}
	var b strings.Builder
	if err := pageTmpl.Execute(&b, pageData{Page: "settings", Email: "a@x", IsAdmin: true, Settings: v}); err != nil {
		t.Fatal(err)
	}
	out := b.String()
	for _, want := range []string{`<option value="20" selected>20:00</option>`, `<option value="6" selected>every 6 hours</option>`,
		"every 6 hours (02:00, 08:00, 14:00, 20:00 UTC)", "daily at 03:00 UTC", "Updates postponed 2026-11-07T04:01:00Z"} {
		if !strings.Contains(out, want) {
			t.Errorf("settings page missing %q", want)
		}
	}
}

func TestBackupConfigValidate(t *testing.T) {
	ok := BackupConfig{Endpoint: "https://s3.example.com", Bucket: "b", AccessKeyID: "k", SecretAccessKey: "s", Prefix: "slicedbread/x", Hour: 3}
	if err := ok.validate(); err != nil {
		t.Fatal(err)
	}
	if ok.Repository() != "s3:https://s3.example.com/b/slicedbread/x" {
		t.Fatal(ok.Repository())
	}
	for _, bad := range []BackupConfig{
		{Endpoint: "http://s3.example.com", Bucket: "b", AccessKeyID: "k", SecretAccessKey: "s"},
		{Endpoint: "https://s3.example.com", Bucket: "", AccessKeyID: "k", SecretAccessKey: "s"},
		{Endpoint: "https://s3.example.com", Bucket: "b"},
		{Endpoint: "https://s3.example.com", Bucket: "b", AccessKeyID: "k", SecretAccessKey: "s", Hour: 24},
	} {
		if bad.validate() == nil {
			t.Errorf("accepted %+v", bad)
		}
	}
}

func TestFinishRunKeepsHistoryAndUnpauses(t *testing.T) {
	s := testServer()
	a := &apps.App{Name: "x", Paused: true, Backup: &apps.BackupState{}}
	for i := 0; i < historyLimit+3; i++ {
		r := &apps.BackupRun{ID: "b", Kind: "backup", Snapshot: "s"}
		a.Backup.Running = r
		s.finishRun(a, r, "succeeded", "")
	}
	if len(a.Backup.History) != historyLimit || a.Paused || a.Backup.Running != nil {
		t.Fatalf("history %d paused %v", len(a.Backup.History), a.Paused)
	}
	f := &apps.BackupRun{ID: "f", Kind: "backup"}
	s.finishRun(a, f, "failed", "boom")
	if a.Backup.LastBackup.Phase != "failed" || len(a.Backup.History) != historyLimit {
		t.Fatal("failed backups are recorded but not added to history")
	}
	if !strings.Contains(backupSummary(&apps.App{Name: "y", Database: true, Backup: a.Backup}), "FAILED") {
		t.Fatal("summary shows failures")
	}
}

func TestRenderPausedScalesToZero(t *testing.T) {
	a := &apps.App{Name: "x1", Image: "i", Access: apps.AccessWall, Paused: true}
	objs, err := apps.Render(a, testServer().params(a, ""))
	if err != nil {
		t.Fatal(err)
	}
	for _, o := range objs {
		if o["kind"] == "Deployment" && !strings.Contains(js(o), `"replicas":0`) {
			t.Fatal("paused app must have 0 replicas")
		}
	}
}

func TestBuildJobIsRootlessAndRecordsDigest(t *testing.T) {
	a := &apps.App{Name: "crm", Upload: &apps.Upload{ID: "u1"}}
	j := js(buildJob(Config{RegistryPush: "reg:5000", BuildNamespace: "sb-builds", RailpackVersion: "v1"}, a, "b1", "reg:5000/crm:b1", "http://x"))
	for _, want := range []string{BuildkitRootlessImage, `"runAsUser":1000`, `"runAsNonRoot":true`, "buildctl-daemonless.sh", "--metadata-file", "SB_DIGEST", `"value":"reg:5000/crm:buildcache"`, "--oci-worker-no-process-sandbox"} {
		if !strings.Contains(j, want) {
			t.Errorf("build job missing %s", want)
		}
	}
	if strings.Contains(j, `"privileged":true`) {
		t.Error("build job must not be privileged")
	}
}

func TestRegistryPolicyAndKeep(t *testing.T) {
	nodes := []kube.Obj{{
		"spec":   map[string]any{"podCIDR": "10.42.1.0/24"},
		"status": map[string]any{"addresses": []any{map[string]any{"type": "InternalIP", "address": "10.0.1.2"}, map[string]any{"type": "Hostname", "address": "n1"}}},
	}}
	src := nodeSources(nodes)
	if strings.Join(src, " ") != "10.0.1.2 10.42.1.0 10.42.1.1" {
		t.Fatalf("sources %v", src)
	}
	p := js(registryPolicyObj(src))
	for _, want := range []string{`"cidr":"10.42.1.1/32"`, `"kubernetes.io/metadata.name":"sb-builds"`, `"app":"sb-registry"`} {
		if !strings.Contains(p, want) {
			t.Errorf("policy missing %s", want)
		}
	}
	if strings.Contains(p, "10.42.0.0/16") {
		t.Error("policy must not admit the whole pod network")
	}
	keep := registryKeep([]*apps.App{
		{Name: "crm", Build: &apps.BuildState{Image: "sb-registry.local:5000/crm:b2@sha256:aa", LastGood: "sb-registry.local:5000/crm:b1@sha256:bb"}},
		{Name: "img", Image: "nginx:1"},
	})
	if keepSpec(keep) != "crm=b1,b2,buildcache,sha256:aa,sha256:bb" {
		t.Fatalf("keep %q", keepSpec(keep))
	}
}

func TestBuildIDFromImage(t *testing.T) {
	for in, want := range map[string]string{
		"sb-registry.local:5000/crm:b1@sha256:abc": "b1",
		"sb-registry.local:5000/crm:b1":            "b1",
		"sb-registry.local:5000/crm":               "",
	} {
		if got := buildIDFromImage(in); got != want {
			t.Errorf("%s: %q, want %q", in, got, want)
		}
	}
}

func TestRestoreTestChecksSQLite(t *testing.T) {
	a := &apps.App{Name: "vault", Volumes: []apps.Volume{{Name: "data", Path: "/data"}}, BackupRequire: []string{"data/db.sqlite3"}}
	plain := restoreTestJob(a, &apps.BackupRun{ID: "t1", Kind: "test", Snapshot: "abc"}, 3, 0)
	if strings.Contains(js(plain), sqliteImage) {
		t.Fatal("no SQLite declared: no SQLite check")
	}
	a.BackupSQLite = []apps.SQLiteCheck{{Path: "data/db.sqlite3", Tables: []string{"users", "ciphers"}}, {Path: "data/other.db"}}
	job := restoreTestJob(a, &apps.BackupRun{ID: "t1", Kind: "test", Snapshot: "abc"}, 3, 0)
	inits := job["spec"].(kube.Obj)["template"].(kube.Obj)["spec"].(kube.Obj)["initContainers"].([]any)
	if len(inits) != 2 {
		t.Fatalf("restore, then the SQLite check: %d init containers", len(inits))
	}
	sq := inits[1].(kube.Obj)
	j := js(sq)
	for _, want := range []string{`"name":"sqlite"`, `"image":"` + sqliteImage + `"`, `"runAsUser":65534`, `"value":"data/db.sqlite3:users,ciphers data/other.db:"`, `"value":"/restore/vol"`, `"mountPath":"/restore"`, "PRAGMA integrity_check"} {
		if !strings.Contains(j, want) {
			t.Errorf("sqlite check missing %s", want)
		}
	}
	if _, ok := sq["envFrom"]; ok {
		t.Error("the SQLite check must not get the backup credentials")
	}
	if !apps.DigestPinned(sqliteImage) {
		t.Error("check image must be pinned by digest")
	}
}

// TestSQLiteCheckScript runs the restore test's SQLite check with a local Node against
// a good database, a fresh one without accounts, one with uncommitted WAL pages, and
// damaged ones.
func TestSQLiteCheckScript(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil || exec.Command(node, "-e", `require("node:sqlite")`).Run() != nil {
		t.Skip("needs node with node:sqlite (22.13 or later)")
	}
	root := t.TempDir()
	mk := func(dir, js string) {
		t.Helper()
		os.MkdirAll(filepath.Join(root, dir), 0o755)
		cmd := exec.Command(node, "-e", `const { DatabaseSync } = require("node:sqlite"); const fs = require("node:fs");`+js)
		cmd.Dir = filepath.Join(root, dir)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("%s: %v %s", dir, err, out)
		}
	}
	schema := `const db = new DatabaseSync("db.sqlite3"); db.exec("create table users (uuid text primary key, email text); create table ciphers (uuid text primary key, data text)");`
	mk("good", schema+`for (let i = 0; i < 300; i++) db.exec("insert into ciphers values ('c" + i + "', '" + "x".repeat(200) + "')"); db.exec("insert into users values ('u1', 'a@example.com')"); db.close();`)
	mk("fresh", schema+`db.close();`)
	// The app was stopped hard: committed rows are only in the write-ahead log.
	mk("wal", schema+`db.exec("pragma journal_mode=wal; pragma wal_autocheckpoint=0"); db.exec("insert into users values ('u1', 'a'), ('u2', 'b')");
		for (const s of ["", "-wal"]) fs.copyFileSync("db.sqlite3" + s, "snap.sqlite3" + s); db.close(); fs.renameSync("snap.sqlite3", "db.sqlite3"); fs.renameSync("snap.sqlite3-wal", "db.sqlite3-wal"); fs.rmSync("db.sqlite3-shm", {force: true});`)
	if fi, err := os.Stat(filepath.Join(root, "wal", "db.sqlite3-wal")); err != nil || fi.Size() == 0 {
		t.Fatal("wal fixture needs a non-empty -wal file")
	}
	mk("notables", `const db = new DatabaseSync("db.sqlite3"); db.exec("create table users (uuid text)"); db.close();`)
	// A copy of the good database with one b-tree page overwritten.
	good, _ := os.ReadFile(filepath.Join(root, "good", "db.sqlite3"))
	if len(good) < 6*4096 {
		t.Fatalf("good database too small to damage: %d bytes", len(good))
	}
	bad := append([]byte{}, good...)
	for i := 3 * 4096; i < 4*4096; i++ {
		bad[i] = 0xff
	}
	os.MkdirAll(filepath.Join(root, "corrupt"), 0o755)
	os.WriteFile(filepath.Join(root, "corrupt", "db.sqlite3"), bad, 0o644)
	// Subtle damage that opening and reading the table does not notice: one email changed
	// in the table but not in its index.
	mk("subtle", `const db = new DatabaseSync("db.sqlite3"); db.exec("create table users (uuid text, email text); create index users_email on users (email); insert into users values ('u1', 'alice@example.com')"); db.close();`)
	sub, _ := os.ReadFile(filepath.Join(root, "subtle", "db.sqlite3"))
	if i := bytes.Index(sub, []byte("alice@")); i < 0 || bytes.Count(sub, []byte("alice@")) != 2 {
		t.Fatal("subtle fixture: expected the email in the table and in the index")
	} else {
		sub[i] = 'A'
	}
	os.WriteFile(filepath.Join(root, "subtle", "db.sqlite3"), sub, 0o644)
	os.MkdirAll(filepath.Join(root, "garbage"), 0o755)
	os.WriteFile(filepath.Join(root, "garbage", "db.sqlite3"), []byte(strings.Repeat("not a database\n", 400)), 0o644)

	run := func(spec string) (map[string]string, []apps.SQLiteResult, bool) {
		t.Helper()
		cmd := exec.Command(node, "-e", sqliteCheckJS)
		cmd.Env = append(os.Environ(), "SB_ROOT="+root, "SB_SQLITE="+spec, "TMPDIR="+t.TempDir())
		out, err := cmd.Output()
		got := map[string]string{}
		for _, line := range strings.Split(string(out), "\n") {
			if k, v, ok := strings.Cut(line, " "); ok && strings.HasPrefix(k, "SB_") {
				got[k] = v
			}
		}
		var rs []apps.SQLiteResult
		if err := json.Unmarshal([]byte(got["SB_SQLITE"]), &rs); err != nil {
			t.Fatalf("%s: no SB_SQLITE in %q", spec, out)
		}
		return got, rs, err == nil
	}
	for spec, want := range map[string]string{
		"good/db.sqlite3:users,ciphers":  "sqlite good/db.sqlite3: integrity ok, rows: ciphers 300, users 1",
		"fresh/db.sqlite3:users,ciphers": "sqlite fresh/db.sqlite3: integrity ok, rows: ciphers 0, users 0",
		"wal/db.sqlite3:users":           "sqlite wal/db.sqlite3: integrity ok, rows: users 2",
		"good/db.sqlite3:":               "sqlite good/db.sqlite3: integrity ok",
	} {
		out, rs, ok := run(spec)
		if !ok || out["SB_ERROR"] != "" || apps.SQLiteSummary(rs) != want {
			t.Errorf("%s: ok=%v error=%q summary %q, want %q", spec, ok, out["SB_ERROR"], apps.SQLiteSummary(rs), want)
		}
	}
	for spec, want := range map[string]string{
		"corrupt/db.sqlite3:users":                 "SQLite corrupt/db.sqlite3 failed PRAGMA integrity_check:",
		"subtle/db.sqlite3:users":                  "SQLite subtle/db.sqlite3 failed PRAGMA integrity_check: row 1 missing from index users_email",
		"garbage/db.sqlite3:users":                 "SQLite garbage/db.sqlite3 failed PRAGMA integrity_check: file is not a database",
		"notables/db.sqlite3:users,ciphers":        "SQLite notables/db.sqlite3 has no table ciphers",
		"missing/db.sqlite3:users":                 "SQLite missing/db.sqlite3 is missing or empty",
		"good/db.sqlite3:users corrupt/db.sqlite3": "SQLite corrupt/db.sqlite3 failed PRAGMA integrity_check:",
	} {
		out, rs, ok := run(spec)
		if ok || !strings.HasPrefix(out["SB_ERROR"], want) {
			t.Errorf("%s: ok=%v error %q, want %q", spec, ok, out["SB_ERROR"], want)
		}
		t.Logf("%s: %s | %s", spec, out["SB_ERROR"], apps.SQLiteSummary(rs))
	}
}

func TestBackupSummaryShowsChecks(t *testing.T) {
	now := time.Now().UTC()
	test := &apps.BackupRun{ID: "t", Kind: "test", Phase: "succeeded", Snapshot: "s1", FinishedAt: now.Format(time.RFC3339),
		SQLite: []apps.SQLiteResult{{Path: "data/db.sqlite3", Integrity: "ok", Tables: map[string]int64{"users": 0}}}}
	a := &apps.App{Name: "vault", Volumes: []apps.Volume{{Name: "data"}}, Backup: &apps.BackupState{
		LastBackup: &apps.BackupRun{Kind: "backup", Phase: "succeeded", Snapshot: "s1", StartedAt: now.Format(time.RFC3339)},
		LastTest:   test,
		History:    []apps.BackupRun{{Snapshot: "s1", StartedAt: now.Format(time.RFC3339)}},
	}}
	if s := backupSummary(a); !strings.Contains(s, "restore test passed") || !strings.Contains(s, "(sqlite data/db.sqlite3: integrity ok, rows: users 0)") {
		t.Errorf("summary %q", s)
	}
	if s := backupShort(a, now); s != "ok, tested (sqlite data/db.sqlite3: integrity ok, rows: users 0), under 1h ago" {
		t.Errorf("short %q", s)
	}
	test.SQLite, test.DBTables, test.DBRows = nil, 4, 120
	if s := backupShort(a, now); s != "ok, tested (database 4 tables, 120 rows), under 1h ago" {
		t.Errorf("short %q", s)
	}
}
