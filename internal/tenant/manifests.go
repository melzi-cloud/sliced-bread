package tenant

import (
	"bytes"
	"text/template"
)

const (
	NodePortHTTP  = 30080
	NodePortHTTPS = 30443

	WildcardSecret = "tenant-wildcard-tls"
	IssuerName     = "letsencrypt-staging"
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
    service:
      type: NodePort
    ports:
      web:
        nodePort: 30080
      websecure:
        nodePort: 30443
`

// ServerManifests returns files for /var/lib/rancher/k3s/server/manifests.
func ServerManifests() map[string]string {
	return map[string]string{"sb-traefik-config.yaml": traefikConfig}
}

// AddonParams fill the addon templates. Secrets are not part of these templates.
type AddonParams struct {
	RunID              string
	Owner              string
	ExpiresAt          string
	Location           string
	DNSZone            string
	WildcardDomain     string // e.g. *.r1234567.test.slicedbread.dev
	ACMEServer         string
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
