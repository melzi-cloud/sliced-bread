package guard

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/hetznercloud/hcloud-go/v2/hcloud"

	"github.com/melzi-cloud/sliced-bread/internal/apps"
	"github.com/melzi-cloud/sliced-bread/internal/kube"
	"github.com/melzi-cloud/sliced-bread/internal/labels"
	"github.com/melzi-cloud/sliced-bread/internal/schedule"
	"github.com/melzi-cloud/sliced-bread/internal/tenant"
)

// OS maintenance. Once a day, an hour after the first backup of the day (or when an admin
// asks), the guard checks each server: a privileged one-off Job installs and configures
// unattended-upgrades and needrestart, refreshes the package lists and asks
// unattended-upgrade which security updates it would install. If any server has updates
// waiting, every app with data must first have a backup from the last 3 hours whose
// restore test passed; the guard asks sb-control for the missing ones (the sb-backup-request
// ConfigMap) and waits. A failed backup postpones the night's updates (an alert says why).
// Then, one server at a time, the guard runs unattended-upgrade itself, needrestart
// restarts services still using replaced libraries, and a server that needs a reboot (a new
// kernel) is rebooted the same night: cordon, move stateless apps by starting their
// replacement elsewhere first, move the rest, reboot, wait for Ready, uncordon. On 1-node
// installs the reboot happens in the same window and apps are down for a minute or two.

const (
	opsNamespace      = "sb-node-ops"
	maintConfigMap    = "sb-maintenance"
	opsImage          = "debian:trixie-20261104-slim"
	rebootReadyWait   = 15 * time.Minute
	settleAfterReboot = 2 * time.Minute
	maintMarker       = "slicedbread.dev/maintenance"
	maintBoot         = "slicedbread.dev/maintenance-boot"

	// BackupRequestConfigMap is where the guard asks sb-control for backups before updates.
	BackupRequestConfigMap = "sb-backup-request"
	// BackupFreshness is how recent a backup must be before updates are installed.
	BackupFreshness = 3 * time.Hour
	// backupWaitMax is how long the guard waits for the backups before it postpones.
	backupWaitMax = 3 * time.Hour

	phaseChecking = "checking"
	phaseBackups  = "backups"
	phaseUpdating = "updating"
)

// MaintState is kept in the sb-maintenance ConfigMap.
type MaintState struct {
	Requested   string               `json:"requested,omitempty"` // set by the dashboard: run now
	LastRun     string               `json:"lastRun,omitempty"`
	Running     bool                 `json:"running,omitempty"`
	Phase       string               `json:"phase,omitempty"` // checking, backups, updating
	Backups     *BackupWait          `json:"backups,omitempty"`
	Postponed   *Postponed           `json:"postponed,omitempty"`   // the last run did not install updates
	LastUpdated string               `json:"lastUpdated,omitempty"` // last run that finished installing
	Nodes       map[string]NodeMaint `json:"nodes,omitempty"`
	Log         []string             `json:"log,omitempty"`
}

// BackupWait is the backups-before-updates step of a run.
type BackupWait struct {
	RequestedAt string   `json:"requestedAt"`
	FreshSince  string   `json:"freshSince"`
	Waiting     []string `json:"waiting,omitempty"` // "app: why"
	DoneAt      string   `json:"doneAt,omitempty"`
}

// Postponed says why a run installed nothing.
type Postponed struct {
	At     string `json:"at"`
	Reason string `json:"reason"`
}

// NodeMaint is the last result for one server.
type NodeMaint struct {
	CheckedAt     string `json:"checkedAt"`
	RebootNeeded  bool   `json:"rebootNeeded"`
	RebootFor     string `json:"rebootFor,omitempty"` // packages that asked for the reboot
	Upgradable    int    `json:"upgradable"`
	Pending       int    `json:"pending"`                 // security updates waiting; -1 unknown
	PendingPkgs   string `json:"pendingPkgs,omitempty"`   // their names
	Installed     string `json:"installed,omitempty"`     // packages the last run installed
	InstalledAt   string `json:"installedAt,omitempty"`   // when
	Restarted     string `json:"restarted,omitempty"`     // services needrestart restarted
	Kernel        string `json:"kernel,omitempty"`        //
	LastRebootAt  string `json:"lastRebootAt,omitempty"`  //
	LastRebootFor string `json:"lastRebootFor,omitempty"` // how long the server was away
	Error         string `json:"error,omitempty"`
}

// BackupRequest asks sb-control to back up every app with data that has no backup since
// FreshSince. sb-control's leader starts the missing backups (each followed by its
// restore test); the guard watches the App resources and deletes the request when done.
type BackupRequest struct {
	RequestedAt string `json:"requestedAt"`
	FreshSince  string `json:"freshSince"`
	Reason      string `json:"reason"`
}

// Times returns the request's times.
func (r *BackupRequest) Times() (freshSince, requested time.Time) {
	freshSince, _ = time.Parse(time.RFC3339, r.FreshSince)
	requested, _ = time.Parse(time.RFC3339, r.RequestedAt)
	return
}

// LoadBackupRequest returns the open request, or nil.
func LoadBackupRequest(ctx context.Context, k *kube.Client) *BackupRequest {
	var cm kube.Obj
	if err := k.Get(ctx, "v1", "ConfigMap", ns, BackupRequestConfigMap, &cm); err != nil {
		return nil
	}
	var r BackupRequest
	if json.Unmarshal([]byte(kube.Str(cm, "data", "request.json")), &r) != nil || r.RequestedAt == "" {
		return nil
	}
	return &r
}

