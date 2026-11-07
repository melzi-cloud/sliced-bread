package apps

import (
	"testing"
	"time"
)

func TestUpdateGate(t *testing.T) {
	req := time.Date(2026, 11, 7, 4, 0, 0, 0, time.UTC)
	fresh := req.Add(-3 * time.Hour)
	ts := func(d time.Duration) string { return req.Add(d).Format(time.RFC3339) }
	ok := BackupRun{ID: "b1", Kind: "backup", Phase: "succeeded", Snapshot: "s1", StartedAt: ts(-time.Hour)}
	old := BackupRun{ID: "b0", Kind: "backup", Phase: "succeeded", Snapshot: "s0", StartedAt: ts(-26 * time.Hour)}
	vol := []Volume{{Name: "data", Path: "/data"}}
	cases := []struct {
		name  string
		app   App
		gate  Gate
		start bool
	}{
		{"no data", App{}, GateOK, false},
		{"never backed up", App{Volumes: vol}, GatePending, true},
		{"fresh and tested", App{Volumes: vol, Backup: &BackupState{History: []BackupRun{ok}, LastBackup: &ok,
			LastTest: &BackupRun{Kind: "test", Phase: "succeeded", Snapshot: "s1"}}}, GateOK, false},
		{"fresh, test running", App{Database: true, Backup: &BackupState{History: []BackupRun{ok}, LastBackup: &ok,
			Running: &BackupRun{Kind: "test", Phase: "running", Snapshot: "s1"}}}, GatePending, false},
		{"fresh, test failed", App{Volumes: vol, Backup: &BackupState{History: []BackupRun{ok}, LastBackup: &ok,
			LastTest: &BackupRun{Kind: "test", Phase: "failed", Snapshot: "s1", Message: "bad"}}}, GateFailed, false},
		{"old backup", App{Volumes: vol, Backup: &BackupState{History: []BackupRun{old}, LastBackup: &old,
			LastTest: &BackupRun{Kind: "test", Phase: "succeeded", Snapshot: "s0"}}}, GatePending, true},
		{"failed before the request is retried", App{Volumes: vol, Backup: &BackupState{History: []BackupRun{old},
			LastBackup: &BackupRun{Kind: "backup", Phase: "failed", StartedAt: ts(-time.Hour)}}}, GatePending, true},
		{"failed after the request", App{Volumes: vol, Backup: &BackupState{History: []BackupRun{old},
			LastBackup: &BackupRun{Kind: "backup", Phase: "failed", StartedAt: ts(time.Minute), Message: "denied"}}}, GateFailed, false},
		{"backup running", App{Volumes: vol, Backup: &BackupState{History: []BackupRun{old}, LastBackup: &old,
			Running: &BackupRun{Kind: "backup", Phase: "running", StartedAt: ts(time.Minute)}}}, GatePending, false},
	}
	for _, c := range cases {
		g, why := UpdateGate(&c.app, fresh, req)
		if g != c.gate {
			t.Errorf("%s: gate %s (%s), want %s", c.name, g, why, c.gate)
		}
		if s := NeedsBackupForUpdates(&c.app, fresh, req); s != c.start {
			t.Errorf("%s: start backup %v, want %v", c.name, s, c.start)
		}
	}
}
