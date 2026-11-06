// Package smoke runs the L2 smoke test: a real tenant on Hetzner serving whoami over TLS.
package smoke

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"path/filepath"
	"strings"
	"text/template"
	"time"

	"github.com/melzi-cloud/sliced-bread/internal/labels"
	"github.com/melzi-cloud/sliced-bread/internal/tenant"
)

var stagingRoots = []string{
	"https://letsencrypt.org/certs/staging/letsencrypt-stg-root-x1.pem",
	"https://letsencrypt.org/certs/staging/letsencrypt-stg-root-x2.pem",
}

// Options controls a smoke run.
type Options struct {
	ArtifactsRoot string // failures write to <root>/<run-id>/
	KeepOnFailure bool   // leave resources for debugging; the reaper still removes them at expiry
}

// Result summarises a run.
type Result struct {
	RunID    string
	Host     string
	Duration time.Duration
}

// Run creates a tenant, verifies it end to end, and destroys it.
func Run(ctx context.Context, p *tenant.Provisioner, opts Options) (res Result, err error) {
	start := time.Now()
	runID := labels.NewRunID()
	res.RunID = runID
	p.Logf("[%s] smoke test starting (%s)", runID, p.Cfg)

	var st *tenant.State
	defer func() {
		res.Duration = time.Since(start)
		cctx, cancel := context.WithTimeout(context.Background(), 20*time.Minute)
		defer cancel()
		if err != nil && st != nil && len(st.Nodes) > 0 {
			p.CollectArtifacts(cctx, st, filepath.Join(opts.ArtifactsRoot, runID))
		}
		if err != nil && opts.KeepOnFailure {
			p.Logf("[%s] keeping resources for debugging; the reaper removes them after %s", runID, p.Cfg.TTL)
			return
		}
		if derr := p.Down(cctx, runID); derr != nil {
			err = errors.Join(err, fmt.Errorf("teardown: %w", derr))
			return
		}
		left, ierr := p.Inventory(cctx, labels.Selector(runID))
		if ierr != nil {
			err = errors.Join(err, ierr)
		} else if len(left) > 0 {
			err = errors.Join(err, fmt.Errorf("resources remain after teardown: %v", left))
		} else {
			p.Logf("[%s] verified: no labelled resources remain", runID)
		}
	}()

	st, err = p.Up(ctx, runID)
	if err != nil {
		return res, fmt.Errorf("up: %w", err)
	}
	if err = p.InstallAddons(ctx, st); err != nil {
		return res, err
	}
	res.Host = "whoami." + st.Domain
	if err = deployWorkloads(ctx, p, st, res.Host); err != nil {
		return res, err
	}
	if err = checkDNS(ctx, res.Host, st.LBIP); err != nil {
		return res, err
	}
	if err = checkHTTPS(ctx, res.Host, st.LBIP); err != nil {
		return res, err
	}
	if err = checkFirewall(ctx, st); err != nil {
		return res, err
	}
	if err = checkMetadataBlocked(ctx, st); err != nil {
		return res, err
	}
	if err = checkPlacement(ctx, p, st); err != nil {
		return res, err
	}
	p.Logf("[%s] smoke checks passed for https://%s", runID, res.Host)
	return res, nil
}

