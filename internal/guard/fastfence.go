package guard

import (
	"context"
	"time"

	"github.com/hetznercloud/hcloud-go/v2/hcloud"

	"github.com/melzi-cloud/sliced-bread/internal/kube"
	"github.com/melzi-cloud/sliced-bread/internal/labels"
	"github.com/melzi-cloud/sliced-bread/internal/tenant"
)

// Kubernetes notices a dead server only after about 45 seconds without status updates.
// Until then the server's pods stay in the endpoints and about half of all requests (load
// balancer to the dead server, Traefik and kube-proxy to its pods) hang. Hetzner knows at
// once that a server is off, so the guard checks every tick and fences an off server
// immediately: out of the load balancer, and the out-of-service taint, which marks its
// pods as terminating so they leave the endpoints within a second.

// LB health checks: notice a server that stops answering within about 6 seconds.
const (
	lbCheckInterval = 3 * time.Second
	lbCheckTimeout  = 2 * time.Second
	lbCheckRetries  = 2
	lbDownRole      = "server-down"
)

func serverDown(status hcloud.ServerStatus) bool {
	switch status {
	case hcloud.ServerStatusOff, hcloud.ServerStatusStopping, hcloud.ServerStatusDeleting, hcloud.ServerStatusUnknown:
		return true
	}
	return false
}

// serverStatuses lists this install's servers by name. A node without an entry has no
// server any more.
func (g *Guard) serverStatuses(ctx context.Context, info *tenant.ClusterInfo) (map[string]*hcloud.Server, error) {
	hc, err := g.hcloud(ctx)
	if err != nil {
		return nil, err
	}
	list, err := hc.Server.AllWithOpts(ctx, hcloud.ServerListOpts{ListOpts: hcloud.ListOpts{LabelSelector: labels.Selector(info.RunID)}})
	if err != nil {
		return nil, err
	}
	out := map[string]*hcloud.Server{}
	for _, s := range list {
		out[s.Name] = s
	}
	return out, nil
}

// fastFence fences nodes whose server Hetzner reports off or gone, and returns when each
// was first seen down (the replacement grace counts from then, not from NotReady).
func (g *Guard) fastFence(ctx context.Context, info *tenant.ClusterInfo, list []kube.Obj, active *Repair) map[string]time.Time {
	servers, err := g.serverStatuses(ctx, info)
	if err != nil {
		g.say("sb-guard: hetzner: %v", err)
		return g.downSince
	}
	if g.downSince == nil {
		g.downSince = map[string]time.Time{}
	}
	now := time.Now()
	for _, n := range list {
		name := kube.Str(n, "metadata", "name")
		if active != nil && active.Node == name {
			continue // being replaced; the new server starts powered off on purpose
		}
		srv := servers[name]
		if srv != nil && !serverDown(srv.Status) {
			delete(g.downSince, name)
			continue
		}
		if _, seen := g.downSince[name]; !seen {
			g.downSince[name] = now
			status := "gone"
			if srv != nil {
				status = string(srv.Status)
			}
			g.Logf("sb-guard: server %s is %s; fencing it now", name, status)
		}
		if srv != nil && srv.Labels[labels.Role] == "server" {
			if _, err := g.setRole(ctx, name, lbDownRole); err != nil {
				g.Logf("sb-guard: take %s out of the load balancer: %v", name, err)
			}
		}
		if err := g.taint(ctx, name); err != nil {
			g.Logf("sb-guard: fence %s: %v", name, err)
		}
	}
	for name := range g.downSince {
		found := false
		for _, n := range list {
			found = found || kube.Str(n, "metadata", "name") == name
		}
		if !found {
			delete(g.downSince, name)
		}
	}
	return g.downSince
}

// ensureLBHealthChecks tightens the load balancer's health checks (installs created before
// this setting used 10 s x 3).
func (g *Guard) ensureLBHealthChecks(ctx context.Context, info *tenant.ClusterInfo) error {
	hc, err := g.hcloud(ctx)
	if err != nil {
		return err
	}
	lbs, err := hc.LoadBalancer.AllWithOpts(ctx, hcloud.LoadBalancerListOpts{ListOpts: hcloud.ListOpts{LabelSelector: labels.Selector(info.RunID)}})
	if err != nil || len(lbs) == 0 {
		return err
	}
	for _, svc := range lbs[0].Services {
		h := svc.HealthCheck
		if h.Interval == lbCheckInterval && h.Timeout == lbCheckTimeout && h.Retries == lbCheckRetries {
			continue
		}
		a, _, err := hc.LoadBalancer.UpdateService(ctx, lbs[0], svc.ListenPort, hcloud.LoadBalancerUpdateServiceOpts{
			HealthCheck: &hcloud.LoadBalancerUpdateServiceOptsHealthCheck{
				Interval: hcloud.Ptr(lbCheckInterval), Timeout: hcloud.Ptr(lbCheckTimeout), Retries: hcloud.Ptr(lbCheckRetries),
			},
		})
		if err != nil {
			return err
		}
		hc.Action.WaitFor(ctx, a)
		g.Logf("sb-guard: load balancer port %d now checks servers every %s", svc.ListenPort, lbCheckInterval)
	}
	return nil
}
