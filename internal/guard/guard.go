// Package guard replaces dead servers on 3-node installs. It runs as its own small
// Deployment (sb-guard) without a volume, so it reschedules within about a minute when
// its own server dies, and it does not depend on sb-control, whose volume may be stuck
// on the dead server until the guard frees it.
package guard

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net"
	"sort"
	"strings"
	"time"

	"github.com/hetznercloud/hcloud-go/v2/hcloud"

	"github.com/melzi-cloud/sliced-bread/internal/kube"
	"github.com/melzi-cloud/sliced-bread/internal/labels"
	"github.com/melzi-cloud/sliced-bread/internal/sshx"
	"github.com/melzi-cloud/sliced-bread/internal/tenant"
)

const (
	ns             = "sb-system"
	stateConfigMap = "sb-node-repair"
	tick           = 10 * time.Second
	// A server that Hetzner reports off or gone, while its node is NotReady, is dead.
	deadGrace = 30 * time.Second
	// A server Hetzner reports running but whose node stays NotReady gets one hard reset...
	resetAfter = 5 * time.Minute
	// ...and is replaced if it is still NotReady this long after the reset.
	replaceAfterReset = 8 * time.Minute
	joinTimeout       = 20 * time.Minute
	maxPerDay         = 3
	outOfServiceTaint = "node.kubernetes.io/out-of-service"
)

// Repair is one replacement in progress or done.
type Repair struct {
	Node       string `json:"node"`
	Phase      string `json:"phase"` // detected, reset, fencing, deleting, creating, joining, done, failed
	Reason     string `json:"reason"`
	PrivateIP  string `json:"privateIp"`
	DetectedAt string `json:"detectedAt"`
	ResetAt    string `json:"resetAt,omitempty"`
	FencedAt   string `json:"fencedAt,omitempty"`
	CreatedAt  string `json:"createdAt,omitempty"`
	ReadyAt    string `json:"readyAt,omitempty"`
	Message    string `json:"message,omitempty"`
}

// State is persisted in the sb-node-repair ConfigMap so a restarted guard resumes.
type State struct {
	Active  *Repair  `json:"active,omitempty"`
	History []Repair `json:"history,omitempty"`
}

// Guard watches nodes and replaces dead servers.
type Guard struct {
	Kube    *kube.Client
	Logf    func(string, ...any)
	hc      *hcloud.Client
	hcToken string
	lastMsg string
	dnsAt   time.Time
}

func ts(t time.Time) string { return t.UTC().Format(time.RFC3339) }
func parse(s string) time.Time {
	t, _ := time.Parse(time.RFC3339, s)
	return t
}

// Run loops until ctx ends.
func (g *Guard) Run(ctx context.Context) error {
	if g.Logf == nil {
		g.Logf = log.Printf
	}
	g.Logf("sb-guard: watching nodes")
	for {
		if err := g.step(ctx); err != nil {
			g.say("sb-guard: %v", err)
		}
		select {
		case <-ctx.Done():
			return nil
		case <-time.After(tick):
		}
	}
}

// say logs a message once until it changes.
func (g *Guard) say(format string, a ...any) {
	m := fmt.Sprintf(format, a...)
	if m != g.lastMsg {
		g.Logf("%s", m)
		g.lastMsg = m
	}
}

func (g *Guard) info(ctx context.Context) (*tenant.ClusterInfo, error) {
	var cm kube.Obj
	if err := g.Kube.Get(ctx, "v1", "ConfigMap", ns, tenant.ClusterInfoConfigMap, &cm); err != nil {
		return nil, err
	}
	var info tenant.ClusterInfo
	if err := json.Unmarshal([]byte(kube.Str(cm, "data", "info.json")), &info); err != nil {
		return nil, err
	}
	return &info, nil
}

func (g *Guard) secret(ctx context.Context, name, key string) (string, error) {
	var sec kube.Obj
	if err := g.Kube.Get(ctx, "v1", "Secret", ns, name, &sec); err != nil {
		return "", err
	}
	b, err := base64.StdEncoding.DecodeString(kube.Str(sec, "data", key))
	if err != nil || len(b) == 0 {
		return "", fmt.Errorf("secret %s has no %s", name, key)
	}
	return strings.TrimSpace(string(b)), nil
}

