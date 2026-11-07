package tenant

import (
	"bytes"
	"text/template"
)

const (
	NodePortHTTP  = 30080
	NodePortHTTPS = 30443

	WildcardSecret = "tenant-wildcard-tls"
	IssuerName     = "letsencrypt"
)

// traefikConfig is dropped into the k3s manifests directory on every server.
const traefikConfig = `apiVersion: helm.cattle.io/v1
kind: HelmChartConfig
metadata:
  name: traefik
  namespace: kube-system
spec:
  valuesContent: |-
    deployment:
      replicas: 2
      # Stay in the endpoints a moment after being told to stop, so a drained or
      # restarted Traefik never refuses a request (patch day, rolling restarts).
      lifecycle:
        preStop:
          exec: {command: ["sh", "-c", "sleep 10"]}
    affinity:
      podAntiAffinity:
        requiredDuringSchedulingIgnoredDuringExecution:
          - topologyKey: kubernetes.io/hostname
            labelSelector: {matchLabels: {app.kubernetes.io/name: traefik}}
    service:
      type: NodePort
    ports:
      web:
        nodePort: 30080
      websecure:
        nodePort: 30443
`

// traefikSingleNode keeps the default LoadBalancer service, which k3s servicelb binds to 80/443 on the node.
const traefikSingleNode = `apiVersion: helm.cattle.io/v1
kind: HelmChartConfig
metadata:
  name: traefik
  namespace: kube-system
spec:
  valuesContent: |-
    deployment:
      replicas: 1
    service:
      type: LoadBalancer
`

// ServerManifests returns files for /var/lib/rancher/k3s/server/manifests.
func ServerManifests(nodes int) map[string]string {
	if nodes == 1 {
		return map[string]string{"sb-traefik-config.yaml": traefikSingleNode}
	}
	return map[string]string{"sb-traefik-config.yaml": traefikConfig}
}

// FallbackIssuerName is the second ACME issuer used when the first cannot issue.
const FallbackIssuerName = "fallback"

// AddonParams fill the addon templates. Secrets are not part of these templates.
type AddonParams struct {
	RunID              string
	Owner              string
	ExpiresAt          string
	Location           string
	DNSZone            string
	WildcardDomain     string // e.g. *.r1234567.test.slicedbread.dev
	ACMEServer         string
	FallbackServer     string // optional; needs the fallback-eab secret
	FallbackKeyID      string // EAB key id (not secret on its own)
	CertManagerVersion string
	HcloudCSIVersion   string
}

var chartsTmpl = template.Must(template.New("charts").Parse(`apiVersion: v1
kind: Namespace
metadata:
  name: cert-manager
---
apiVersion: helm.cattle.io/v1
kind: HelmChart
metadata:
  name: cert-manager
  namespace: kube-system
spec:
  repo: https://charts.jetstack.io
  chart: cert-manager
  version: {{.CertManagerVersion}}
  targetNamespace: cert-manager
  valuesContent: |-
    crds:
      enabled: true
    extraArgs:
      - --dns01-recursive-nameservers-only
      - --dns01-recursive-nameservers=1.1.1.1:53,8.8.8.8:53
---
apiVersion: helm.cattle.io/v1
kind: HelmChart
metadata:
  name: hcloud-csi
  namespace: kube-system
spec:
  repo: https://charts.hetzner.cloud
  chart: hcloud-csi
  version: {{.HcloudCSIVersion}}
  targetNamespace: kube-system
  valuesContent: |-
    node:
      hostNetwork: true   # reads Hetzner metadata from the node; pod CIDR traffic to metadata is dropped
    controller:
      hcloudVolumeDefaultLocation: {{.Location}}
      volumeExtraLabels:
        sb/run-id: "{{.RunID}}"
        sb/owner: "{{.Owner}}"
        sb/expires-at: "{{.ExpiresAt}}"
`))

var tlsTmpl = template.Must(template.New("tls").Parse(`apiVersion: cert-manager.io/v1
kind: ClusterIssuer
metadata:
  name: ` + IssuerName + `
spec:
  acme:
    server: {{.ACMEServer}}
    privateKeySecretRef:
      name: ` + IssuerName + `-account
    solvers:
      - selector:
          dnsZones:
            - {{.DNSZone}}
        dns01:
          cloudflare:
            apiTokenSecretRef:
              name: cloudflare-api-token
              key: api-token
{{- if .FallbackServer}}
---
apiVersion: cert-manager.io/v1
kind: ClusterIssuer
metadata:
  name: ` + FallbackIssuerName + `
spec:
  acme:
    server: {{.FallbackServer}}
    externalAccountBinding:
      keyID: "{{.FallbackKeyID}}"
      keySecretRef:
        name: fallback-eab
        key: hmac
    privateKeySecretRef:
      name: ` + FallbackIssuerName + `-account
    solvers:
      - selector:
          dnsZones:
            - {{.DNSZone}}
        dns01:
          cloudflare:
            apiTokenSecretRef:
              name: cloudflare-api-token
              key: api-token
{{- end}}
---
apiVersion: cert-manager.io/v1
kind: Certificate
metadata:
  name: tenant-wildcard
  namespace: kube-system
spec:
  secretName: ` + WildcardSecret + `
  issuerRef:
    kind: ClusterIssuer
    name: ` + IssuerName + `
  dnsNames:
    - "{{.WildcardDomain}}"
---
apiVersion: traefik.io/v1alpha1
kind: TLSStore
metadata:
  name: default
  namespace: kube-system
spec:
  defaultCertificate:
    secretName: ` + WildcardSecret + `
`))

// ChartsManifest installs cert-manager and the Hetzner CSI driver.
func ChartsManifest(p AddonParams) (string, error) { return render(chartsTmpl, p) }

// TLSManifest creates the ACME issuer, the tenant wildcard certificate and Traefik's default TLS store.
func TLSManifest(p AddonParams) (string, error) { return render(tlsTmpl, p) }

// SecretsInput is passed to kubectl over stdin, never written to disk or logs.
type SecretsInput struct {
	HcloudToken     string
	CloudflareToken string
	FallbackHMAC    string // optional EAB HMAC key
}

var secretsTmpl = template.Must(template.New("secrets").Parse(`apiVersion: v1
kind: Secret
metadata:
  name: hcloud
  namespace: kube-system
type: Opaque
stringData:
  token: {{printf "%q" .HcloudToken}}
---
apiVersion: v1
kind: Secret
metadata:
  name: cloudflare-api-token
  namespace: cert-manager
type: Opaque
stringData:
  api-token: {{printf "%q" .CloudflareToken}}
{{- if .FallbackHMAC}}
---
apiVersion: v1
kind: Secret
metadata:
  name: fallback-eab
  namespace: cert-manager
type: Opaque
stringData:
  hmac: {{printf "%q" .FallbackHMAC}}
{{- end}}
---
apiVersion: v1
kind: Secret
metadata:
  name: sb-provider
  namespace: sb-system
type: Opaque
stringData:
  hcloud-token: {{printf "%q" .HcloudToken}}
  cloudflare-token: {{printf "%q" .CloudflareToken}}
`))

// SecretsManifest renders the tenant secrets. Only ever send the result over stdin.
func SecretsManifest(s SecretsInput) ([]byte, error) {
	var b bytes.Buffer
	err := secretsTmpl.Execute(&b, s)
	return b.Bytes(), err
}

func render(t *template.Template, data any) (string, error) {
	var b bytes.Buffer
	err := t.Execute(&b, data)
	return b.String(), err
}
