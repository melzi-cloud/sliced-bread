package tenant

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// Kubectl runs "k3s kubectl <args>" on the first node with optional stdin.
func (st *State) Kubectl(ctx context.Context, args string, stdin []byte) ([]byte, error) {
	t, err := st.Target(0)
	if err != nil {
		return nil, err
	}
	var in io.Reader // stays a nil interface when there is no stdin
	if stdin != nil {
		in = bytes.NewReader(stdin)
	}
	return t.Run(ctx, "k3s kubectl "+args, in)
}

// Params returns addon template parameters for this tenant.
func (p *Provisioner) Params(st *State) AddonParams {
	ap := AddonParams{
		RunID: st.RunID, Owner: st.Owner, ExpiresAt: strconv.FormatInt(st.ExpiresAt.Unix(), 10),
		Location: st.Location, DNSZone: p.Cfg.DNSZone, WildcardDomain: "*." + st.Domain,
		ACMEServer: p.Cfg.ACMEServer, CertManagerVersion: p.Cfg.CertManagerVersion, HcloudCSIVersion: p.Cfg.HcloudCSIVersion,
	}
	if f := p.Cfg.Fallback; f != nil {
		ap.FallbackServer, ap.FallbackKeyID = f.Server, f.KeyID
	}
	return ap
}

// InstallAddons installs secrets, cert-manager, the CSI driver, and the tenant wildcard certificate.
func (p *Provisioner) InstallAddons(ctx context.Context, st *State) error {
	params := p.Params(st)
	p.Logf("[%s] installing secrets and charts", st.RunID)
	if _, err := st.Kubectl(ctx, "create namespace cert-manager --dry-run=client -o yaml | k3s kubectl apply -f - && k3s kubectl create namespace sb-system --dry-run=client -o yaml | k3s kubectl apply -f -", nil); err != nil {
		return fmt.Errorf("namespace: %w", err)
	}
	in := SecretsInput{HcloudToken: p.Cfg.HcloudToken, CloudflareToken: p.Cfg.CloudflareToken}
	if p.Cfg.Fallback != nil {
		in.FallbackHMAC = p.Cfg.Fallback.HMAC
	}
	secrets, err := SecretsManifest(in)
	if err != nil {
		return err
	}
	if _, err := st.Kubectl(ctx, "apply -f -", secrets); err != nil {
		return fmt.Errorf("apply secrets: %w", err) // error text comes from kubectl, never contains stdin
	}
	charts, err := ChartsManifest(params)
	if err != nil {
		return err
	}
	if _, err := st.Kubectl(ctx, "apply -f -", []byte(charts)); err != nil {
		return fmt.Errorf("apply charts: %w", err)
	}

	p.Logf("[%s] waiting for cert-manager, Traefik CRDs and CSI driver", st.RunID)
	err = poll(ctx, 10*time.Minute, 10*time.Second, func(ctx context.Context) (bool, error) {
		_, err := st.Kubectl(ctx, "get crd certificates.cert-manager.io tlsstores.traefik.io -o name && "+
			"k3s kubectl -n cert-manager rollout status deploy/cert-manager-webhook --timeout=15s && "+
			"k3s kubectl -n kube-system rollout status deploy/hcloud-csi-controller --timeout=15s && "+
			"k3s kubectl get storageclass hcloud-volumes -o name", nil)
		return err == nil, nil
	})
	if err != nil {
		return fmt.Errorf("addons not ready: %w", err)
	}

	tls, err := TLSManifest(params)
	if err != nil {
		return err
	}
	p.Logf("[%s] requesting wildcard certificate for %s", st.RunID, params.WildcardDomain)
	err = poll(ctx, 3*time.Minute, 10*time.Second, func(ctx context.Context) (bool, error) {
		_, err := st.Kubectl(ctx, "apply -f -", []byte(tls))
		return err == nil, nil
	})
	if err != nil {
		return fmt.Errorf("apply TLS manifests: %w", err)
	}
	certReady := func(timeout time.Duration) error {
		return poll(ctx, timeout, 10*time.Second, func(ctx context.Context) (bool, error) {
			_, err := st.Kubectl(ctx, "-n kube-system wait --for=condition=Ready certificate/tenant-wildcard --timeout=20s", nil)
			return err == nil, nil
		})
	}
	first := 12 * time.Minute
	if params.FallbackServer != "" {
		first = 8 * time.Minute
	}
	if err = certReady(first); err != nil && params.FallbackServer != "" {
		p.Logf("[%s] first issuer did not issue in %s; switching to the fallback issuer", st.RunID, first)
		if _, perr := st.Kubectl(ctx, `-n kube-system patch certificate tenant-wildcard --type merge -p '{"spec":{"issuerRef":{"name":"`+FallbackIssuerName+`"}}}'`, nil); perr != nil {
			return fmt.Errorf("switch to fallback issuer: %w", perr)
		}
		err = certReady(12 * time.Minute)
	}
	if err != nil {
		return fmt.Errorf("wildcard certificate not issued: %w", err)
	}
	p.Logf("[%s] addons ready", st.RunID)
	return nil
}

