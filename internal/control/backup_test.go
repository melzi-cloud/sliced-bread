package control

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/melzi-cloud/sliced-bread/internal/apps"
)

func js(v any) string { b, _ := json.Marshal(v); return string(b) }

func TestBackupJobs(t *testing.T) {
	a := &apps.App{Name: "vault", Volumes: []apps.Volume{{Name: "data", Path: "/data"}}, BackupRequire: []string{"data/db.sqlite3"}}
	run := &apps.BackupRun{ID: "r1", Kind: "backup"}
	j := js(backupJob(a, run, "node-2"))
	for _, want := range []string{`"claimName":"data-data"`, `"mountPath":"/sb/vol/data"`, `"nodeName":"node-2"`, `"name":"sb-backup-env"`, resticImage, `"namespace":"app-vault"`} {
		if !strings.Contains(j, want) {
			t.Errorf("backup job missing %s", want)
		}
	}
	if strings.Contains(j, postgresImage) {
		t.Error("no database: no dump container")
	}
	a.Database = true
	j = js(backupJob(a, run, ""))
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

func TestBackupDueWindow(t *testing.T) {
	c := &BackupConfig{Hour: 3}
	sched := func(t0 time.Time) time.Time {
		s := time.Date(t0.Year(), t0.Month(), t0.Day(), c.Hour, 0, 0, 0, time.UTC)
		if t0.Before(s) {
			s = s.Add(-24 * time.Hour)
		}
		return s
	}
	if got := sched(time.Date(2026, 11, 7, 2, 0, 0, 0, time.UTC)); got.Day() != 6 {
		t.Fatalf("before 03:00 the window started yesterday, got %v", got)
	}
	if got := sched(time.Date(2026, 11, 7, 4, 0, 0, 0, time.UTC)); got.Day() != 7 {
		t.Fatalf("after 03:00 the window started today, got %v", got)
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
