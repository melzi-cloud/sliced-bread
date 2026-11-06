// Package tenant creates and destroys development tenants on Hetzner Cloud.
package tenant

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/hetznercloud/hcloud-go/v2/hcloud"

	"github.com/melzi-cloud/sliced-bread/internal/config"
	"github.com/melzi-cloud/sliced-bread/internal/dns"
	"github.com/melzi-cloud/sliced-bread/internal/labels"
	"github.com/melzi-cloud/sliced-bread/internal/sshx"
)

// Provisioner manages development tenants.
type Provisioner struct {
	Cfg       config.Dev
	HC        *hcloud.Client
	DNS       *dns.Client
	StateRoot string
	Logf      func(format string, args ...any)
}

// New builds a Provisioner from config.
func New(cfg config.Dev, stateRoot string) *Provisioner {
	return &Provisioner{
		Cfg:       cfg,
		HC:        hcloud.NewClient(hcloud.WithToken(cfg.HcloudToken), hcloud.WithApplication("slicedbread-dev", "0")),
		DNS:       dns.New(cfg.CloudflareToken),
		StateRoot: stateRoot,
		Logf:      func(f string, a ...any) { fmt.Fprintf(os.Stderr, f+"\n", a...) },
	}
}

const nodeCount = 3

func networkZone(location string) (hcloud.NetworkZone, error) {
	switch location {
	case "fsn1", "nbg1", "hel1":
		return hcloud.NetworkZoneEUCentral, nil
	case "ash":
		return hcloud.NetworkZoneUSEast, nil
	case "hil":
		return hcloud.NetworkZoneUSWest, nil
	case "sin":
		return hcloud.NetworkZoneAPSouthEast, nil
	}
	return "", fmt.Errorf("unknown location %q", location)
}

// OperatorIP returns this machine's public IPv4, used to allow SSH from here only.
func OperatorIP(ctx context.Context) (net.IP, error) {
	var last error
	for _, u := range []string{"https://api4.ipify.org", "https://ipv4.icanhazip.com"} {
		req, _ := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			last = err
			continue
		}
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 64))
		resp.Body.Close()
		ip := net.ParseIP(strings.TrimSpace(string(b))).To4()
		if ip != nil {
			return ip, nil
		}
		last = fmt.Errorf("%s returned %q", u, b)
	}
	return nil, fmt.Errorf("detect operator IP: %w", last)
}

func randomToken() string {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	return hex.EncodeToString(b)
}

