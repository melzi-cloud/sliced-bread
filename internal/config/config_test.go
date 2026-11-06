package config

import (
	"strings"
	"testing"
)

func TestValidateDomains(t *testing.T) {
	cases := []struct {
		zone, test string
		ok         bool
	}{
		{"slicedbread.dev", "test.slicedbread.dev", true},
		{"slicedbread.dev", "slicedbread.dev", false},
		{"slicedbread.dev", "test.example.com", false},
		{"melzi.dev", "test.melzi.dev", false},
		{"slicedbread.dev", "x.melzi.dev", false},
		{"", "test.slicedbread.dev", false},
	}
	for _, c := range cases {
		err := ValidateDomains(c.zone, c.test)
		if (err == nil) != c.ok {
			t.Errorf("ValidateDomains(%q, %q) err=%v, want ok=%v", c.zone, c.test, err, c.ok)
		}
	}
}

func TestStringRedactsSecrets(t *testing.T) {
	d := Dev{HcloudToken: "hcloud-secret-value", CloudflareToken: "cf-secret-value", CloudflareAccountID: "acct-secret"}
	for _, s := range []string{d.String(), d.GoString()} {
		for _, secret := range []string{"hcloud-secret-value", "cf-secret-value", "acct-secret"} {
			if strings.Contains(s, secret) {
				t.Fatalf("String() leaked a secret: %s", s)
			}
		}
	}
}

func TestLoadDevRequiresSecretsAndStaging(t *testing.T) {
	t.Setenv("HCLOUD_TOKEN", "")
	t.Setenv("CF_API_TOKEN", "")
	t.Setenv("CF_ACCOUNT_ID", "")
	if _, err := LoadDev(); err == nil {
		t.Fatal("expected error without secrets")
	}
	t.Setenv("HCLOUD_TOKEN", "x")
	t.Setenv("CF_API_TOKEN", "x")
	t.Setenv("CF_ACCOUNT_ID", "x")
	t.Setenv("SB_ACME_SERVER", "https://acme-v02.api.letsencrypt.org/directory")
	if _, err := LoadDev(); err == nil || !strings.Contains(err.Error(), "staging") {
		t.Fatalf("expected staging error, got %v", err)
	}
	t.Setenv("SB_ACME_SERVER", "")
	if _, err := LoadDev(); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}
