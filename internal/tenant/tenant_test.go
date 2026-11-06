package tenant

import (
	"strings"
	"testing"

	"github.com/melzi-cloud/sliced-bread/internal/sshx"
)

func nodeConfig(t *testing.T, init bool) NodeConfig {
	t.Helper()
	hk, err := sshx.NewKeyPair("host")
	if err != nil {
		t.Fatal(err)
	}
	return NodeConfig{
		Init: init, JoinAddr: "10.0.1.2", Token: "abc123", K3sVersion: "v1.37.2+k3s1",
		HostKeyPriv: string(hk.PrivatePEM), HostKeyPub: hk.AuthorizedKey, Manifests: ServerManifests(),
	}
}

func TestRenderUserDataInit(t *testing.T) {
	ud, err := RenderUserData(nodeConfig(t, true))
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"#cloud-config", "ed25519_private: |", "    -----BEGIN OPENSSH PRIVATE KEY-----",
		"--cluster-init", "INSTALL_K3S_VERSION='v1.37.2+k3s1'", "--disable servicelb", "--disable local-storage",
		"/var/lib/rancher/k3s/server/manifests/sb-traefik-config.yaml", "nodePort: 30443", "--token-file /etc/sb/k3s-token", "/etc/rancher/k3s/registries.yaml", "http://10.43.200.200:5000",
		"ip saddr 10.42.0.0/16 ip daddr 169.254.169.254 counter drop", "systemctl enable --now sb-metadata-block.service", "ExecStart=/usr/sbin/nft -f /etc/sb/metadata-block.nft",
	} {
		if !strings.Contains(ud, want) {
			t.Errorf("user data missing %q", want)
		}
	}
	if strings.Contains(ud, "--server https://") {
		t.Error("init node must not join another server")
	}
	if strings.Contains(ud, "\t") {
		t.Error("user data must not contain tabs (invalid YAML indentation)")
	}
}

func TestRenderUserDataJoin(t *testing.T) {
	ud, err := RenderUserData(nodeConfig(t, false))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(ud, "--server https://10.0.1.2:6443") || strings.Contains(ud, "--cluster-init") {
		t.Fatal("join node must join the first server and not init a cluster")
	}
	c := nodeConfig(t, false)
	c.JoinAddr = ""
	if _, err := RenderUserData(c); err == nil {
		t.Fatal("join node without JoinAddr must fail")
	}
	c = nodeConfig(t, true)
	c.Token = "bad token"
	if _, err := RenderUserData(c); err == nil {
		t.Fatal("token with whitespace must fail")
	}
}

func TestManifests(t *testing.T) {
	p := AddonParams{
		RunID: "rabcdefg", Owner: "me", ExpiresAt: "1800000000", Location: "nbg1", DNSZone: "slicedbread.dev",
		WildcardDomain: "*.rabcdefg.test.slicedbread.dev", ACMEServer: "https://acme-staging-v02.api.letsencrypt.org/directory",
		CertManagerVersion: "v1.21.3", HcloudCSIVersion: "2.24.1",
	}
	charts, err := ChartsManifest(p)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"version: v1.21.3", "version: 2.24.1", "hostNetwork: true", `sb/run-id: "rabcdefg"`, `sb/expires-at: "1800000000"`, "hcloudVolumeDefaultLocation: nbg1"} {
		if !strings.Contains(charts, want) {
			t.Errorf("charts missing %q", want)
		}
	}
	tlsM, err := TLSManifest(p)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"acme-staging-v02", `"*.rabcdefg.test.slicedbread.dev"`, "kind: TLSStore", "secretName: " + WildcardSecret, "- slicedbread.dev"} {
		if !strings.Contains(tlsM, want) {
			t.Errorf("tls manifest missing %q", want)
		}
	}
	sec, err := SecretsManifest(SecretsInput{HcloudToken: `tok"en`, CloudflareToken: "cf"})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(sec), `token: "tok\"en"`) {
		t.Errorf("secret values must be quoted safely:\n%s", sec)
	}
}

func TestRunFromName(t *testing.T) {
	cases := map[string]string{
		"_acme-challenge.rabcdefg.test.slicedbread.dev":   "rabcdefg",
		"_acme-challenge.x.rabcdefg.test.slicedbread.dev": "rabcdefg",
		"_acme-challenge.whatever.test.slicedbread.dev":   "",
	}
	for name, want := range cases {
		if got := runFromName(name, "test.slicedbread.dev"); got != want {
			t.Errorf("runFromName(%q) = %q, want %q", name, got, want)
		}
	}
}