// Up creates the cloud resources for a new tenant and waits for k3s on all nodes.
// On error the caller should call Down(runID); resources are labelled and expire regardless.
func (p *Provisioner) Up(ctx context.Context, runID string) (*State, error) {
	if !labels.ValidRunID(runID) {
		return nil, fmt.Errorf("invalid run id %q", runID)
	}
	c := p.Cfg
	expires := time.Now().Add(c.TTL).UTC().Truncate(time.Second)
	base := labels.For(runID, c.Owner, expires)
	st := &State{
		RunID: runID, Owner: c.Owner, ExpiresAt: expires, Location: c.Location,
		Domain: runID + "." + c.TestDomain, Dir: runDir(p.StateRoot, runID),
	}
	name := "sb-" + runID

	zone, err := networkZone(c.Location)
	if err != nil {
		return nil, err
	}
	opIP, err := OperatorIP(ctx)
	if err != nil {
		return nil, err
	}

	// Per-run SSH key, stored only in the gitignored run directory.
	kp, err := sshx.NewKeyPair(name)
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(st.Dir, 0o700); err != nil {
		return nil, err
	}
	if err := os.WriteFile(filepath.Join(st.Dir, "id_ed25519"), kp.PrivatePEM, 0o600); err != nil {
		return nil, err
	}
	if err := st.Save(); err != nil {
		return nil, err
	}

	p.Logf("[%s] creating ssh key, network, firewall (ssh allowed from %s only)", runID, opIP)
	sshKey, _, err := p.HC.SSHKey.Create(ctx, hcloud.SSHKeyCreateOpts{Name: name, PublicKey: kp.AuthorizedKey, Labels: base})
	if err != nil {
		return st, fmt.Errorf("create ssh key: %w", err)
	}
	_, ipRange, _ := net.ParseCIDR("10.0.0.0/16")
	_, subnet, _ := net.ParseCIDR("10.0.1.0/24")
	network, _, err := p.HC.Network.Create(ctx, hcloud.NetworkCreateOpts{
		Name: name, IPRange: ipRange, Labels: base,
		Subnets: []hcloud.NetworkSubnet{{Type: hcloud.NetworkSubnetTypeCloud, IPRange: subnet, NetworkZone: zone}},
	})
	if err != nil {
		return st, fmt.Errorf("create network: %w", err)
	}
	// No public inbound rules except SSH from the operator. HTTP(S) arrives through
	// the load balancer over the private network, which Hetzner firewalls do not filter.
	fwRes, _, err := p.HC.Firewall.Create(ctx, hcloud.FirewallCreateOpts{
		Name: name, Labels: base,
		Rules: []hcloud.FirewallRule{{
			Direction: hcloud.FirewallRuleDirectionIn, Protocol: hcloud.FirewallRuleProtocolTCP,
			Port: hcloud.Ptr("22"), SourceIPs: []net.IPNet{{IP: opIP, Mask: net.CIDRMask(32, 32)}},
			Description: hcloud.Ptr("ssh from operator"),
		}},
	})
	if err != nil {
		return st, fmt.Errorf("create firewall: %w", err)
	}
	if err := p.HC.Action.WaitFor(ctx, fwRes.Actions...); err != nil {
		return st, fmt.Errorf("firewall actions: %w", err)
	}

	token := randomToken()
	createNode := func(i int, joinAddr string) (Node, error) {
		hostKey, err := sshx.NewKeyPair(fmt.Sprintf("%s-s%d-host", name, i))
		if err != nil {
			return Node{}, err
		}
		ud, err := RenderUserData(NodeConfig{
			Init: i == 1, JoinAddr: joinAddr, Token: token, K3sVersion: c.K3sVersion,
			HostKeyPriv: string(hostKey.PrivatePEM), HostKeyPub: hostKey.AuthorizedKey,
			Manifests: ServerManifests(),
		})
		if err != nil {
			return Node{}, err
		}
		nodeName := fmt.Sprintf("%s-s%d", name, i)
		res, _, err := p.HC.Server.Create(ctx, hcloud.ServerCreateOpts{
			Name:       nodeName,
			ServerType: &hcloud.ServerType{Name: c.ServerType},
			Image:      &hcloud.Image{Name: c.Image},
			Location:   &hcloud.Location{Name: c.Location},
			SSHKeys:    []*hcloud.SSHKey{sshKey},
			UserData:   ud,
			Labels:     labels.With(base, labels.Role, "server"),
			Networks:   []*hcloud.Network{network},
			Firewalls:  []*hcloud.ServerCreateFirewall{{Firewall: *fwRes.Firewall}},
			PublicNet:  &hcloud.ServerCreatePublicNet{EnableIPv4: true, EnableIPv6: true},
		})
		if err != nil {
			return Node{}, fmt.Errorf("create %s: %w", nodeName, err)
		}
		actions := append([]*hcloud.Action{res.Action}, res.NextActions...)
		if err := p.HC.Action.WaitFor(ctx, actions...); err != nil {
			return Node{}, fmt.Errorf("wait %s: %w", nodeName, err)
		}
		srv, _, err := p.HC.Server.GetByID(ctx, res.Server.ID)
		if err != nil || srv == nil {
			return Node{}, fmt.Errorf("get %s: %v", nodeName, err)
		}
		n := Node{Name: nodeName, ID: srv.ID, PublicIP: srv.PublicNet.IPv4.IP.String(), HostKey: hostKey.AuthorizedKey}
		for _, pn := range srv.PrivateNet {
			if pn.Network != nil && pn.Network.ID == network.ID {
				n.PrivateIP = pn.IP.String()
			}
		}
		if n.PrivateIP == "" {
			return Node{}, fmt.Errorf("%s has no private IP", nodeName)
		}
		return n, nil
	}

	p.Logf("[%s] creating server 1/%d", runID, nodeCount)
	first, err := createNode(1, "")
	if err != nil {
		return st, err
	}
	st.Nodes = append(st.Nodes, first)
	_ = st.Save()

	p.Logf("[%s] creating servers 2-%d (join %s)", runID, nodeCount, first.PrivateIP)
	var wg sync.WaitGroup
	results := make([]Node, nodeCount-1)
	errs := make([]error, nodeCount-1)
	for i := 2; i <= nodeCount; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			results[i-2], errs[i-2] = createNode(i, first.PrivateIP)
		}(i)
	}
	wg.Wait()
	for i, e := range errs {
		if e != nil {
			return st, e
		}
		st.Nodes = append(st.Nodes, results[i])
	}
	_ = st.Save()

	p.Logf("[%s] creating load balancer", runID)
	svc := func(listen, dest int) hcloud.LoadBalancerCreateOptsService {
		return hcloud.LoadBalancerCreateOptsService{
			Protocol: hcloud.LoadBalancerServiceProtocolTCP, ListenPort: hcloud.Ptr(listen), DestinationPort: hcloud.Ptr(dest),
			HealthCheck: &hcloud.LoadBalancerCreateOptsServiceHealthCheck{
				Protocol: hcloud.LoadBalancerServiceProtocolTCP, Port: hcloud.Ptr(dest),
				Interval: hcloud.Ptr(10 * time.Second), Timeout: hcloud.Ptr(5 * time.Second), Retries: hcloud.Ptr(3),
			},
		}
	}
	lbRes, _, err := p.HC.LoadBalancer.Create(ctx, hcloud.LoadBalancerCreateOpts{
		Name: name, LoadBalancerType: &hcloud.LoadBalancerType{Name: c.LBType},
		Location: &hcloud.Location{Name: c.Location}, Labels: base, Network: network,
		PublicInterface: hcloud.Ptr(true),
		Targets: []hcloud.LoadBalancerCreateOptsTarget{{
			Type:          hcloud.LoadBalancerTargetTypeLabelSelector,
			LabelSelector: hcloud.LoadBalancerCreateOptsTargetLabelSelector{Selector: labels.Selector(runID) + "," + labels.Role + "=server"},
			UsePrivateIP:  hcloud.Ptr(true),
		}},
		Services: []hcloud.LoadBalancerCreateOptsService{svc(80, NodePortHTTP), svc(443, NodePortHTTPS)},
	})
	if err != nil {
		return st, fmt.Errorf("create load balancer: %w", err)
	}
	if err := p.HC.Action.WaitFor(ctx, lbRes.Action); err != nil {
		return st, fmt.Errorf("wait load balancer: %w", err)
	}
	lb, _, err := p.HC.LoadBalancer.GetByID(ctx, lbRes.LoadBalancer.ID)
	if err != nil || lb == nil {
		return st, fmt.Errorf("get load balancer: %v", err)
	}
	st.LBIP = lb.PublicNet.IPv4.IP.String()
	_ = st.Save()

	p.Logf("[%s] creating DNS *.%s -> %s", runID, st.Domain, st.LBIP)
	zoneID, err := p.DNS.ZoneID(ctx, c.DNSZone)
	if err != nil {
		return st, err
	}
	rec, err := p.DNS.Create(ctx, zoneID, dns.Record{
		Type: "A", Name: "*." + st.Domain, Content: st.LBIP, TTL: 60,
		Comment: labels.DNSComment(runID, expires),
	})
	if err != nil {
		return st, fmt.Errorf("create DNS record: %w", err)
	}
	st.DNSRecords = append(st.DNSRecords, rec.ID)
	if err := st.Save(); err != nil {
		return st, err
	}

	if err := p.waitK3s(ctx, st); err != nil {
		return st, err
	}
	return st, nil
}