func (g *Guard) loadMaint(ctx context.Context) *MaintState {
	st := &MaintState{Nodes: map[string]NodeMaint{}}
	var cm kube.Obj
	if err := g.Kube.Get(ctx, "v1", "ConfigMap", ns, maintConfigMap, &cm); err == nil {
		json.Unmarshal([]byte(kube.Str(cm, "data", "state.json")), st)
		// "Run now" is a separate key written with a merge patch, so the guard saving its
		// state (server-side apply of state.json only) never drops a request.
		if r := kube.Str(cm, "data", "requested"); r > st.Requested {
			st.Requested = r
		}
	}
	if st.Nodes == nil {
		st.Nodes = map[string]NodeMaint{}
	}
	return st
}

func (g *Guard) saveMaint(ctx context.Context, st *MaintState) error {
	if len(st.Log) > 60 {
		st.Log = st.Log[len(st.Log)-60:]
	}
	b, _ := json.Marshal(st)
	return g.Kube.Apply(ctx, kube.Obj{"apiVersion": "v1", "kind": "ConfigMap",
		"metadata": kube.Obj{"name": maintConfigMap, "namespace": ns}, "data": kube.Obj{"state.json": string(b)}})
}

func (g *Guard) mlog(st *MaintState, format string, a ...any) {
	m := fmt.Sprintf(format, a...)
	g.Logf("sb-guard: %s", m)
	st.Log = append(st.Log, time.Now().UTC().Format(time.RFC3339)+" "+m)
}

// LoadMaint reads the maintenance state (for sb-control's dashboard and alerts).
func LoadMaint(ctx context.Context, k *kube.Client) *MaintState {
	return (&Guard{Kube: k}).loadMaint(ctx)
}

// RequestMaintenance asks the guard to run a maintenance cycle now. It takes the same path
// as the nightly run, backups first.
func RequestMaintenance(ctx context.Context, k *kube.Client) error {
	now := time.Now().UTC().Format(time.RFC3339)
	err := k.MergePatch(ctx, "v1", "ConfigMap", ns, maintConfigMap, map[string]any{"data": map[string]any{"requested": now}})
	if kube.IsNotFound(err) {
		st := &MaintState{Requested: now}
		b, _ := json.Marshal(st)
		return k.Apply(ctx, kube.Obj{"apiVersion": "v1", "kind": "ConfigMap",
			"metadata": kube.Obj{"name": maintConfigMap, "namespace": ns}, "data": kube.Obj{"state.json": string(b)}})
	}
	return err
}

// backupSettings reads the backup schedule and whether a bucket is configured.
func (g *Guard) backupSettings(ctx context.Context) (schedule.Backup, bool) {
	var sec kube.Obj
	if err := g.Kube.Get(ctx, "v1", "Secret", ns, "sb-backup", &sec); err != nil {
		return schedule.Default(), false
	}
	d := func(k string) string {
		b, _ := base64.StdEncoding.DecodeString(kube.Str(sec, "data", k))
		return string(b)
	}
	return schedule.FromStrings(d("hour"), d("every"), d("weekday")), d("endpoint") != "" && d("bucket") != ""
}

// maintenanceDue reports whether a cycle should start now.
func maintenanceDue(st *MaintState, hour int, now time.Time) bool {
	last, _ := time.Parse(time.RFC3339, st.LastRun)
	if req, err := time.Parse(time.RFC3339, st.Requested); err == nil && req.After(last) {
		return true
	}
	window := time.Date(now.Year(), now.Month(), now.Day(), hour, 0, 0, 0, time.UTC)
	return !now.Before(window) && now.Before(window.Add(2*time.Hour)) && last.Before(window)
}

// maybeMaintain starts a maintenance cycle when due, or advances one waiting for backups.
// Checking and installing block the guard loop while they run (node repair waits;
// maintenance only starts when all nodes are Ready); waiting for backups does not.
func (g *Guard) maybeMaintain(ctx context.Context, info *tenant.ClusterInfo, nodes []nodeView) {
	st := g.loadMaint(ctx)
	if st.Running && st.Phase == phaseBackups {
		g.continueBackups(ctx, info, nodes, st)
		return
	}
	if st.Running && !g.inMaint && !g.rebootInProgress(ctx) {
		// A guard stopped in the middle of a run (not by rebooting its own server, which
		// finishLeftovers completes). The next run starts over.
		g.endRun(ctx, st, "maintenance interrupted (the guard restarted); it runs again at the next window or request")
	}
	sched, configured := g.backupSettings(ctx)
	if !maintenanceDue(st, sched.MaintenanceHour(), time.Now().UTC()) {
		return
	}
	for _, n := range nodes {
		if !n.Ready {
			g.say("sb-guard: maintenance waits: %s is NotReady", n.Name)
			return
		}
	}
	st.Running, st.LastRun, st.Phase, st.Backups = true, time.Now().UTC().Format(time.RFC3339), phaseChecking, nil
	g.mlog(st, "maintenance started (%d servers)", len(nodes))
	g.saveMaint(ctx, st)
	if err := g.ensureOpsNamespace(ctx); err != nil {
		g.endRun(ctx, st, "maintenance stopped: %v", err)
		return
	}
	g.inMaint = true
	defer func() { g.inMaint = false }()
	pending := g.checkNodes(ctx, nodes, st)
	if len(pending) == 0 {
		g.mlog(st, "no security updates waiting")
		g.applyUpdates(ctx, info, nodes, st)
		return
	}
	g.mlog(st, "security updates waiting on %s", strings.Join(pending, ", "))
	if !configured {
		g.mlog(st, "backups are not configured, so updates install without a backup first")
		g.applyUpdates(ctx, info, nodes, st)
		return
	}
	now := time.Now().UTC()
	req := BackupRequest{RequestedAt: now.Format(time.RFC3339), FreshSince: now.Add(-BackupFreshness).Format(time.RFC3339),
		Reason: "OS updates waiting on " + strings.Join(pending, ", ")}
	if err := g.writeBackupRequest(ctx, req); err != nil {
		g.endRun(ctx, st, "maintenance stopped: could not ask for backups: %v", err)
		return
	}
	st.Phase, st.Backups = phaseBackups, &BackupWait{RequestedAt: req.RequestedAt, FreshSince: req.FreshSince}
	g.mlog(st, "waiting for a backup from the last %s with a passed restore test of every app with data", BackupFreshness)
	g.saveMaint(ctx, st)
	g.inMaint = false
	g.continueBackups(ctx, info, nodes, st)
}