var workloadTmpl = template.Must(template.New("w").Parse(`apiVersion: v1
kind: Namespace
metadata:
  name: smoke
  labels:
    pod-security.kubernetes.io/enforce: restricted
---
apiVersion: apps/v1
kind: Deployment
metadata:
  name: whoami
  namespace: smoke
spec:
  replicas: 2
  selector:
    matchLabels: {app: whoami}
  template:
    metadata:
      labels: {app: whoami}
    spec:
      securityContext:
        runAsNonRoot: true
        runAsUser: 65534
        seccompProfile: {type: RuntimeDefault}
      containers:
        - name: whoami
          image: traefik/whoami:v1.12.0
          args: ["--port", "8080"]
          ports: [{containerPort: 8080}]
          readinessProbe:
            httpGet: {path: /health, port: 8080}
          securityContext:
            allowPrivilegeEscalation: false
            capabilities: {drop: [ALL]}
---
apiVersion: v1
kind: Service
metadata:
  name: whoami
  namespace: smoke
spec:
  selector: {app: whoami}
  ports: [{port: 80, targetPort: 8080}]
---
apiVersion: networking.k8s.io/v1
kind: Ingress
metadata:
  name: whoami
  namespace: smoke
  annotations:
    traefik.ingress.kubernetes.io/router.entrypoints: websecure
    traefik.ingress.kubernetes.io/router.tls: "true"
spec:
  ingressClassName: traefik
  rules:
    - host: {{.Host}}
      http:
        paths:
          - path: /
            pathType: Prefix
            backend:
              service: {name: whoami, port: {number: 80}}
---
apiVersion: v1
kind: PersistentVolumeClaim
metadata:
  name: csi-check
  namespace: smoke
spec:
  accessModes: [ReadWriteOnce]
  storageClassName: hcloud-volumes
  resources:
    requests: {storage: 10Gi}
---
apiVersion: v1
kind: Pod
metadata:
  name: csi-check
  namespace: smoke
spec:
  securityContext:
    runAsNonRoot: true
    runAsUser: 65534
    fsGroup: 65534
    seccompProfile: {type: RuntimeDefault}
  containers:
    - name: check
      image: busybox:1.38.0
      command: ["sh", "-c", "echo ok > /data/ok && sleep 3600"]
      readinessProbe:
        exec: {command: ["cat", "/data/ok"]}
      securityContext:
        allowPrivilegeEscalation: false
        capabilities: {drop: [ALL]}
      volumeMounts: [{name: data, mountPath: /data}]
  volumes:
    - name: data
      persistentVolumeClaim: {claimName: csi-check}
`))

func deployWorkloads(ctx context.Context, p *tenant.Provisioner, st *tenant.State, host string) error {
	var b strings.Builder
	if err := workloadTmpl.Execute(&b, struct{ Host string }{host}); err != nil {
		return err
	}
	p.Logf("[%s] deploying whoami and a CSI volume check", st.RunID)
	if _, err := st.Kubectl(ctx, "apply -f -", []byte(b.String())); err != nil {
		return fmt.Errorf("apply workloads: %w", err)
	}
	return pollUntil(ctx, 8*time.Minute, 10*time.Second, func(ctx context.Context) error {
		_, err := st.Kubectl(ctx, "-n smoke rollout status deploy/whoami --timeout=15s && k3s kubectl -n smoke wait --for=condition=Ready pod/csi-check --timeout=15s", nil)
		return err
	})
}

func checkDNS(ctx context.Context, host, want string) error {
	r := &net.Resolver{PreferGo: true, Dial: func(ctx context.Context, network, _ string) (net.Conn, error) {
		var d net.Dialer
		return d.DialContext(ctx, network, "1.1.1.1:53")
	}}
	return pollUntil(ctx, 5*time.Minute, 10*time.Second, func(ctx context.Context) error {
		addrs, err := r.LookupHost(ctx, host)
		if err != nil {
			return err
		}
		for _, a := range addrs {
			if a == want {
				return nil
			}
		}
		return fmt.Errorf("%s resolves to %v, want %s", host, addrs, want)
	})
}

func stagingPool(ctx context.Context) (*x509.CertPool, error) {
	pool := x509.NewCertPool()
	for _, u := range stagingRoots {
		req, _ := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			return nil, err
		}
		b, err := io.ReadAll(io.LimitReader(resp.Body, 1<<16))
		resp.Body.Close()
		if err != nil {
			return nil, err
		}
		if !pool.AppendCertsFromPEM(b) {
			return nil, fmt.Errorf("no certificate in %s", u)
		}
	}
	return pool, nil
}