// waitK3s waits for SSH, cloud-init, and all nodes Ready.
func (p *Provisioner) waitK3s(ctx context.Context, st *State) error {
	for i := range st.Nodes {
		t, err := st.Target(i)
		if err != nil {
			return err
		}
		p.Logf("[%s] waiting for ssh and cloud-init on %s", st.RunID, st.Nodes[i].Name)
		wctx, cancel := context.WithTimeout(ctx, 6*time.Minute)
		err = t.WaitReady(wctx)
		cancel()
		if err != nil {
			return err
		}
		// cloud-init status --wait exits non-zero (2 = recoverable warnings) on some images; check the bootstrap log instead.
		cctx, cancel := context.WithTimeout(ctx, 10*time.Minute)
		_, err = t.Run(cctx, `cloud-init status --wait >/dev/null 2>&1 || true; grep -q '^sb-bootstrap done' /var/log/sb-bootstrap.log`, nil)
		cancel()
		if err != nil {
			return fmt.Errorf("bootstrap on %s did not finish: %w", st.Nodes[i].Name, err)
		}
	}
	t, err := st.Target(0)
	if err != nil {
		return err
	}
	p.Logf("[%s] waiting for %d Ready nodes", st.RunID, len(st.Nodes))
	return poll(ctx, 8*time.Minute, 5*time.Second, func(ctx context.Context) (bool, error) {
		out, err := t.Run(ctx, `k3s kubectl get nodes --no-headers 2>/dev/null | awk '$2=="Ready"' | wc -l`, nil)
		if err != nil {
			return false, nil
		}
		n, _ := strconv.Atoi(strings.TrimSpace(string(out)))
		return n == len(st.Nodes), nil
	})
}

