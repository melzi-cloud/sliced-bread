package guard

import (
	"context"

	"encoding/base64"
	"encoding/json"
	"fmt"
	"github.com/hetznercloud/hcloud-go/v2/hcloud"
	"github.com/melzi-cloud/sliced-bread/internal/labels"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/melzi-cloud/sliced-bread/internal/kube"
	"github.com/melzi-cloud/sliced-bread/internal/tenant"
)

// OS maintenance. Once a day, an hour after the backup hour, the guard checks each server:
// a privileged one-off Job makes sure Debian's unattended-upgrades installs security
// updates, and reports whether a reboot is pending. Servers that need one are rebooted one
// at a time: cordon, move stateless apps by starting their replacement elsewhere first,
// move the rest, reboot, wait for Ready, uncordon. On 1-node installs the reboot happens
// in the same nightly window and apps are down for a minute or two.

const (
	opsNamespace      = "sb-node-ops"
	maintConfigMap    = "sb-maintenance"
	opsImage          = "debian:trixie-20261104-slim"
	rebootReadyWait   = 15 * time.Minute
	settleAfterReboot = 2 * time.Minute
	maintMarker       = "slicedbread.dev/maintenance"
	maintBoot         = "slicedbread.dev/maintenance-boot"
)

// MaintState is kept in the sb-maintenance ConfigMap.
type MaintState struct {
	Requested string               `json:"requested,omitempty"` // set by the dashboard: run now
	LastRun   string               `json:"lastRun,omitempty"`
	Running   bool                 `json:"running,omitempty"`
	Nodes     map[string]NodeMaint `json:"nodes,omitempty"`
	Log       []string             `json:"log,omitempty"`
}

// NodeMaint is the last result for one server.
type NodeMaint struct {
	CheckedAt     string `json:"checkedAt"`
	RebootNeeded  bool   `json:"rebootNeeded"`
	Upgradable    int    `json:"upgradable"`
	Kernel        string `json:"kernel,omitempty"`
	LastRebootAt  string `json:"lastRebootAt,omitempty"`
	LastRebootFor string `json:"lastRebootFor,omitempty"` // how long the server was away
	Error         string `json:"error,omitempty"`
}

func (g *Guard) loadMaint(ctx context.Context) *MaintState {
	st := &MaintState{Nodes: map[string]NodeMaint{}}
	var cm kube.Obj
	if err := g.Kube.Get(ctx, "v1", "ConfigMap", ns, maintConfigMap, &cm); err == nil {
		json.Unmarshal([]byte(kube.Str(cm, "data", "state.json")), st)
	}
	if st.Nodes == nil {
		st.Nodes = map[string]NodeMaint{}
	}
	return st
}