// CollectArtifacts writes cluster state and logs to dir for debugging. It never collects secrets.
func (p *Provisioner) CollectArtifacts(ctx context.Context, st *State, dir string) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		p.Logf("artifacts: %v", err)
		return
	}
	write := func(name string, b []byte, err error) {
		if err != nil {
			b = append(b, []byte("\nERROR: "+err.Error()+"\n")...)
		}
		_ = os.WriteFile(filepath.Join(dir, name), b, 0o600)
	}
	if b, err := os.ReadFile(filepath.Join(st.Dir, "state.json")); err == nil {
		write("state.json", b, nil)
	}
	kube := map[string]string{
		"nodes.txt":        "get nodes -o wide",
		"pods.txt":         "get pods -A -o wide",
		"events.txt":       "get events -A --sort-by=.lastTimestamp",
		"helmcharts.txt":   "get helmcharts -A -o wide",
		"certs.txt":        "describe clusterissuer,certificate,certificaterequest,order,challenge -A",
		"ingress.txt":      "get ingress,svc,endpoints -A -o wide",
		"pvc.txt":          "get pvc,pv -A -o wide",
		"cert-manager.log": "-n cert-manager logs deploy/cert-manager --tail=400",
		"traefik.log":      "-n kube-system logs deploy/traefik --tail=200",
		"csi.log":          "-n kube-system logs deploy/hcloud-csi-controller --all-containers --tail=200",
		"helm-jobs.log":    "-n kube-system logs -l helmcharts.helm.cattle.io/chart --all-containers --tail=200 --prefix",
	}
	for name, args := range kube {
		cctx, cancel := context.WithTimeout(ctx, 30*time.Second)
		b, err := st.Kubectl(cctx, args, nil)
		cancel()
		write(name, b, err)
	}
	for i, n := range st.Nodes {
		t, err := st.Target(i)
		if err != nil {
			continue
		}
		cctx, cancel := context.WithTimeout(ctx, 30*time.Second)
		b, err := t.Run(cctx, "cat /var/log/sb-bootstrap.log; echo '--- cloud-init'; tail -n 100 /var/log/cloud-init-output.log; echo '--- k3s'; journalctl -u k3s --no-pager -n 300", nil)
		cancel()
		write(n.Name+".log", b, err)
	}
	p.Logf("[%s] artifacts written to %s", st.RunID, dir)
}

// Redacted reports whether s contains any configured secret. Used by tests on artifacts.
func (p *Provisioner) Redacted(s string) bool {
	for _, secret := range []string{p.Cfg.HcloudToken, p.Cfg.CloudflareToken} {
		if secret != "" && strings.Contains(s, secret) {
			return false
		}
	}
	return true
}
