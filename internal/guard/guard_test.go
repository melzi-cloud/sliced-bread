package guard

import (
	"strings"
	"testing"
	"time"

	"github.com/melzi-cloud/sliced-bread/internal/apps"
)

func TestDecide(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	cases := []struct {
		status  string
		down    time.Duration
		resetAt time.Time
		want    Decision
	}{
		{"off", 10 * time.Second, time.Time{}, Wait},
		{"off", 40 * time.Second, time.Time{}, Replace},
		{"", 40 * time.Second, time.Time{}, Replace},
		{"running", 2 * time.Minute, time.Time{}, Wait},
		{"running", 6 * time.Minute, time.Time{}, Reset},
		{"running", 10 * time.Minute, now.Add(-3 * time.Minute), Wait},
		{"running", 16 * time.Minute, now.Add(-9 * time.Minute), Replace},
	}
	for i, c := range cases {
		if got := Decide(c.status, c.down, c.resetAt, now); got != c.want {
			t.Errorf("case %d: got %s want %s", i, got, c.want)
		}
	}
}

func TestNodeViews(t *testing.T) {
	v := nodeViews([]map[string]any{{
		"metadata": map[string]any{"name": "sb-r-s2"},
		"status": map[string]any{
			"conditions": []any{map[string]any{"type": "Ready", "status": "Unknown", "lastTransitionTime": "2026-11-07T03:00:00Z"}},
			"addresses":  []any{map[string]any{"type": "InternalIP", "address": "10.0.1.3"}},
		},
	}, {"metadata": map[string]any{"name": "no-status"}}})
	if len(v) != 2 || v[1].Ready || v[1].InternalIP != "10.0.1.3" || v[1].Since.IsZero() {
		t.Fatalf("%+v", v)
	}
}

func TestBackupsVerdict(t *testing.T) {
	req := time.Date(2026, 11, 7, 4, 0, 0, 0, time.UTC)
	fresh := req.Add(-BackupFreshness)
	ok := apps.BackupRun{Kind: "backup", Phase: "succeeded", Snapshot: "s1", StartedAt: "2026-11-07T03:00:05Z"}
	list := []*apps.App{
		{Name: "site"}, // no data: never waited for
		{Name: "vault", Volumes: []apps.Volume{{Name: "data"}}, Backup: &apps.BackupState{History: []apps.BackupRun{ok}, LastBackup: &ok,
			LastTest: &apps.BackupRun{Kind: "test", Phase: "succeeded", Snapshot: "s1"}}},
		{Name: "crm", Database: true},
	}
	failed, waiting := backupsVerdict(list, fresh, req)
	if len(failed) != 0 || len(waiting) != 1 || waiting[0] != "crm: backup needed" {
		t.Fatalf("failed %v waiting %v", failed, waiting)
	}
	list[2].Backup = &apps.BackupState{LastBackup: &apps.BackupRun{Kind: "backup", Phase: "failed", StartedAt: "2026-11-07T04:00:30Z", Message: "access denied"}}
	failed, _ = backupsVerdict(list, fresh, req)
	if len(failed) != 1 || failed[0] != "crm: backup failed: access denied" {
		t.Fatalf("failed %v", failed)
	}
}

func TestMaintenanceDue(t *testing.T) {
	at := func(s string) time.Time { x, _ := time.Parse(time.RFC3339, s); return x }
	cases := []struct {
		st   MaintState
		hour int
		now  string
		want bool
	}{
		{MaintState{}, 4, "2026-11-07T04:00:05Z", true},
		{MaintState{}, 4, "2026-11-07T03:59:00Z", false},
		{MaintState{}, 4, "2026-11-07T06:00:00Z", false},
		{MaintState{LastRun: "2026-11-07T04:00:05Z"}, 4, "2026-11-07T04:30:00Z", false},
		{MaintState{LastRun: "2026-11-07T04:00:05Z", Requested: "2026-11-07T10:00:00Z"}, 4, "2026-11-07T10:00:05Z", true},
	}
	for i, c := range cases {
		if got := maintenanceDue(&c.st, c.hour, at(c.now)); got != c.want {
			t.Errorf("case %d: %v, want %v", i, got, c.want)
		}
	}
}

func TestHostScripts(t *testing.T) {
	for _, want := range []string{"needrestart", `$nrconf{restart} = 'a';`, `APT::Periodic::Unattended-Upgrade "0";`,
		"unattended-upgrade --dry-run", "SB_PENDING", `Automatic-Reboot "false"`, "qr(^k3s)"} {
		if !strings.Contains(checkScript, want) {
			t.Errorf("check script missing %s", want)
		}
	}
	for _, want := range []string{"NEEDRESTART_SUSPEND=1 unattended-upgrade", "needrestart -r a", "SB_RESTARTED", "/run/reboot-required"} {
		if !strings.Contains(installScript, want) {
			t.Errorf("install script missing %s", want)
		}
	}
	if strings.Contains(checkScript+installScript, "SlicedBreadTest") {
		t.Error("the test repository's origin is allowed only by the e2e test, never by product code")
	}
}
