package apps

import (
	"crypto/sha256"
	"encoding/hex"
	"strings"
)

// Hosting modes decide the hostname format (docs/hosting-modes.md).
const (
	ModeSelfHosted = "self-hosted" // <app>.<domain>, one wildcard per install
	ModeHosted     = "hosted"      // <app>-<tenant>.<edge domain>, one wildcard for every tenant
)

// Hosts builds every hostname SlicedBread serves. Nothing else should compose hostnames.
type Hosts struct {
	Mode   string // default self-hosted
	Domain string // self-hosted: the install's base domain; hosted: the shared edge domain
	Tenant string // hosted mode only
}

// App returns the public hostname of an app.
func (h Hosts) App(name string) string { return h.host(name) }

// Auth returns the login and dashboard host.
func (h Hosts) Auth() string { return h.host("auth") }

// API returns the API host.
func (h Hosts) API() string { return h.host("api") }

func (h Hosts) host(label string) string {
	if h.Mode == ModeHosted {
		return Label(label+"-"+h.Tenant) + "." + h.Domain
	}
	return Label(label) + "." + h.Domain
}

// Label keeps a DNS label within 63 characters, shortening long ones to a prefix plus a stable hash.
func Label(s string) string {
	s = strings.ToLower(s)
	if len(s) <= 63 {
		return s
	}
	sum := sha256.Sum256([]byte(s))
	return strings.TrimRight(s[:54], "-") + "-" + hex.EncodeToString(sum[:])[:8]
}