func (g *Guard) saveMaint(ctx context.Context, st *MaintState) error {
	if len(st.Log) > 40 {
		st.Log = st.Log[len(st.Log)-40:]
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

// RequestMaintenance asks the guard to run a maintenance cycle now.
func RequestMaintenance(ctx context.Context, k *kube.Client) error {
	g := &Guard{Kube: k, Logf: func(string, ...any) {}}
	st := g.loadMaint(ctx)
	st.Requested = time.Now().UTC().Format(time.RFC3339)
	return g.saveMaint(ctx, st)
}

// maintenanceHour is an hour after the backup hour (default 03:00 UTC).
func (g *Guard) maintenanceHour(ctx context.Context) int {
	hour := 3
	var sec kube.Obj
	if err := g.Kube.Get(ctx, "v1", "Secret", ns, "sb-backup", &sec); err == nil {
		if b, err := base64.StdEncoding.DecodeString(kube.Str(sec, "data", "hour")); err == nil {
			if h, err := strconv.Atoi(strings.TrimSpace(string(b))); err == nil && h >= 0 && h < 24 {
				hour = h
			}
		}
	}
	return (hour + 1) % 24
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

// maybeMaintain runs a whole maintenance cycle when due. It blocks the guard loop while it
// runs (node repair waits; maintenance only starts when all nodes are Ready).
func (g *Guard) maybeMaintain(ctx context.Context, info *tenant.ClusterInfo, nodes []nodeView) {
	st := g.loadMaint(ctx)
	if !maintenanceDue(st, g.maintenanceHour(ctx), time.Now().UTC()) {
		return
	}
	for _, n := range nodes {
		if !n.Ready {
			g.say("sb-guard: maintenance waits: %s is NotReady", n.Name)
			return
		}
	}
	st.Running, st.LastRun = true, time.Now().UTC().Format(time.RFC3339)
	g.mlog(st, "maintenance started (%d servers)", len(nodes))
	g.saveMaint(ctx, st)
	if err := g.ensureOpsNamespace(ctx); err != nil {
		g.mlog(st, "maintenance stopped: %v", err)
		st.Running = false
		g.saveMaint(ctx, st)
		return
	}
	g.inMaint = true
	defer func() { g.inMaint = false }()
	// The guard's own server goes last: its reboot ends this process, and the guard that
	// comes up elsewhere finishes it (see finishLeftovers).
	ordered := append([]nodeView{}, nodes...)
	sort.SliceStable(ordered, func(i, j int) bool { return ordered[j].Name == os.Getenv("NODE_NAME") })
	for _, n := range ordered {
		res := g.checkNode(ctx, n.Name)
		prev := st.Nodes[n.Name]
		res.LastRebootAt, res.LastRebootFor = prev.LastRebootAt, prev.LastRebootFor
		st.Nodes[n.Name] = res
		g.saveMaint(ctx, st)
		if res.Error != "" {
			g.mlog(st, "%s: check failed: %s", n.Name, res.Error)
			continue
		}
		if !res.RebootNeeded {
			g.mlog(st, "%s: up to date, no reboot needed (%d upgradable packages left for apt)", n.Name, res.Upgradable)
			continue
		}
		g.mlog(st, "%s: reboot needed after updates", n.Name)
		took, err := g.rebootNode(ctx, info, n.Name, st)
		res = st.Nodes[n.Name]
		if err != nil {
			res.Error = err.Error()
			st.Nodes[n.Name] = res
			g.mlog(st, "%s: maintenance stopped: %v", n.Name, err)
			break // never take a second server down after a failure
		}
		res.RebootNeeded, res.LastRebootAt, res.LastRebootFor = false, time.Now().UTC().Format(time.RFC3339), took.Round(time.Second).String()
		st.Nodes[n.Name] = res
		g.mlog(st, "%s: rebooted and back after %s", n.Name, took.Round(time.Second))
		g.saveMaint(ctx, st)
	}
	st.Running = false
	g.mlog(st, "maintenance finished")
	g.saveMaint(ctx, st)
}

func (g *Guard) ensureOpsNamespace(ctx context.Context) error {
	return g.Kube.Apply(ctx, kube.Obj{"apiVersion": "v1", "kind": "Namespace", "metadata": kube.Obj{"name": opsNamespace,
		"labels": kube.Obj{"pod-security.kubernetes.io/enforce": "privileged", "app.kubernetes.io/managed-by": "slicedbread"}}})
}

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

// checkScript makes sure security updates install by themselves and reports whether the
// server needs a reboot.
const checkScript = `set -e
export DEBIAN_FRONTEND=noninteractive
if ! dpkg -s unattended-upgrades >/dev/null 2>&1; then
  apt-get update -qq && apt-get install -y -qq unattended-upgrades >/dev/null
fi
cat > /etc/apt/apt.conf.d/20auto-upgrades <<'EOF'
APT::Periodic::Update-Package-Lists "1";
APT::Periodic::Unattended-Upgrade "1";
EOF
# The guard reboots servers one at a time; apt must never reboot by itself.
echo 'Unattended-Upgrade::Automatic-Reboot "false";' > /etc/apt/apt.conf.d/52sb-no-reboot
systemctl enable --now apt-daily.timer apt-daily-upgrade.timer >/dev/null 2>&1 || true
echo "SB_UPGRADABLE $(apt list --upgradable 2>/dev/null | grep -c upgradable || true)"
echo "SB_KERNEL $(uname -r)"
if [ -f /run/reboot-required ]; then echo "SB_REBOOT yes"; else echo "SB_REBOOT no"; fi
`

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
	out, err := g.runHostJob(ctx, "check-"+shortName(node)+"-"+jobID(), node, checkScript, 10*time.Minute)
	if err != nil {
		res.Error = err.Error()
		return res
	}
	res.RebootNeeded = out["SB_REBOOT"] == "yes"
	res.Upgradable, _ = strconv.Atoi(out["SB_UPGRADABLE"])
	res.Kernel = out["SB_KERNEL"]
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
		st.Running = false
		g.mlog(st, "maintenance finished")
		g.saveMaint(ctx, st)
	}
}
