// Package config loads development settings from the environment.
//
// Secrets arrive through environment variables set by hack/secrets-env.
// Never log a Dev value: String() redacts secrets, but fields are still exported.
package config

import (
	"errors"
	"fmt"
	"os"
	"os/user"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// Dev holds everything needed to create a throwaway development tenant.
type Dev struct {
	HcloudToken         string
	CloudflareToken     string
	CloudflareAccountID string

	DNSZone    string // e.g. slicedbread.dev
	TestDomain string // e.g. test.slicedbread.dev

	Location   string
	ServerType string
	Image      string
	LBType     string
	K3sVersion string

	CertManagerVersion string
	HcloudCSIVersion   string
	ACMEServer         string

	Owner string
	TTL   time.Duration

	// Self-hosted installs (sb init). Dev runs leave these empty.
	Domain   string // base domain for apps; empty means <run-id>.<TestDomain>
	Nodes    int    // 1 or 3; 0 means 3
	NoExpiry bool   // label resources sb/expires-at=never
	Fallback *EAB   // optional second ACME issuer
}

// EAB is an ACME external account binding for the fallback issuer (ZeroSSL).
type EAB struct {
	Server string
	KeyID  string
	HMAC   string
}

// FallbackServers maps issuer names to ACME directories.
var FallbackServers = map[string]string{
	"zerossl": "https://acme.zerossl.com/v2/DV90",
}

// LetsEncryptProduction is the production ACME directory.
const LetsEncryptProduction = "https://acme-v02.api.letsencrypt.org/directory"

// NodeCount returns the configured node count.
func (d Dev) NodeCount() int {
	if d.Nodes == 0 {
		return 3
	}
	return d.Nodes
}

// DNSScope is the name under which this provisioner's DNS records live.
func (d Dev) DNSScope() string {
	if d.Domain != "" {
		return d.Domain
	}
	return d.TestDomain
}

// Defaults that are safe for development.
const (
	DefaultLocation           = "nbg1"
	DefaultServerType         = "cx23"
	DefaultImage              = "debian-13"
	DefaultLBType             = "lb11"
	DefaultK3sVersion         = "v1.37.2+k3s1"
	DefaultCertManagerVersion = "v1.21.3"
	DefaultHcloudCSIVersion   = "2.24.1"
	LetsEncryptStaging        = "https://acme-staging-v02.api.letsencrypt.org/directory"
	DefaultTTL                = 2 * time.Hour
)

// LoadDev reads the development configuration from the environment.
func LoadDev() (Dev, error) {
	d := Dev{
		HcloudToken:         os.Getenv("HCLOUD_TOKEN"),
		CloudflareToken:     os.Getenv("CF_API_TOKEN"),
		CloudflareAccountID: os.Getenv("CF_ACCOUNT_ID"),
		DNSZone:             env("SB_DNS_ZONE", "slicedbread.dev"),
		TestDomain:          env("SB_TEST_DOMAIN", "test.slicedbread.dev"),
		Location:            env("SB_LOCATION", DefaultLocation),
		ServerType:          env("SB_SERVER_TYPE", DefaultServerType),
		Image:               env("SB_IMAGE", DefaultImage),
		LBType:              env("SB_LB_TYPE", DefaultLBType),
		K3sVersion:          env("SB_K3S_VERSION", DefaultK3sVersion),
		CertManagerVersion:  env("SB_CERT_MANAGER_VERSION", DefaultCertManagerVersion),
		HcloudCSIVersion:    env("SB_HCLOUD_CSI_VERSION", DefaultHcloudCSIVersion),
		ACMEServer:          env("SB_ACME_SERVER", LetsEncryptStaging),
		Owner:               env("SB_OWNER", defaultOwner()),
		TTL:                 DefaultTTL,
	}
	if v := os.Getenv("SB_NODES"); v == "1" || v == "3" {
		d.Nodes, _ = strconv.Atoi(v) // dev: SB_NODES=1 for a one-server prototype
	}
	if v := os.Getenv("SB_TTL"); v != "" {
		ttl, err := time.ParseDuration(v)
		if err != nil {
			return Dev{}, fmt.Errorf("SB_TTL: %w", err)
		}
		d.TTL = ttl
	}
	return d, d.Validate()
}

// Validate checks required values and safety rules.
func (d Dev) Validate() error {
	var errs []error
	if d.HcloudToken == "" {
		errs = append(errs, errors.New("HCLOUD_TOKEN is not set (run through hack/secrets-env)"))
	}
	if d.CloudflareToken == "" {
		errs = append(errs, errors.New("CF_API_TOKEN is not set (run through hack/secrets-env)"))
	}
	if d.CloudflareAccountID == "" {
		errs = append(errs, errors.New("CF_ACCOUNT_ID is not set (run through hack/secrets-env)"))
	}
	if err := ValidateDomains(d.DNSZone, d.TestDomain); err != nil {
		errs = append(errs, err)
	}
	if d.TTL <= 0 || d.TTL > 24*time.Hour {
		errs = append(errs, fmt.Errorf("TTL %s must be between 0 and 24h", d.TTL))
	}
	if d.ACMEServer != LetsEncryptStaging && os.Getenv("SB_ALLOW_PRODUCTION_ACME") != "1" {
		errs = append(errs, errors.New("development tenants must use Let's Encrypt staging"))
	}
	return errors.Join(errs...)
}

// ValidateDomains enforces that test records stay inside the test subdomain.
// The Cloudflare token's zone scope is what limits which zones can be touched.
func ValidateDomains(zone, testDomain string) error {
	zone = strings.TrimSuffix(strings.ToLower(zone), ".")
	testDomain = strings.TrimSuffix(strings.ToLower(testDomain), ".")
	if zone == "" || testDomain == "" {
		return errors.New("DNS zone and test domain must be set")
	}
	if !strings.HasSuffix(testDomain, "."+zone) {
		return fmt.Errorf("test domain %q must be a subdomain of zone %q", testDomain, zone)
	}
	return nil
}

// String never includes secrets.
func (d Dev) String() string {
	return fmt.Sprintf("Dev{zone=%s testDomain=%s location=%s serverType=%s image=%s lb=%s k3s=%s owner=%s ttl=%s secrets=redacted}",
		d.DNSZone, d.TestDomain, d.Location, d.ServerType, d.Image, d.LBType, d.K3sVersion, d.Owner, d.TTL)
}

// GoString keeps %#v from printing secrets.
func (d Dev) GoString() string { return d.String() }

func env(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

var labelValueUnsafe = regexp.MustCompile(`[^a-z0-9._-]+`)

func defaultOwner() string {
	name := os.Getenv("USER")
	if u, err := user.Current(); err == nil && u.Username != "" {
		name = u.Username
	}
	name = labelValueUnsafe.ReplaceAllString(strings.ToLower(name), "-")
	name = strings.Trim(name, "-._")
	if name == "" {
		return "agent"
	}
	if len(name) > 40 {
		name = name[:40]
	}
	return name
}