// poll calls f until it returns true, an error, or the timeout passes.
func poll(ctx context.Context, timeout, every time.Duration, f func(context.Context) (bool, error)) error {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	for {
		cctx, ccancel := context.WithTimeout(ctx, 30*time.Second)
		ok, err := f(cctx)
		ccancel()
		if err != nil {
			return err
		}
		if ok {
			return nil
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("timed out after %s", timeout)
		case <-time.After(every):
		}
	}
}

// Resource is a labelled cloud resource found during inventory.
type Resource struct {
	Kind   string
	ID     string
	Name   string
	Labels map[string]string
}

func (r Resource) String() string { return fmt.Sprintf("%s %s (%s)", r.Kind, r.Name, r.ID) }

// Inventory lists every resource matching a label selector, plus DNS records
// under the test domain. selector "" lists everything labelled with a run id.
func (p *Provisioner) Inventory(ctx context.Context, selector string) ([]Resource, error) {
	if selector == "" {
		selector = labels.RunID
	}
	lo := hcloud.ListOpts{LabelSelector: selector, PerPage: 50}
	var out []Resource
	add := func(kind string, id int64, name string, l map[string]string) {
		out = append(out, Resource{Kind: kind, ID: strconv.FormatInt(id, 10), Name: name, Labels: l})
	}
	lbs, err := p.HC.LoadBalancer.AllWithOpts(ctx, hcloud.LoadBalancerListOpts{ListOpts: lo})
	if err != nil {
		return nil, err
	}
	for _, x := range lbs {
		add("load_balancer", x.ID, x.Name, x.Labels)
	}
	srvs, err := p.HC.Server.AllWithOpts(ctx, hcloud.ServerListOpts{ListOpts: lo})
	if err != nil {
		return nil, err
	}
	for _, x := range srvs {
		add("server", x.ID, x.Name, x.Labels)
	}
	vols, err := p.HC.Volume.AllWithOpts(ctx, hcloud.VolumeListOpts{ListOpts: lo})
	if err != nil {
		return nil, err
	}
	for _, x := range vols {
		add("volume", x.ID, x.Name, x.Labels)
	}
	fws, err := p.HC.Firewall.AllWithOpts(ctx, hcloud.FirewallListOpts{ListOpts: lo})
	if err != nil {
		return nil, err
	}
	for _, x := range fws {
		add("firewall", x.ID, x.Name, x.Labels)
	}
	nets, err := p.HC.Network.AllWithOpts(ctx, hcloud.NetworkListOpts{ListOpts: lo})
	if err != nil {
		return nil, err
	}
	for _, x := range nets {
		add("network", x.ID, x.Name, x.Labels)
	}
	keys, err := p.HC.SSHKey.AllWithOpts(ctx, hcloud.SSHKeyListOpts{ListOpts: lo})
	if err != nil {
		return nil, err
	}
	for _, x := range keys {
		add("ssh_key", x.ID, x.Name, x.Labels)
	}
	ips, err := p.HC.PrimaryIP.AllWithOpts(ctx, hcloud.PrimaryIPListOpts{ListOpts: lo})
	if err != nil {
		return nil, err
	}
	for _, x := range ips {
		add("primary_ip", x.ID, x.Name, x.Labels)
	}

	zoneID, err := p.DNS.ZoneID(ctx, p.Cfg.DNSZone)
	if err != nil {
		return nil, err
	}
	recs, err := p.DNS.ListUnder(ctx, zoneID, p.Cfg.TestDomain)
	if err != nil {
		return nil, err
	}
	wantRun := ""
	if k, v, ok := strings.Cut(selector, "="); ok && k == labels.RunID {
		wantRun = v
	}
	for _, r := range recs {
		l := labels.ParseDNSComment(r.Comment)
		switch {
		case l[labels.RunID] != "":
			if wantRun != "" && l[labels.RunID] != wantRun {
				continue
			}
		case strings.HasPrefix(r.Name, "_acme-challenge."):
			// Left behind if a run is destroyed mid-challenge. Attribute by name.
			if wantRun != "" && !strings.Contains(r.Name, "."+wantRun+".") {
				continue
			}
			l = map[string]string{labels.RunID: runFromName(r.Name, p.Cfg.TestDomain)}
			if time.Since(r.CreatedOn) > 2*time.Hour {
				l[labels.ExpiresAt] = "0"
			} else {
				l[labels.ExpiresAt] = strconv.FormatInt(r.CreatedOn.Add(2*time.Hour).Unix(), 10)
			}
		default:
			continue // not ours
		}
		out = append(out, Resource{Kind: "dns_record", ID: r.ID, Name: r.Name, Labels: l})
	}
	return out, nil
}

