package control

import (
	"context"
	"encoding/json"
	"io"
	"strings"
	"testing"
	"time"
)

func TestAlertLifecycle(t *testing.T) {
	st := &alertState{}
	t0 := time.Unix(1_800_000_000, 0)
	down := condition{Alert{ID: "app-down/x", Severity: "critical", Title: "x is down"}, 15 * time.Minute}
	fail := condition{Alert{ID: "backup-failed/y", Severity: "critical", Title: "backup failed"}, 0}
	opened, _ := updateAlerts(st, []condition{down, fail}, t0)
	if len(opened) != 1 || opened[0].ID != "backup-failed/y" {
		t.Fatalf("only zero-grace alerts open at once: %v", opened)
	}
	opened, _ = updateAlerts(st, []condition{down, fail}, t0.Add(16*time.Minute))
	if len(opened) != 1 || opened[0].ID != "app-down/x" {
		t.Fatalf("grace elapsed: %v", opened)
	}
	if got := st.openAlerts(); len(got) != 2 {
		t.Fatalf("open %v", got)
	}
	st.Entries["backup-failed/y"].Notified = true
	_, cleared := updateAlerts(st, []condition{down}, t0.Add(17*time.Minute))
	if len(cleared) != 1 || cleared[0].ID != "backup-failed/y" {
		t.Fatalf("cleared %v", cleared)
	}
	// A condition that flaps before its grace never opens and never notifies.
	_, cleared = updateAlerts(st, nil, t0.Add(18*time.Minute))
	if len(cleared) != 0 {
		t.Fatalf("un-notified alerts clear silently: %v", cleared)
	}
	updateAlerts(st, []condition{down}, t0.Add(19*time.Minute))
	if o, _ := updateAlerts(st, nil, t0.Add(20*time.Minute)); len(o) != 0 || len(st.Entries) != 0 {
		t.Fatal("short blip must not open")
	}
}

func TestWebhookFormats(t *testing.T) {
	a := Alert{ID: "backup-failed/vault", Severity: "critical", Title: "backup of vault failed", Detail: "boom", Hint: "sb backups vault"}
	for format, want := range map[string]string{
		"json":    `"event":"opened"`,
		"slack":   `"text":"[ALERT] d.example: backup of vault failed`,
		"discord": `"content":"[ALERT] d.example: backup of vault failed`,
		"ntfy":    "[ALERT] d.example: backup of vault failed\nboom\nNext: sb backups vault",
	} {
		req, err := webhookRequest(context.Background(), &WebhookConfig{URL: "https://hook.example/x", Format: format}, "d.example", "opened", a)
		if err != nil {
			t.Fatal(err)
		}
		b, _ := io.ReadAll(req.Body)
		if !strings.Contains(string(b), want) {
			t.Errorf("%s body %s", format, b)
		}
		if format == "ntfy" && req.Header.Get("Priority") != "high" {
			t.Error("ntfy critical alerts are high priority")
		}
		if format == "json" {
			var v map[string]any
			if json.Unmarshal(b, &v) != nil || v["install"] != "d.example" {
				t.Errorf("json body %s", b)
			}
		}
	}
	if (WebhookConfig{URL: "ftp://x", Format: "json"}).validate() == nil || (WebhookConfig{URL: "https://x", Format: "teams"}).validate() == nil {
		t.Fatal("bad webhook config accepted")
	}
}