func (g *Guard) hcloud(ctx context.Context) (*hcloud.Client, error) {
	tok, err := g.secret(ctx, "sb-provider", "hcloud-token")
	if err != nil {
		return nil, err
	}
	if g.hc == nil || tok != g.hcToken {
		g.hc, g.hcToken = hcloud.NewClient(hcloud.WithToken(tok), hcloud.WithApplication("sb-guard", "1")), tok
	}
	return g.hc, nil
}

func (g *Guard) load(ctx context.Context) *State {
	st := &State{}
	var cm kube.Obj
	if err := g.Kube.Get(ctx, "v1", "ConfigMap", ns, stateConfigMap, &cm); err == nil {
		json.Unmarshal([]byte(kube.Str(cm, "data", "state.json")), st)
	}
	return st
}

func (g *Guard) save(ctx context.Context, st *State) error {
	if len(st.History) > 20 {
		st.History = st.History[len(st.History)-20:]
	}
	b, _ := json.Marshal(st)
	return g.Kube.Apply(ctx, kube.Obj{"apiVersion": "v1", "kind": "ConfigMap",
		"metadata": kube.Obj{"name": stateConfigMap, "namespace": ns}, "data": kube.Obj{"state.json": string(b)}})
}

// nodeView is what the guard needs to know about a Kubernetes node.
type nodeView struct {
	Name       string
	Ready      bool
	Since      time.Time // last Ready transition
	InternalIP string
}