// rebootInProgress reports whether a server carries the planned-reboot marker.
func (g *Guard) rebootInProgress(ctx context.Context) bool {
	list, err := g.Kube.List(ctx, "v1", "Node", "", "")
	if err != nil {
		return true // cannot tell; leave the state alone
	}
	for _, n := range list {
		if kube.Str(n, "metadata", "annotations", maintMarker) != "" {
			return true
		}
	}
	return false
}

// endRun stops a run with a message.
func (g *Guard) endRun(ctx context.Context, st *MaintState, format string, a ...any) {
	g.mlog(st, format, a...)
	st.Running, st.Phase = false, ""
	g.saveMaint(ctx, st)
}

func (g *Guard) writeBackupRequest(ctx context.Context, r BackupRequest) error {
	b, _ := json.Marshal(r)
	return g.Kube.Apply(ctx, kube.Obj{"apiVersion": "v1", "kind": "ConfigMap",
		"metadata": kube.Obj{"name": BackupRequestConfigMap, "namespace": ns, "labels": kube.Obj{"app.kubernetes.io/managed-by": "slicedbread"}},
		"data":     kube.Obj{"request.json": string(b)}})
}

func (g *Guard) clearBackupRequest(ctx context.Context) {
	if err := g.Kube.Delete(ctx, "v1", "ConfigMap", ns, BackupRequestConfigMap); err != nil && !kube.IsNotFound(err) {
		g.Logf("sb-guard: delete backup request: %v", err)
	}
}

