package apps

import "time"

// Backups before OS updates. Before the nightly OS maintenance installs packages, every
// app with data needs a successful backup that started after freshSince and whose restore
// test passed. sb-guard asks for the missing ones (at requested) and sb-control starts them.

// Gate is an app's readiness for OS updates.
type Gate string

const (
	GateOK      Gate = "ok"
	GatePending Gate = "pending"
	GateFailed  Gate = "failed"
)

func startedAt(r *BackupRun) time.Time {
	if r == nil {
		return time.Time{}
	}
	t, _ := time.Parse(time.RFC3339, r.StartedAt)
	return t
}

// UpdateGate reports whether an app has a fresh, tested backup. A backup or restore test
// that failed after requested makes the gate fail; older failures are retried.
func UpdateGate(a *App, freshSince, requested time.Time) (Gate, string) {
	if !a.HasData() {
		return GateOK, ""
	}
	b := a.Backup
	if b != nil && len(b.History) > 0 && !startedAt(&b.History[0]).Before(freshSince) {
		latest := b.History[0]
		if t := b.LastTest; t != nil && t.Snapshot == latest.Snapshot {
			switch t.Phase {
			case "succeeded":
				return GateOK, "backup " + latest.StartedAt + " restore-tested"
			case "failed":
				return GateFailed, "restore test of the backup from " + latest.StartedAt + " failed: " + t.Message
			}
		}
		return GatePending, "restore test of the new backup is running"
	}
	if b != nil && b.LastBackup != nil && b.LastBackup.Phase == "failed" && !startedAt(b.LastBackup).Before(requested) {
		return GateFailed, "backup failed: " + b.LastBackup.Message
	}
	if b != nil && b.Running != nil {
		return GatePending, b.Running.Kind + " running"
	}
	return GatePending, "backup needed"
}

// NeedsBackupForUpdates reports whether sb-control should start a backup for an open
// request: the app has data, no fresh backup, nothing running, and no backup started
// since the request (a failed one is not retried within the same request).
func NeedsBackupForUpdates(a *App, freshSince, requested time.Time) bool {
	if g, _ := UpdateGate(a, freshSince, requested); g != GatePending {
		return false
	}
	b := a.Backup
	if b == nil {
		return true
	}
	if b.Running != nil {
		return false
	}
	if len(b.History) > 0 && !startedAt(&b.History[0]).Before(freshSince) {
		return false // fresh backup, its restore test is pending
	}
	return b.LastBackup == nil || startedAt(b.LastBackup).Before(requested)
}