func nodeViews(list []kube.Obj) []nodeView {
	var out []nodeView
	for _, n := range list {
		v := nodeView{Name: kube.Str(n, "metadata", "name")}
		status, _ := n["status"].(map[string]any)
		conds, _ := status["conditions"].([]any)
		for _, c := range conds {
			m, _ := c.(map[string]any)
			if m["type"] == "Ready" {
				v.Ready = m["status"] == "True"
				v.Since = parse(fmt.Sprint(m["lastTransitionTime"]))
			}
		}
		addrs, _ := status["addresses"].([]any)
		for _, a := range addrs {
			m, _ := a.(map[string]any)
			if m["type"] == "InternalIP" {
				v.InternalIP = fmt.Sprint(m["address"])
			}
		}
		out = append(out, v)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// Decision is what the guard does about one NotReady node.
type Decision string

const (
	Wait    Decision = "wait"
	Reset   Decision = "reset"
	Replace Decision = "replace"
)

// Decide applies the policy: Hetzner says the server is off or gone -> replace after a
// short grace; Hetzner says running -> one hard reset after resetAfter, then replace if
// still NotReady replaceAfterReset later.
func Decide(serverStatus string, notReadyFor time.Duration, resetAt time.Time, now time.Time) Decision {
	switch serverStatus {
	case "", string(hcloud.ServerStatusOff), string(hcloud.ServerStatusDeleting), string(hcloud.ServerStatusStopping), string(hcloud.ServerStatusUnknown):
		if notReadyFor >= deadGrace {
			return Replace
		}
		return Wait
	}
	if resetAt.IsZero() {
		if notReadyFor >= resetAfter {
			return Reset
		}
		return Wait
	}
	if now.Sub(resetAt) >= replaceAfterReset {
		return Replace
	}
	return Wait
}

func (g *Guard) step(ctx context.Context) error {
	info, err := g.info(ctx)
	if err != nil {
		g.say("sb-guard: no install info (%v); node repair is off", err)
		return nil
	}
	if info.Size != 3 {
		g.say("sb-guard: %d-node install; servers are not replaced automatically", info.Size)
		return nil
	}
	list, err := g.Kube.List(ctx, "v1", "Node", "", "")
	if err != nil {
		return err
	}
	nodes := nodeViews(list)
	if time.Since(g.dnsAt) > time.Minute {
		if err := g.ensureCoreDNS(ctx); err != nil {
			g.say("sb-guard: coredns: %v", err)
		}
		g.dnsAt = time.Now()
	}
	st := g.load(ctx)
	now := time.Now()
	if st.Active != nil {
		return g.continueRepair(ctx, info, st, nodes)
	}
	var bad []nodeView
	for _, n := range nodes {
		if !n.Ready {
			bad = append(bad, n)
		}
	}
	if len(bad) == 0 {
		g.say("sb-guard: %d nodes Ready", len(nodes))
		return nil
	}
	if len(bad) > 1 || len(nodes) < 3 {
		g.say("sb-guard: %d of %d nodes NotReady; replacing needs exactly one bad node in a 3-node cluster, waiting", len(bad), len(nodes))
		return nil
	}
	n := bad[0]
	hc, err := g.hcloud(ctx)
	if err != nil {
		return err
	}
	srv, _, err := hc.Server.GetByName(ctx, n.Name)
	if err != nil {
		return fmt.Errorf("hetzner: %w", err)
	}
	status := ""
	if srv != nil {
		status = string(srv.Status)
	}
	// Reset bookkeeping lives in history-free memory of the ConfigMap: a pending reset is
	// stored as an Active repair in phase "reset".
	switch Decide(status, now.Sub(n.Since), time.Time{}, now) {
	case Wait:
		g.say("sb-guard: %s NotReady since %s (server %s), watching", n.Name, ts(n.Since), orGone(status))
		return nil
	case Reset:
		if a, _, err := hc.Server.Reset(ctx, srv); err != nil {
			return fmt.Errorf("reset %s: %w", n.Name, err)
		} else if a != nil {
			hc.Action.WaitFor(ctx, a)
		}
		g.Logf("sb-guard: %s NotReady for %s while its server runs; hard reset", n.Name, now.Sub(n.Since).Round(time.Second))
		st.Active = &Repair{Node: n.Name, Phase: "reset", Reason: "node NotReady while server running", PrivateIP: n.InternalIP, DetectedAt: ts(n.Since), ResetAt: ts(now)}
		return g.save(ctx, st)
	}
	if recent(st.History, now) >= maxPerDay {
		// Still fence it, so its pods and volumes move to the healthy servers; fencing a
		// server Hetzner reports off or gone is always safe.
		if status != string(hcloud.ServerStatusRunning) {
			if err := g.taint(ctx, n.Name); err != nil {
				return err
			}
		}
		g.say("sb-guard: %s is dead, but %d servers were replaced in the last 24 hours; fenced it, not replacing more automatically", n.Name, maxPerDay)
		return nil
	}
	reason := "server " + orGone(status) + " and node NotReady"
	g.Logf("sb-guard: replacing %s: %s", n.Name, reason)
	st.Active = &Repair{Node: n.Name, Phase: "fencing", Reason: reason, PrivateIP: n.InternalIP, DetectedAt: ts(n.Since)}
	if err := g.save(ctx, st); err != nil {
		return err
	}
	return g.continueRepair(ctx, info, st, nodes)
}

func orGone(s string) string {
	if s == "" {
		return "gone"
	}
	return s
}

func recent(h []Repair, now time.Time) int {
	n := 0
	for _, r := range h {
		if now.Sub(parse(r.DetectedAt)) < 24*time.Hour && r.Phase == "done" {
			n++
		}
	}
	return n
}

// continueRepair advances the active repair by one or more phases. Every phase is
// idempotent, so a guard restarted mid-way picks up where the last one stopped.
func (g *Guard) continueRepair(ctx context.Context, info *tenant.ClusterInfo, st *State, nodes []nodeView) error {
	r := st.Active
	now := time.Now()
	hc, err := g.hcloud(ctx)
	if err != nil {
		return err
	}
	var self *nodeView
	for i := range nodes {
		if nodes[i].Name == r.Node {
			self = &nodes[i]
		}
	}
	fail := func(msg string) error {
		r.Phase, r.Message = "failed", msg
		g.Logf("sb-guard: replacing %s failed: %s", r.Node, msg)
		st.History = append(st.History, *r)
		st.Active = nil
		return g.save(ctx, st)
	}
	if r.Phase == "reset" {
		if self != nil && self.Ready {
			g.Logf("sb-guard: %s is Ready again after the reset", r.Node)
			r.Phase, r.ReadyAt = "done", ts(now)
			r.Message = "recovered after a hard reset"
			st.History = append(st.History, *r)
			st.Active = nil
			return g.save(ctx, st)
		}
		srv, _, err := hc.Server.GetByName(ctx, r.Node)
		if err != nil {
			return err
		}
		status := ""
		if srv != nil {
			status = string(srv.Status)
		}
		since := now.Sub(parse(r.DetectedAt))
		if Decide(status, since, parse(r.ResetAt), now) != Replace {
			g.say("sb-guard: %s still NotReady after reset, waiting", r.Node)
			return nil
		}
		r.Phase, r.Reason = "fencing", "still NotReady 8 minutes after a hard reset"
		g.Logf("sb-guard: replacing %s: %s", r.Node, r.Reason)
		if err := g.save(ctx, st); err != nil {
			return err
		}
	}
	if r.Phase == "fencing" {
		// The out-of-service taint makes Kubernetes delete the node's pods and detach their
		// volumes at once, instead of waiting about 6 minutes, so stateful apps restart on
		// the other servers while the replacement boots.
		if self != nil {
			if err := g.taint(ctx, r.Node); err != nil {
				return err
			}
		}
		r.Phase, r.FencedAt = "deleting", ts(now)
		g.Logf("sb-guard: %s fenced (out-of-service taint); its pods and volumes move", r.Node)
		if err := g.save(ctx, st); err != nil {
			return err
		}
	}
	if r.Phase == "deleting" {
		srv, _, err := hc.Server.GetByName(ctx, r.Node)
		if err != nil {
			return err
		}
		if srv != nil {
			if r.PrivateIP == "" {
				for _, pn := range srv.PrivateNet {
					r.PrivateIP = pn.IP.String()
				}
			}
			res, _, err := hc.Server.DeleteWithResult(ctx, srv)
			if err != nil {
				return fmt.Errorf("delete server %s: %w", r.Node, err)
			}
			if res != nil && res.Action != nil {
				if err := hc.Action.WaitFor(ctx, res.Action); err != nil {
					return fmt.Errorf("delete server %s: %w", r.Node, err)
				}
			}
		}
		// Deleting the Node removes its etcd member and its node password, so the
		// replacement can join under the same name.
		if err := g.Kube.Delete(ctx, "v1", "Node", "", r.Node); err != nil && !kube.IsNotFound(err) {
			return err
		}
		r.Phase = "creating"
		g.Logf("sb-guard: deleted server and node %s", r.Node)
		if err := g.save(ctx, st); err != nil {
			return err
		}
	}
	if r.Phase == "creating" {
		if err := g.create(ctx, hc, info, r, nodes); err != nil {
			if strings.Contains(err.Error(), "uniqueness_error") {
				// Created by an earlier attempt that did not record it.
				r.Phase = "joining"
				return g.save(ctx, st)
			}
			r.Message = err.Error()
			g.save(ctx, st)
			if now.Sub(parse(r.FencedAt)) > time.Hour {
				return fail("could not create the replacement server for an hour: " + err.Error())
			}
			return err
		}
		r.Phase, r.CreatedAt, r.Message = "joining", ts(time.Now()), ""
		if err := g.save(ctx, st); err != nil {
			return err
		}
	}
	if r.Phase == "joining" {
		if self != nil && self.Ready && self.Since.After(parse(r.FencedAt)) {
			r.Phase, r.ReadyAt = "done", ts(now)
			g.Logf("sb-guard: %s replaced and Ready (%s after it went down)", r.Node, now.Sub(parse(r.DetectedAt)).Round(time.Second))
			st.History = append(st.History, *r)
			st.Active = nil
			return g.save(ctx, st)
		}
		if now.Sub(parse(r.CreatedAt)) > joinTimeout {
			return fail("the replacement server did not join within 20 minutes")
		}
		g.say("sb-guard: waiting for replacement %s to join", r.Node)
	}
	return nil
}

// taint adds the out-of-service taint to a node, keeping its other taints.
func (g *Guard) taint(ctx context.Context, name string) error {
	var n kube.Obj
	if err := g.Kube.Get(ctx, "v1", "Node", "", name, &n); err != nil {
		if kube.IsNotFound(err) {
			return nil
		}
		return err
	}
	spec, _ := n["spec"].(map[string]any)
	taints, _ := spec["taints"].([]any)
	for _, t := range taints {
		if m, _ := t.(map[string]any); m["key"] == outOfServiceTaint {
			return nil
		}
	}
	taints = append(taints, map[string]any{"key": outOfServiceTaint, "value": "nodeshutdown", "effect": "NoExecute"})
	return g.Kube.MergePatch(ctx, "v1", "Node", "", name, map[string]any{"spec": map[string]any{"taints": taints}})
}

// create makes the replacement server: same name, labels, network, firewall, placement
// group and SSH key as its peers, the same private IP as the dead server, and user data
// that joins it to a Ready server.
func (g *Guard) create(ctx context.Context, hc *hcloud.Client, info *tenant.ClusterInfo, r *Repair, nodes []nodeView) error {
	peers, err := hc.Server.AllWithOpts(ctx, hcloud.ServerListOpts{ListOpts: hcloud.ListOpts{LabelSelector: labels.Selector(info.RunID) + "," + labels.Role + "=server"}})
	if err != nil {
		return err
	}
	var peer *hcloud.Server
	for _, p := range peers {
		if p.Name != r.Node && p.Status == hcloud.ServerStatusRunning {
			peer = p
			break
		}
	}
	if peer == nil {
		return errors.New("no running peer server to copy settings from")
	}
	join := ""
	for _, n := range nodes {
		if n.Ready && n.Name != r.Node && n.InternalIP != "" {
			join = n.InternalIP
			break
		}
	}
	if join == "" {
		return errors.New("no Ready server to join")
	}
	token, err := g.secret(ctx, tenant.JoinSecret, "token")
	if err != nil {
		return err
	}
	if len(peer.PrivateNet) == 0 || peer.PrivateNet[0].Network == nil {
		return errors.New("peer has no private network")
	}
	network := peer.PrivateNet[0].Network
	sshKey, _, err := hc.SSHKey.GetByName(ctx, "sb-"+info.RunID)
	if err != nil || sshKey == nil {
		return fmt.Errorf("ssh key sb-%s not found: %v", info.RunID, err)
	}
	hostKey, err := sshx.NewKeyPair(r.Node + "-host")
	if err != nil {
		return err
	}
	ud, err := tenant.RenderUserData(tenant.NodeConfig{
		JoinAddr: join, Token: token, K3sVersion: info.K3sVersion,
		HostKeyPriv: string(hostKey.PrivatePEM), HostKeyPub: hostKey.AuthorizedKey,
		Manifests: tenant.ServerManifests(info.Size),
	})
	if err != nil {
		return err
	}
	var fws []*hcloud.ServerCreateFirewall
	for _, f := range peer.PublicNet.Firewalls {
		fws = append(fws, &hcloud.ServerCreateFirewall{Firewall: hcloud.Firewall{ID: f.Firewall.ID}})
	}
	g.Logf("sb-guard: creating replacement %s (joins %s)", r.Node, join)
	res, _, err := hc.Server.Create(ctx, hcloud.ServerCreateOpts{
		Name: r.Node, ServerType: &hcloud.ServerType{Name: info.ServerType}, Image: &hcloud.Image{Name: info.Image},
		Location: &hcloud.Location{Name: info.Location}, SSHKeys: []*hcloud.SSHKey{sshKey}, UserData: ud,
		Labels: peer.Labels, Firewalls: fws, PlacementGroup: peer.PlacementGroup,
		PublicNet:        &hcloud.ServerCreatePublicNet{EnableIPv4: true, EnableIPv6: true},
		StartAfterCreate: hcloud.Ptr(false),
	})
	if err != nil {
		return fmt.Errorf("create server: %w", err)
	}
	if err := hc.Action.WaitFor(ctx, append([]*hcloud.Action{res.Action}, res.NextActions...)...); err != nil {
		return fmt.Errorf("create server: %w", err)
	}
	// Same private IP as before: the other servers were started with --server pointing at
	// their peers' private addresses.
	attach := hcloud.ServerAttachToNetworkOpts{Network: network}
	if ip := net.ParseIP(r.PrivateIP); ip != nil {
		attach.IP = ip
	}
	a, _, err := hc.Server.AttachToNetwork(ctx, res.Server, attach)
	if err != nil {
		return fmt.Errorf("attach network: %w", err)
	}
	if err := hc.Action.WaitFor(ctx, a); err != nil {
		return fmt.Errorf("attach network: %w", err)
	}
	a, _, err = hc.Server.Poweron(ctx, res.Server)
	if err != nil {
		return fmt.Errorf("power on: %w", err)
	}
	if err := hc.Action.WaitFor(ctx, a); err != nil {
		return fmt.Errorf("power on: %w", err)
	}
	srv, _, err := hc.Server.GetByID(ctx, res.Server.ID)
	if err != nil || srv == nil {
		return fmt.Errorf("read new server: %v", err)
	}
	return g.recordNode(ctx, tenant.NodeInfo{Name: r.Node, PublicIP: srv.PublicNet.IPv4.IP.String(), PrivateIP: r.PrivateIP,
		HostKey: hostKey.AuthorizedKey, ServerID: srv.ID})
}

// recordNode updates the node list the CLI reads (public IP and pinned host key).
func (g *Guard) recordNode(ctx context.Context, n tenant.NodeInfo) error {
	var cm kube.Obj
	var list []tenant.NodeInfo
	if err := g.Kube.Get(ctx, "v1", "ConfigMap", ns, tenant.NodesConfigMap, &cm); err == nil {
		json.Unmarshal([]byte(kube.Str(cm, "data", "nodes.json")), &list)
	}
	replaced := false
	for i := range list {
		if list[i].Name == n.Name {
			list[i], replaced = n, true
		}
	}
	if !replaced {
		list = append(list, n)
	}
	b, _ := json.Marshal(list)
	return g.Kube.Apply(ctx, kube.Obj{"apiVersion": "v1", "kind": "ConfigMap",
		"metadata": kube.Obj{"name": tenant.NodesConfigMap, "namespace": ns}, "data": kube.Obj{"nodes.json": string(b)}})
}

// LoadState reads the repair state (for sb-control's alerts and API).
func LoadState(ctx context.Context, k *kube.Client) *State {
	g := &Guard{Kube: k}
	return g.load(ctx)
}

// ensureCoreDNS runs cluster DNS on two servers and moves it off an unreachable server
// within 30 seconds. k3s ships one replica, so losing its server stalled every name
// lookup in the cluster for about 5 minutes. k3s may reset these fields when it restarts;
// the guard puts them back within a minute.
func (g *Guard) ensureCoreDNS(ctx context.Context) error {
	var d kube.Obj
	if err := g.Kube.Get(ctx, "apps/v1", "Deployment", "kube-system", "coredns", &d); err != nil {
		return err
	}
	spec, _ := d["spec"].(map[string]any)
	tpl, _ := spec["template"].(map[string]any)
	pod, _ := tpl["spec"].(map[string]any)
	tols, _ := pod["tolerations"].([]any)
	has := map[string]bool{}
	var keep []any
	for _, t := range tols {
		m, _ := t.(map[string]any)
		k, _ := m["key"].(string)
		if k == "node.kubernetes.io/not-ready" || k == "node.kubernetes.io/unreachable" {
			if fmt.Sprint(m["tolerationSeconds"]) == "30" {
				has[k] = true
				keep = append(keep, t)
			}
			continue
		}
		keep = append(keep, t)
	}
	aff, _ := pod["affinity"].(map[string]any)
	_, hasAffinity := aff["podAntiAffinity"]
	if fmt.Sprint(spec["replicas"]) == "2" && has["node.kubernetes.io/not-ready"] && has["node.kubernetes.io/unreachable"] && hasAffinity {
		return nil
	}
	for _, k := range []string{"node.kubernetes.io/not-ready", "node.kubernetes.io/unreachable"} {
		if !has[k] {
			keep = append(keep, map[string]any{"key": k, "operator": "Exists", "effect": "NoExecute", "tolerationSeconds": 30})
		}
	}
	g.Logf("sb-guard: running cluster DNS on two servers")
	return g.Kube.MergePatch(ctx, "apps/v1", "Deployment", "kube-system", "coredns", map[string]any{"spec": map[string]any{
		"replicas": 2,
		"template": map[string]any{"spec": map[string]any{
			"tolerations": keep,
			"affinity": map[string]any{"podAntiAffinity": map[string]any{"requiredDuringSchedulingIgnoredDuringExecution": []any{
				map[string]any{"topologyKey": "kubernetes.io/hostname", "labelSelector": map[string]any{"matchLabels": map[string]any{"k8s-app": "kube-dns"}}},
			}}},
		}},
	}})
}