func runFromName(name, testDomain string) string {
	rest := strings.TrimSuffix(name, "."+testDomain)
	parts := strings.Split(rest, ".")
	for i := len(parts) - 1; i >= 0; i-- {
		if labels.ValidRunID(parts[i]) {
			return parts[i]
		}
	}
	return ""
}

// deleteOrder is the order resources must be removed in.
var deleteOrder = map[string]int{"load_balancer": 0, "server": 1, "volume": 2, "primary_ip": 2, "dns_record": 2, "firewall": 3, "network": 4, "ssh_key": 5}

// Down deletes every resource belonging to runID and verifies nothing remains.
func (p *Provisioner) Down(ctx context.Context, runID string) error {
	if !labels.ValidRunID(runID) {
		return fmt.Errorf("invalid run id %q", runID)
	}
	ctx, cancel := context.WithTimeout(ctx, 15*time.Minute)
	defer cancel()
	for attempt := 1; ; attempt++ {
		res, err := p.Inventory(ctx, labels.Selector(runID))
		if err != nil {
			return err
		}
		if len(res) == 0 {
			if attempt > 1 {
				p.Logf("[%s] all resources deleted", runID)
			}
			return nil
		}
		p.Logf("[%s] deleting %d resources (pass %d)", runID, len(res), attempt)
		if err := p.deleteAll(ctx, res); err != nil {
			p.Logf("[%s] delete pass %d: %v", runID, attempt, err)
		}
		select {
		case <-ctx.Done():
			left, _ := p.Inventory(context.Background(), labels.Selector(runID))
			return fmt.Errorf("teardown of %s timed out, remaining: %v", runID, left)
		case <-time.After(5 * time.Second):
		}
	}
}

func (p *Provisioner) deleteAll(ctx context.Context, res []Resource) error {
	sort.SliceStable(res, func(i, j int) bool { return deleteOrder[res[i].Kind] < deleteOrder[res[j].Kind] })
	var errs []error
	var zoneID string
	for _, r := range res {
		id, _ := strconv.ParseInt(r.ID, 10, 64)
		var err error
		switch r.Kind {
		case "load_balancer":
			_, err = p.HC.LoadBalancer.Delete(ctx, &hcloud.LoadBalancer{ID: id})
		case "server":
			var dr *hcloud.ServerDeleteResult
			dr, _, err = p.HC.Server.DeleteWithResult(ctx, &hcloud.Server{ID: id})
			if err == nil && dr != nil && dr.Action != nil {
				err = p.HC.Action.WaitFor(ctx, dr.Action)
			}
		case "volume":
			_, err = p.HC.Volume.Delete(ctx, &hcloud.Volume{ID: id})
		case "primary_ip":
			_, err = p.HC.PrimaryIP.Delete(ctx, &hcloud.PrimaryIP{ID: id})
		case "firewall":
			_, err = p.HC.Firewall.Delete(ctx, &hcloud.Firewall{ID: id})
		case "network":
			_, err = p.HC.Network.Delete(ctx, &hcloud.Network{ID: id})
		case "ssh_key":
			_, err = p.HC.SSHKey.Delete(ctx, &hcloud.SSHKey{ID: id})
		case "dns_record":
			if zoneID == "" {
				if zoneID, err = p.DNS.ZoneID(ctx, p.Cfg.DNSZone); err != nil {
					break
				}
			}
			err = p.DNS.Delete(ctx, zoneID, r.ID)
		}
		if err != nil && !hcloud.IsError(err, hcloud.ErrorCodeNotFound) {
			errs = append(errs, fmt.Errorf("%s: %w", r, err))
		}
	}
	return errors.Join(errs...)
}

// Reap deletes all runs that have at least one expired resource.
func (p *Provisioner) Reap(ctx context.Context, now time.Time) ([]string, error) {
	res, err := p.Inventory(ctx, "")
	if err != nil {
		return nil, err
	}
	expired := map[string]bool{}
	for _, r := range res {
		if labels.Expired(r.Labels, now) && labels.ValidRunID(r.Labels[labels.RunID]) {
			expired[r.Labels[labels.RunID]] = true
		}
	}
	var runs []string
	for id := range expired {
		runs = append(runs, id)
	}
	sort.Strings(runs)
	var errs []error
	for _, id := range runs {
		p.Logf("[reaper] run %s expired, deleting", id)
		if err := p.Down(ctx, id); err != nil {
			errs = append(errs, err)
		}
	}
	return runs, errors.Join(errs...)
}