// checkHTTPS connects to the load balancer with the right SNI and verifies the
// certificate chains to Let's Encrypt staging roots and matches the host.
func checkHTTPS(ctx context.Context, host, lbIP string) error {
	pool, err := stagingPool(ctx)
	if err != nil {
		return fmt.Errorf("load staging roots: %w", err)
	}
	client := &http.Client{
		Timeout: 15 * time.Second,
		Transport: &http.Transport{
			TLSClientConfig: &tls.Config{RootCAs: pool, ServerName: host, MinVersion: tls.VersionTLS12},
			DialContext: func(ctx context.Context, network, _ string) (net.Conn, error) {
				var d net.Dialer
				return d.DialContext(ctx, network, net.JoinHostPort(lbIP, "443"))
			},
		},
	}
	return pollUntil(ctx, 6*time.Minute, 10*time.Second, func(ctx context.Context) error {
		req, _ := http.NewRequestWithContext(ctx, http.MethodGet, "https://"+host+"/", nil)
		resp, err := client.Do(req)
		if err != nil {
			return err
		}
		defer resp.Body.Close()
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<16))
		if resp.StatusCode != http.StatusOK || !strings.Contains(string(body), "Hostname:") {
			return fmt.Errorf("HTTP %d, body %.200q", resp.StatusCode, body)
		}
		return nil
	})
}

// checkFirewall confirms internal ports on the nodes are not publicly reachable.
// SSH (22) is open only to the operator's IP, so it is expected to connect from here.
func checkFirewall(ctx context.Context, st *tenant.State) error {
	for _, n := range st.Nodes {
		for _, port := range []string{"6443", "10250", "30080", "30443"} {
			d := net.Dialer{Timeout: 4 * time.Second}
			conn, err := d.DialContext(ctx, "tcp", net.JoinHostPort(n.PublicIP, port))
			if err == nil {
				conn.Close()
				return fmt.Errorf("port %s on %s (%s) is publicly reachable", port, n.Name, n.PublicIP)
			}
		}
	}
	return nil
}

func pollUntil(ctx context.Context, timeout, every time.Duration, f func(context.Context) error) error {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	var last error
	for {
		cctx, ccancel := context.WithTimeout(ctx, 45*time.Second)
		last = f(cctx)
		ccancel()
		if last == nil {
			return nil
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("timed out after %s: %w", timeout, last)
		case <-time.After(every):
		}
	}
}

// checkMetadataBlocked confirms pods cannot read Hetzner user data (join token, host keys).
func checkMetadataBlocked(ctx context.Context, st *tenant.State) error {
	out, err := st.Kubectl(ctx, `-n smoke run metadata-probe --rm -i --restart=Never --image=busybox:1.38.0 `+
		`--overrides='{"spec":{"securityContext":{"runAsNonRoot":true,"runAsUser":65534,"seccompProfile":{"type":"RuntimeDefault"}},"containers":[{"name":"metadata-probe","image":"busybox:1.38.0","command":["sh","-c","wget -T 3 -qO- http://169.254.169.254/hetzner/v1/userdata >/dev/null 2>&1 && echo REACHABLE || echo BLOCKED"],"securityContext":{"allowPrivilegeEscalation":false,"capabilities":{"drop":["ALL"]}}}]}}'`, nil)
	if err != nil {
		return fmt.Errorf("metadata probe: %w", err)
	}
	if !strings.Contains(string(out), "BLOCKED") {
		return fmt.Errorf("pods can read the Hetzner metadata service: %q", strings.TrimSpace(string(out)))
	}
	return nil
}

// checkPlacement confirms every server is in the run's spread placement group.
func checkPlacement(ctx context.Context, p *tenant.Provisioner, st *tenant.State) error {
	groups := map[int64]bool{}
	for _, n := range st.Nodes {
		srv, _, err := p.HC.Server.GetByID(ctx, n.ID)
		if err != nil || srv == nil {
			return fmt.Errorf("get %s: %v", n.Name, err)
		}
		if srv.PlacementGroup == nil || srv.PlacementGroup.Type != "spread" {
			return fmt.Errorf("%s is not in a spread placement group", n.Name)
		}
		groups[srv.PlacementGroup.ID] = true
	}
	if len(groups) != 1 {
		return fmt.Errorf("servers are in %d placement groups, want 1", len(groups))
	}
	return nil
}