func (g *Guard) listApps(ctx context.Context) ([]*apps.App, error) {
	items, err := g.Kube.List(ctx, "slicedbread.dev/v1alpha1", "App", "", "")
	if err != nil {
		return nil, err
	}
	var out []*apps.App
	for _, it := range items {
		b, _ := json.Marshal(it["spec"])
		var a apps.App
		if json.Unmarshal(b, &a) == nil {
			a.Name = kube.Str(it, "metadata", "name")
			out = append(out, &a)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

// backupsVerdict sums up the apps' gates: failed reasons and the apps still waiting.
func backupsVerdict(list []*apps.App, freshSince, requested time.Time) (failed, waiting []string) {
	for _, a := range list {
		switch g, why := apps.UpdateGate(a, freshSince, requested); g {
		case apps.GateFailed:
			failed = append(failed, a.Name+": "+why)
		case apps.GatePending:
			waiting = append(waiting, a.Name+": "+why)
		}
	}
	return
}

// continueBackups checks the backups a run waits for; once every app with data has a
// fresh tested backup it installs the updates.
func (g *Guard) continueBackups(ctx context.Context, info *tenant.ClusterInfo, nodes []nodeView, st *MaintState) {
	bw := st.Backups
	if bw == nil {
		g.endRun(ctx, st, "maintenance stopped: lost track of the backups")
		return
	}
	requested, _ := time.Parse(time.RFC3339, bw.RequestedAt)
	freshSince, _ := time.Parse(time.RFC3339, bw.FreshSince)
	list, err := g.listApps(ctx)
	if err != nil {
		g.say("sb-guard: maintenance: list apps: %v", err)
		return
	}
	failed, waiting := backupsVerdict(list, freshSince, requested)
	if len(failed) > 0 {
		g.postpone(ctx, st, "backup failed ("+strings.Join(failed, "; ")+")")
		return
	}
	if len(waiting) > 0 {
		if time.Since(requested) > backupWaitMax {
			g.postpone(ctx, st, fmt.Sprintf("backups did not finish within %s (%s)", backupWaitMax, strings.Join(waiting, "; ")))
			return
		}
		if strings.Join(waiting, "|") != strings.Join(bw.Waiting, "|") {
			bw.Waiting = waiting
			g.saveMaint(ctx, st)
		}
		g.say("sb-guard: maintenance waits for backups: %s", strings.Join(waiting, "; "))
		return
	}
	for _, n := range nodes {
		if !n.Ready {
			g.say("sb-guard: maintenance waits: %s is NotReady", n.Name)
			return
		}
	}
	bw.Waiting, bw.DoneAt = nil, time.Now().UTC().Format(time.RFC3339)
	g.clearBackupRequest(ctx)
	g.mlog(st, "backups done: every app with data has a backup from the last %s with a passed restore test", BackupFreshness)
	g.applyUpdates(ctx, info, nodes, st)
}

// postpone ends a run without installing anything.
func (g *Guard) postpone(ctx context.Context, st *MaintState, reason string) {
	g.clearBackupRequest(ctx)
	st.Postponed = &Postponed{At: time.Now().UTC().Format(time.RFC3339), Reason: reason}
	g.endRun(ctx, st, "updates postponed: %s", reason)
}

// checkNodes runs the check job on every server at once. It returns the servers with
// security updates waiting (or whose check could not tell).
func (g *Guard) checkNodes(ctx context.Context, nodes []nodeView, st *MaintState) []string {
	res := make([]NodeMaint, len(nodes))
	var wg sync.WaitGroup
	for i, n := range nodes {
		wg.Add(1)
		go func(i int, node string) {
			defer wg.Done()
			res[i] = g.checkNode(ctx, node)
		}(i, n.Name)
	}
	wg.Wait()
	var pending []string
	for i, n := range nodes {
		r, prev := res[i], st.Nodes[n.Name]
		r.LastRebootAt, r.LastRebootFor = prev.LastRebootAt, prev.LastRebootFor
		r.Installed, r.InstalledAt, r.Restarted = prev.Installed, prev.InstalledAt, prev.Restarted
		st.Nodes[n.Name] = r
		switch {
		case r.Error != "":
			g.mlog(st, "%s: check failed: %s", n.Name, r.Error)
		case r.Pending != 0:
			pending = append(pending, n.Name)
			what := r.PendingPkgs
			if r.Pending < 0 {
				what = "unknown (treated as waiting)"
			}
			g.mlog(st, "%s: updates waiting: %s", n.Name, what)
		}
	}
	g.saveMaint(ctx, st)
	return pending
}

// applyUpdates installs the waiting updates one server at a time and reboots the servers
// that need it. The guard's own server goes last: its reboot ends this process, and the
// guard that comes up elsewhere finishes it (see finishLeftovers).
func (g *Guard) applyUpdates(ctx context.Context, info *tenant.ClusterInfo, nodes []nodeView, st *MaintState) {
	g.inMaint = true
	defer func() { g.inMaint = false }()
	st.Phase, st.Postponed = phaseUpdating, nil
	g.saveMaint(ctx, st)
	ordered := append([]nodeView{}, nodes...)
	sort.SliceStable(ordered, func(i, j int) bool { return ordered[j].Name == os.Getenv("NODE_NAME") })
	clean := true
	for _, n := range ordered {
		res := st.Nodes[n.Name]
		if res.Error != "" {
			clean = false
			continue
		}
		if res.Pending != 0 {
			g.mlog(st, "%s: installing updates", n.Name)
			g.saveMaint(ctx, st)
			res = g.installNode(ctx, n.Name, res)
			st.Nodes[n.Name] = res
			if res.Error != "" {
				clean = false
				g.mlog(st, "%s: installing updates failed: %s", n.Name, res.Error)
				g.saveMaint(ctx, st)
				continue
			}
			g.mlog(st, "%s: installed %s; services restarted by needrestart: %s", n.Name, orNone(res.Installed), orNone(res.Restarted))
			g.saveMaint(ctx, st)
		}
		if !res.RebootNeeded {
			g.mlog(st, "%s: up to date, no reboot needed (%d upgradable packages left for apt)", n.Name, res.Upgradable)
			continue
		}
		g.mlog(st, "%s: reboot needed (for %s), rebooting", n.Name, orNone(res.RebootFor))
		g.saveMaint(ctx, st)
		if n.Name == os.Getenv("NODE_NAME") && clean {
			// The reboot may end this process; record a finished install first.
			st.LastUpdated = time.Now().UTC().Format(time.RFC3339)
		}
		took, err := g.rebootNode(ctx, info, n.Name, st)
		res = st.Nodes[n.Name]
		if err != nil {
			res.Error = err.Error()
			st.Nodes[n.Name] = res
			g.mlog(st, "%s: maintenance stopped: %v", n.Name, err)
			clean = false
			break // never take a second server down after a failure
		}
		res.RebootNeeded, res.LastRebootAt, res.LastRebootFor = false, time.Now().UTC().Format(time.RFC3339), took.Round(time.Second).String()
		st.Nodes[n.Name] = res
		g.mlog(st, "%s: rebooted and back after %s", n.Name, took.Round(time.Second))
		g.saveMaint(ctx, st)
	}
	if clean {
		st.LastUpdated = time.Now().UTC().Format(time.RFC3339)
	}
	g.endRun(ctx, st, "maintenance finished")
}

func orNone(s string) string {
	if strings.TrimSpace(s) == "" {
		return "none"
	}
	return s
}

func (g *Guard) ensureOpsNamespace(ctx context.Context) error {
	return g.Kube.Apply(ctx, kube.Obj{"apiVersion": "v1", "kind": "Namespace", "metadata": kube.Obj{"name": opsNamespace,
		"labels": kube.Obj{"pod-security.kubernetes.io/enforce": "privileged", "app.kubernetes.io/managed-by": "slicedbread"}}})
}

// hostScriptPrelude is shared by the host scripts.
const hostScriptPrelude = `set -e
export DEBIAN_FRONTEND=noninteractive LANG=C LC_ALL=C
aptq() { apt-get -o DPkg::Lock::Timeout=600 -qq "$@"; }
report() {
  echo "SB_UPGRADABLE $(apt list --upgradable 2>/dev/null | grep -c upgradable || true)"
  echo "SB_KERNEL $(uname -r)"
  if [ -f /run/reboot-required ]; then echo "SB_REBOOT yes"; else echo "SB_REBOOT no"; fi
  if [ -f /run/reboot-required.pkgs ]; then echo "SB_REBOOT_PKGS $(sort -u /run/reboot-required.pkgs | tr '\n' ' ')"; fi
}
`

// checkScript installs and configures unattended-upgrades and needrestart, refreshes the
// package lists and reports which security updates unattended-upgrade would install.
// Nothing is installed by the system timers: updates only install in the guard's nightly
// run, after the backups.
const checkScript = hostScriptPrelude + `aptq update
missing=""
for p in unattended-upgrades needrestart; do dpkg -s "$p" >/dev/null 2>&1 || missing="$missing $p"; done
if [ -n "$missing" ]; then aptq install -y $missing >/dev/null; fi
cat > /etc/apt/apt.conf.d/20auto-upgrades <<'EOF'
// Written by SlicedBread: package lists refresh daily; sb-guard installs security updates
// itself in the nightly maintenance run, after backups.
APT::Periodic::Update-Package-Lists "1";
APT::Periodic::Unattended-Upgrade "0";
EOF
# The guard reboots servers one at a time; apt must never reboot by itself.
echo 'Unattended-Upgrade::Automatic-Reboot "false";' > /etc/apt/apt.conf.d/52sb-no-reboot
mkdir -p /etc/needrestart/conf.d
cat > /etc/needrestart/conf.d/50-slicedbread.conf <<'EOF'
# Written by SlicedBread: restart services that still use replaced libraries, without asking.
$nrconf{restart} = 'a';
$nrconf{kernelhints} = 0;
$nrconf{ucodehints} = 0;
# k3s runs the cluster on this server; it restarts with the server's reboot instead.
$nrconf{override_rc}{qr(^k3s)} = 0;
EOF
systemctl enable --now apt-daily.timer >/dev/null 2>&1 || true
out=$(unattended-upgrade --dry-run -v 2>&1 || true)
if echo "$out" | grep -q "Packages that will be upgraded:"; then
  pk=$(echo "$out" | sed -n 's/.*Packages that will be upgraded: *//p' | tail -1)
  echo "SB_PENDING $(echo $pk | wc -w)"
  echo "SB_PENDING_PKGS $pk"
elif echo "$out" | grep -q "No packages found that can be upgraded"; then
  echo "SB_PENDING 0"
else
  echo "$out" | tail -5
  echo "SB_PENDING unknown"
fi
report
`

// installScript installs the waiting security updates with unattended-upgrade (from the
// lists the check read), then lets needrestart restart services that still use replaced
// files. needrestart's apt hook is suspended during the install so its restarts happen
// once, here, where they are reported.
const installScript = hostScriptPrelude + `log=$(mktemp)
if ! NEEDRESTART_SUSPEND=1 unattended-upgrade -v >"$log" 2>&1; then tail -5 "$log"; echo "SB_ERROR unattended-upgrade failed: $(tail -1 "$log")"; exit 1; fi
echo "SB_INSTALLED $(sed -n 's/.*Packages that will be upgraded: *//p' "$log" | tail -1)"
rm -f "$log"
nr=$(needrestart -r a -u NeedRestart::UI::stdio -l 2>&1 || true)
echo "$nr" | tail -8
echo "SB_RESTARTED $(echo "$nr" | sed -n 's/^ *systemctl restart //p' | tr '\n' ' ')"
echo "SB_STALE $(needrestart -b -l 2>/dev/null | sed -n 's/^NEEDRESTART-SVC: //p' | tr '\n' ' ')"
report
`

// hostJob runs a script in the host's namespaces on one node.
func hostJob(name, node, script string) kube.Obj {
	return kube.Obj{
		"apiVersion": "batch/v1", "kind": "Job",
		"metadata": kube.Obj{"name": name, "namespace": opsNamespace, "labels": kube.Obj{"app.kubernetes.io/managed-by": "slicedbread"}},
		"spec": kube.Obj{
			"backoffLimit": 0, "activeDeadlineSeconds": 1200, "ttlSecondsAfterFinished": 3600,
			"template": kube.Obj{"spec": kube.Obj{
				"restartPolicy": "Never", "nodeName": node, "hostPID": true, "automountServiceAccountToken": false,
				"tolerations": []any{kube.Obj{"operator": "Exists"}},
				"containers": []any{kube.Obj{
					"name": "host", "image": opsImage,
					"command":         []any{"nsenter", "-t", "1", "-m", "-u", "-i", "-n", "-p", "--", "/bin/sh", "-c", script},
					"securityContext": kube.Obj{"privileged": true},
				}},
			}},
		},
	}
}

// runHostJob runs a host job and returns its SB_ lines.
func (g *Guard) runHostJob(ctx context.Context, name, node, script string, timeout time.Duration) (map[string]string, error) {
	if err := g.Kube.Apply(ctx, hostJob(name, node, script)); err != nil {
		return nil, err
	}
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		var j kube.Obj
		if err := g.Kube.Get(ctx, "batch/v1", "Job", opsNamespace, name, &j); err == nil {
			ok, bad := kube.Num(j, "status", "succeeded") > 0, kube.Num(j, "status", "failed") > 0
			if ok || bad {
				out := map[string]string{}
				if pods, err := g.Kube.List(ctx, "v1", "Pod", opsNamespace, "job-name="+name); err == nil && len(pods) > 0 {
					logs, _ := g.Kube.Logs(ctx, opsNamespace, kube.Str(pods[0], "metadata", "name"), "host", 200)
					for _, l := range strings.Split(logs, "\n") {
						if k, v, found := strings.Cut(strings.TrimSpace(l), " "); found && strings.HasPrefix(k, "SB_") {
							out[k] = v
						}
					}
					if bad {
						return out, fmt.Errorf("job %s failed: %s", name, lastLine(logs))
					}
				}
				return out, nil
			}
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(3 * time.Second):
		}
	}
	return nil, fmt.Errorf("job %s did not finish in %s", name, timeout)
}

func lastLine(s string) string {
	lines := strings.Split(strings.TrimSpace(s), "\n")
	return lines[len(lines)-1]
}

func jobID() string { return strconv.FormatInt(time.Now().Unix()%1_000_000, 36) }

func (g *Guard) checkNode(ctx context.Context, node string) NodeMaint {
	res := NodeMaint{CheckedAt: time.Now().UTC().Format(time.RFC3339)}
	out, err := g.runHostJob(ctx, "check-"+shortName(node)+"-"+jobID(), node, checkScript, 15*time.Minute)
	if err != nil {
		res.Error = err.Error()
		return res
	}
	res.RebootNeeded, res.RebootFor = out["SB_REBOOT"] == "yes", strings.TrimSpace(out["SB_REBOOT_PKGS"])
	res.Upgradable, _ = strconv.Atoi(out["SB_UPGRADABLE"])
	res.Kernel = out["SB_KERNEL"]
	if n, err := strconv.Atoi(out["SB_PENDING"]); err == nil {
		res.Pending = n
	} else {
		res.Pending = -1 // the check could not tell: install, after backups
	}
	res.PendingPkgs = out["SB_PENDING_PKGS"]
	return res
}

// installNode installs the waiting updates on one server.
func (g *Guard) installNode(ctx context.Context, node string, res NodeMaint) NodeMaint {
	out, err := g.runHostJob(ctx, "install-"+shortName(node)+"-"+jobID(), node, installScript, 20*time.Minute)
	if err != nil {
		if e := out["SB_ERROR"]; e != "" {
			err = fmt.Errorf("%s", e)
		}
		res.Error = err.Error()
		return res
	}
	res.Pending, res.PendingPkgs = 0, ""
	res.Installed, res.InstalledAt = strings.TrimSpace(out["SB_INSTALLED"]), time.Now().UTC().Format(time.RFC3339)
	res.Restarted = strings.TrimSpace(out["SB_RESTARTED"])
	res.RebootNeeded, res.RebootFor = out["SB_REBOOT"] == "yes", strings.TrimSpace(out["SB_REBOOT_PKGS"])
	res.Upgradable, _ = strconv.Atoi(out["SB_UPGRADABLE"])
	res.Kernel = out["SB_KERNEL"]
	if stale := strings.TrimSpace(out["SB_STALE"]); stale != "" {
		g.Logf("sb-guard: %s: services still using replaced files after needrestart: %s", node, stale)
	}
	return res
}

func shortName(node string) string {
	if i := strings.LastIndex(node, "-"); i >= 0 {
		return node[i+1:]
	}
	return node
}

// underMaintenance reports whether a node is in a planned reboot (node repair leaves it
// alone for that time).
func underMaintenance(n kube.Obj, now time.Time) bool {
	at, err := time.Parse(time.RFC3339, kube.Str(n, "metadata", "annotations", maintMarker))
	return err == nil && now.Sub(at) < rebootReadyWait+5*time.Minute
}

func bootID(n kube.Obj) string { return kube.Str(n, "status", "nodeInfo", "bootID") }

// rebootNode drains a node the gentle way and reboots it. Returns how long it was away.
// A quick reboot may never show as NotReady, so "back" means a new boot ID and Ready.
func (g *Guard) rebootNode(ctx context.Context, info *tenant.ClusterInfo, node string, st *MaintState) (time.Duration, error) {
	var before kube.Obj
	if err := g.Kube.Get(ctx, "v1", "Node", "", node, &before); err != nil {
		return 0, err
	}
	oldBoot := bootID(before)
	if err := g.Kube.MergePatch(ctx, "v1", "Node", "", node, map[string]any{
		"metadata": map[string]any{"annotations": map[string]any{maintMarker: time.Now().UTC().Format(time.RFC3339), maintBoot: oldBoot}},
		"spec":     map[string]any{"unschedulable": true},
	}); err != nil {
		return 0, fmt.Errorf("cordon: %w", err)
	}
	finish := func() { g.endMaintenance(context.Background(), info, node) }
	if info.Size > 1 {
		moved, err := g.moveWorkloads(ctx, node)
		if err != nil {
			finish()
			return 0, err
		}
		g.mlog(st, "%s: moved %d workloads to the other servers", node, moved)
		if err := g.loadBalancerOut(ctx, info, node); err != nil {
			g.mlog(st, "%s: could not take it out of the load balancer first (%v); rebooting anyway", node, err)
		}
	}
	start := time.Now()
	// The job is killed by the reboot itself, so it is not waited for.
	if err := g.Kube.Apply(ctx, hostJob("reboot-"+shortName(node)+"-"+jobID(), node, "sleep 2; systemctl reboot")); err != nil {
		finish()
		return 0, fmt.Errorf("start reboot: %w", err)
	}
	for time.Since(start) < rebootReadyWait {
		select {
		case <-ctx.Done():
			return 0, ctx.Err()
		case <-time.After(5 * time.Second):
		}
		var n kube.Obj
		if err := g.Kube.Get(ctx, "v1", "Node", "", node, &n); err != nil {
			continue
		}
		if b := bootID(n); b != "" && b != oldBoot && nodeViews([]kube.Obj{n})[0].Ready {
			took := time.Since(start)
			// Ready comes before the pod network and service rules have settled; the load
			// balancer only checks that the port accepts connections, so give it a moment.
			time.Sleep(45 * time.Second)
			finish()
			time.Sleep(settleAfterReboot)
			return took, nil
		}
	}
	finish()
	return 0, fmt.Errorf("%s did not come back within %s after the reboot", node, rebootReadyWait)
}

// endMaintenance uncordons a node and puts it back into the load balancer.
func (g *Guard) endMaintenance(ctx context.Context, info *tenant.ClusterInfo, node string) {
	g.Kube.MergePatch(ctx, "v1", "Node", "", node, map[string]any{
		"metadata": map[string]any{"annotations": map[string]any{maintMarker: nil, maintBoot: nil}},
		"spec":     map[string]any{"unschedulable": nil},
	})
	if info.Size > 1 {
		if err := g.loadBalancerIn(ctx, info, node); err != nil {
			g.Logf("sb-guard: %s: back into the load balancer: %v", node, err)
		}
	}
}

// The load balancer targets servers by label (sb/role=server). Changing the label takes
// a server out before its reboot, so no request is sent to it while it is down.
const lbOutRole = "server-maintenance"

func (g *Guard) setRole(ctx context.Context, node, role string) (*hcloud.Server, error) {
	hc, err := g.hcloud(ctx)
	if err != nil {
		return nil, err
	}
	srv, _, err := hc.Server.GetByName(ctx, node)
	if err != nil || srv == nil {
		return nil, fmt.Errorf("server %s: %v", node, err)
	}
	if srv.Labels[labels.Role] == role {
		return srv, nil
	}
	l := map[string]string{}
	for k, v := range srv.Labels {
		l[k] = v
	}
	l[labels.Role] = role
	srv, _, err = hc.Server.Update(ctx, srv, hcloud.ServerUpdateOpts{Labels: l})
	return srv, err
}

// lbTarget reports whether the install's load balancer currently targets the server, and
// whether every service sees it healthy.
func (g *Guard) lbTarget(ctx context.Context, info *tenant.ClusterInfo, id int64) (present, healthy bool, err error) {
	hc, err := g.hcloud(ctx)
	if err != nil {
		return false, false, err
	}
	lbs, err := hc.LoadBalancer.AllWithOpts(ctx, hcloud.LoadBalancerListOpts{ListOpts: hcloud.ListOpts{LabelSelector: labels.Selector(info.RunID)}})
	if err != nil || len(lbs) == 0 {
		return false, false, fmt.Errorf("load balancer: %v", err)
	}
	for _, t := range lbs[0].Targets {
		for _, r := range t.Targets {
			if r.Server != nil && r.Server.Server != nil && r.Server.Server.ID == id {
				healthy = len(r.HealthStatus) > 0
				for _, h := range r.HealthStatus {
					if h.Status != hcloud.LoadBalancerTargetHealthStatusStatusHealthy {
						healthy = false
					}
				}
				return true, healthy, nil
			}
		}
	}
	return false, false, nil
}

func (g *Guard) loadBalancerOut(ctx context.Context, info *tenant.ClusterInfo, node string) error {
	srv, err := g.setRole(ctx, node, lbOutRole)
	if err != nil {
		return err
	}
	deadline := time.Now().Add(90 * time.Second)
	for time.Now().Before(deadline) {
		if present, _, err := g.lbTarget(ctx, info, srv.ID); err == nil && !present {
			time.Sleep(10 * time.Second) // let open connections finish
			return nil
		}
		time.Sleep(3 * time.Second)
	}
	return fmt.Errorf("still a load balancer target after 90 seconds")
}

func (g *Guard) loadBalancerIn(ctx context.Context, info *tenant.ClusterInfo, node string) error {
	srv, err := g.setRole(ctx, node, "server")
	if err != nil {
		return err
	}
	deadline := time.Now().Add(3 * time.Minute)
	for time.Now().Before(deadline) {
		if present, healthy, err := g.lbTarget(ctx, info, srv.ID); err == nil && present && healthy {
			return nil
		}
		time.Sleep(5 * time.Second)
	}
	return fmt.Errorf("not healthy in the load balancer after 3 minutes")
}

// moveWorkloads empties a cordoned node. Deployments that can roll (no ReadWriteOnce
// volume) are restarted with a surge, so their new pod serves elsewhere before the old one
// stops: no downtime. Everything else (Recreate deployments, the database instance) is
// deleted and comes back on another server once its volume moves.
func (g *Guard) moveWorkloads(ctx context.Context, node string) (int, error) {
	pods, err := g.Kube.List(ctx, "v1", "Pod", "", "")
	if err != nil {
		return 0, err
	}
	rsOwner := map[string]string{} // ns/rs -> deployment
	if rss, err := g.Kube.List(ctx, "apps/v1", "ReplicaSet", "", ""); err == nil {
		for _, rs := range rss {
			for _, o := range owners(rs) {
				if o[0] == "Deployment" {
					rsOwner[kube.Str(rs, "metadata", "namespace")+"/"+kube.Str(rs, "metadata", "name")] = o[1]
				}
			}
		}
	}
	type dep struct{ ns, name string }
	rolling := map[dep]bool{}
	var others []kube.Obj
	for _, p := range pods {
		if kube.Str(p, "spec", "nodeName") != node {
			continue
		}
		phase := kube.Str(p, "status", "phase")
		if phase == "Succeeded" || phase == "Failed" || kube.Str(p, "metadata", "namespace") == opsNamespace {
			continue
		}
		kind, owner := "", ""
		for _, o := range owners(p) {
			kind, owner = o[0], o[1]
		}
		if kind == "DaemonSet" {
			continue
		}
		if hasClaim(p) {
			others = append(others, p) // a ReadWriteOnce volume cannot be in two places
			continue
		}
		if kind == "ReplicaSet" {
			if d, ok := rsOwner[kube.Str(p, "metadata", "namespace")+"/"+owner]; ok {
				if d == "sb-guard" {
					continue // the guard itself goes down with the reboot and comes back elsewhere
				}
				var dobj kube.Obj
				if err := g.Kube.Get(ctx, "apps/v1", "Deployment", kube.Str(p, "metadata", "namespace"), d, &dobj); err == nil &&
					kube.Str(dobj, "spec", "strategy", "type") != "Recreate" {
					rolling[dep{kube.Str(p, "metadata", "namespace"), d}] = true
					continue
				}
			}
		}
		others = append(others, p)
	}
	stamp := time.Now().UTC().Format(time.RFC3339)
	keys := make([]dep, 0, len(rolling))
	for d := range rolling {
		keys = append(keys, d)
	}
	sort.Slice(keys, func(i, j int) bool { return keys[i].ns+keys[i].name < keys[j].ns+keys[j].name })
	for _, d := range keys {
		if err := g.Kube.MergePatch(ctx, "apps/v1", "Deployment", d.ns, d.name, map[string]any{"spec": map[string]any{"template": map[string]any{
			"metadata": map[string]any{"annotations": map[string]any{"kubectl.kubernetes.io/restartedAt": stamp}}}}}); err != nil {
			return 0, fmt.Errorf("restart %s/%s: %w", d.ns, d.name, err)
		}
	}
	// Wait until the restarted deployments are available elsewhere. A restart that cannot
	// finish in 3 minutes (nowhere to put the surge pod) falls back to deleting the old pod.
	deadline := time.Now().Add(3 * time.Minute)
	for _, d := range keys {
		for time.Now().Before(deadline) {
			var dobj kube.Obj
			if err := g.Kube.Get(ctx, "apps/v1", "Deployment", d.ns, d.name, &dobj); err == nil {
				want := kube.Num(dobj, "spec", "replicas")
				if kube.Num(dobj, "status", "updatedReplicas") >= want && kube.Num(dobj, "status", "availableReplicas") >= want &&
					kube.Num(dobj, "status", "replicas") == want {
					break
				}
			}
			time.Sleep(3 * time.Second)
		}
	}
	for _, p := range others {
		g.Kube.Delete(ctx, "v1", "Pod", kube.Str(p, "metadata", "namespace"), kube.Str(p, "metadata", "name"))
	}
	// Anything still on the node (a restart that could not finish) is deleted now.
	if left, err := g.Kube.List(ctx, "v1", "Pod", "", ""); err == nil {
		for _, p := range left {
			if kube.Str(p, "spec", "nodeName") != node || kube.Str(p, "metadata", "namespace") == opsNamespace || kube.Str(p, "metadata", "deletionTimestamp") != "" {
				continue
			}
			isDS, isGuard := false, false
			for _, o := range owners(p) {
				isDS = isDS || o[0] == "DaemonSet"
				isGuard = isGuard || strings.HasPrefix(o[1], "sb-guard-")
			}
			if phase := kube.Str(p, "status", "phase"); isDS || isGuard || phase == "Succeeded" || phase == "Failed" {
				continue
			}
			g.Kube.Delete(ctx, "v1", "Pod", kube.Str(p, "metadata", "namespace"), kube.Str(p, "metadata", "name"))
		}
	}
	return len(keys) + len(others), nil
}

func hasClaim(p kube.Obj) bool {
	spec, _ := p["spec"].(map[string]any)
	vols, _ := spec["volumes"].([]any)
	for _, v := range vols {
		if m, _ := v.(map[string]any); m["persistentVolumeClaim"] != nil {
			return true
		}
	}
	return false
}

// owners returns (kind, name) pairs of an object's owner references.
func owners(o kube.Obj) [][2]string {
	var out [][2]string
	md, _ := o["metadata"].(map[string]any)
	refs, _ := md["ownerReferences"].([]any)
	for _, r := range refs {
		m, _ := r.(map[string]any)
		out = append(out, [2]string{fmt.Sprint(m["kind"]), fmt.Sprint(m["name"])})
	}
	return out
}

// finishLeftovers completes maintenance a previous guard left behind (it was running on
// the server it rebooted): once the server has booted again and is Ready, it is uncordoned
// and put back into the load balancer.
func (g *Guard) finishLeftovers(ctx context.Context, info *tenant.ClusterInfo, list []kube.Obj) {
	if g.inMaint {
		return
	}
	for _, n := range list {
		if kube.Str(n, "metadata", "annotations", maintMarker) == "" {
			continue
		}
		v := nodeViews([]kube.Obj{n})[0]
		at, _ := time.Parse(time.RFC3339, kube.Str(n, "metadata", "annotations", maintMarker))
		old := kube.Str(n, "metadata", "annotations", maintBoot)
		if !v.Ready || bootID(n) == "" || bootID(n) == old {
			if time.Since(at) < rebootReadyWait {
				continue
			}
		}
		g.endMaintenance(ctx, info, v.Name)
		st := g.loadMaint(ctx)
		res := st.Nodes[v.Name]
		if v.Ready && bootID(n) != old {
			res.RebootNeeded, res.LastRebootAt = false, time.Now().UTC().Format(time.RFC3339)
			g.mlog(st, "%s: rebooted and back (finished by a new guard)", v.Name)
		} else {
			res.Error = "did not come back within 15 minutes after the reboot"
			g.mlog(st, "%s: %s", v.Name, res.Error)
		}
		st.Nodes[v.Name] = res
		st.Running, st.Phase = false, ""
		g.mlog(st, "maintenance finished")
		g.saveMaint(ctx, st)
	}
}
