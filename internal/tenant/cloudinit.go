package tenant

import (
	"bytes"
	"fmt"
	"strings"
	"text/template"
)

// NodeConfig is everything a node needs at first boot.
//
// Note: Hetzner user data is readable through the metadata service from inside
// the server, including from pods. The join token and host key are per-run and
// short-lived in development. Production must block pod access to the metadata
// service or deliver these differently.
type NodeConfig struct {
	Init        bool   // first server: --cluster-init
	JoinAddr    string // private IP of the first server, for joining nodes
	Token       string
	K3sVersion  string
	HostKeyPriv string // OpenSSH private host key
	HostKeyPub  string // authorized_keys format
	Manifests   map[string]string
}

var userDataTmpl = template.Must(template.New("ud").Funcs(template.FuncMap{
	"indent": func(n int, s string) string {
		pad := strings.Repeat(" ", n)
		lines := strings.Split(strings.TrimRight(s, "\n"), "\n")
		for i, l := range lines {
			lines[i] = pad + l
		}
		return strings.Join(lines, "\n")
	},
	"sq": shellQuote,
}).Parse(`#cloud-config
ssh_deletekeys: true
ssh_genkeytypes: []
ssh_keys:
  ed25519_private: |
{{indent 4 .HostKeyPriv}}
  ed25519_public: {{.HostKeyPub}}
write_files:
  - path: /etc/rancher/k3s/registries.yaml
    permissions: "0600"
    owner: root:root
    content: |
{{indent 6 .Registries}}
  - path: /etc/sb/metadata-block.nft
    permissions: "0644"
    owner: root:root
    content: |
{{indent 6 .MetadataBlock}}
  - path: /etc/systemd/system/sb-metadata-block.service
    permissions: "0644"
    owner: root:root
    content: |
{{indent 6 .MetadataUnit}}
  - path: /etc/sb/k3s-token
    permissions: "0600"
    owner: root:root
    content: {{.Token}}
{{- range $name, $body := .Manifests}}
  - path: /var/lib/rancher/k3s/server/manifests/{{$name}}
    permissions: "0600"
    owner: root:root
    content: |
{{indent 6 $body}}
{{- end}}
  - path: /usr/local/sbin/sb-bootstrap
    permissions: "0700"
    owner: root:root
    content: |
{{indent 6 .Script}}
runcmd:
  - [/usr/local/sbin/sb-bootstrap]
`))

var scriptTmpl = template.Must(template.New("script").Funcs(template.FuncMap{"sq": shellQuote}).Parse(`#!/bin/bash
set -euo pipefail
exec >>/var/log/sb-bootstrap.log 2>&1
echo "sb-bootstrap start $(date -Is)"

PRIV_IF=""
for _ in $(seq 1 90); do
  PRIV_IF=$(ip -4 -o addr show | awk '$4 ~ /^10\.0\./ {print $2; exit}')
  [ -n "$PRIV_IF" ] && break
  sleep 2
done
if [ -z "$PRIV_IF" ]; then echo "private network interface not found"; exit 1; fi
PRIV_IP=$(ip -4 -o addr show dev "$PRIV_IF" | awk '$4 ~ /^10\.0\./ {split($4,a,"/"); print a[1]; exit}')
echo "private interface $PRIV_IF $PRIV_IP"

if ! command -v curl >/dev/null || ! command -v nft >/dev/null; then
  apt-get update -qq
  DEBIAN_FRONTEND=noninteractive apt-get install -y -qq curl nftables
fi
# Pods must not reach the Hetzner metadata service: user data holds the join token and host keys.
systemctl daemon-reload
systemctl enable --now sb-metadata-block.service

curl -sfL --retry 5 https://get.k3s.io -o /root/k3s-install.sh
export INSTALL_K3S_VERSION={{sq .K3sVersion}}
ARGS=(server
  --token-file /etc/sb/k3s-token
  --node-ip "$PRIV_IP"
  --advertise-address "$PRIV_IP"
  --flannel-iface "$PRIV_IF"
  --disable servicelb
  --disable local-storage
  --secrets-encryption
  --write-kubeconfig-mode 0600)
{{- if .Init}}
ARGS+=(--cluster-init)
{{- else}}
ARGS+=(--server https://{{.JoinAddr}}:6443)
{{- end}}
sh /root/k3s-install.sh "${ARGS[@]}"
echo "sb-bootstrap done $(date -Is)"
`))

// RenderUserData renders the cloud-config for one node.
func RenderUserData(c NodeConfig) (string, error) {
	if c.Token == "" || c.K3sVersion == "" || c.HostKeyPriv == "" || c.HostKeyPub == "" {
		return "", fmt.Errorf("node config incomplete")
	}
	if !c.Init && c.JoinAddr == "" {
		return "", fmt.Errorf("joining node needs JoinAddr")
	}
	if strings.ContainsAny(c.Token, " \n\"'") {
		return "", fmt.Errorf("token must not contain whitespace or quotes")
	}
	var script bytes.Buffer
	if err := scriptTmpl.Execute(&script, c); err != nil {
		return "", err
	}
	var out bytes.Buffer
	err := userDataTmpl.Execute(&out, struct {
		NodeConfig
		Script        string
		Registries    string
		MetadataBlock string
		MetadataUnit  string
	}{c, script.String(), registriesYAML, metadataBlockNft, metadataBlockUnit})
	if err != nil {
		return "", err
	}
	if out.Len() > 32*1024 {
		return "", fmt.Errorf("user data is %d bytes, Hetzner allows 32KiB", out.Len())
	}
	return out.String(), nil
}

// PodCIDR is the k3s default cluster CIDR.
const PodCIDR = "10.42.0.0/16"

// metadataBlockNft drops pod traffic to the Hetzner metadata service. Pod traffic to
// 169.254.169.254 is routed through the host, so it passes the forward hook; host
// processes (cloud-init, k3s, host-network pods) are unaffected.
const metadataBlockNft = `table inet sb_metadata
delete table inet sb_metadata
table inet sb_metadata {
  chain forward {
    type filter hook forward priority -5; policy accept;
    ip saddr ` + PodCIDR + ` ip daddr 169.254.169.254 counter drop
  }
}
`

const metadataBlockUnit = `[Unit]
Description=Block pod access to the Hetzner metadata service
Before=k3s.service
After=network-pre.target

[Service]
Type=oneshot
RemainAfterExit=yes
ExecStart=/usr/sbin/nft -f /etc/sb/metadata-block.nft

[Install]
WantedBy=multi-user.target
`

func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}
