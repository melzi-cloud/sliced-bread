// Package labels defines the labels every development cloud resource carries.
package labels

import (
	"crypto/rand"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"
)

const (
	RunID     = "sb/run-id"
	Owner     = "sb/owner"
	ExpiresAt = "sb/expires-at" // unix seconds; label values cannot contain ':'
	Role      = "sb/role"
)

// For returns the base labels for a run.
func For(runID, owner string, expires time.Time) map[string]string {
	return map[string]string{
		RunID:     runID,
		Owner:     owner,
		ExpiresAt: strconv.FormatInt(expires.Unix(), 10),
	}
}

// With returns a copy of base with extra labels added.
func With(base map[string]string, kv ...string) map[string]string {
	out := make(map[string]string, len(base)+len(kv)/2)
	for k, v := range base {
		out[k] = v
	}
	for i := 0; i+1 < len(kv); i += 2 {
		out[kv[i]] = kv[i+1]
	}
	return out
}

// Expired reports whether a resource's expiry has passed. Resources without a
// parseable expiry are treated as expired only if they carry a run id, so
// half-labelled leftovers from a crashed run still get cleaned up.
func Expired(l map[string]string, now time.Time) bool {
	v, ok := l[ExpiresAt]
	if !ok {
		return l[RunID] != ""
	}
	ts, err := strconv.ParseInt(v, 10, 64)
	if err != nil {
		return l[RunID] != ""
	}
	return now.Unix() >= ts
}

// Selector returns a label selector matching one run.
func Selector(runID string) string { return RunID + "=" + runID }

var runIDPattern = regexp.MustCompile(`^r[a-z0-9]{7}$`)

// NewRunID returns a short, DNS-safe run id such as "r7k2m9qa".
func NewRunID() string {
	const alphabet = "abcdefghijklmnopqrstuvwxyz0123456789"
	b := make([]byte, 7)
	if _, err := rand.Read(b); err != nil {
		panic(fmt.Sprintf("crypto/rand: %v", err))
	}
	var sb strings.Builder
	sb.WriteByte('r')
	for _, c := range b {
		sb.WriteByte(alphabet[int(c)%len(alphabet)])
	}
	return sb.String()
}

// ValidRunID reports whether s looks like a run id produced by NewRunID.
func ValidRunID(s string) bool { return runIDPattern.MatchString(s) }

// DNSComment encodes run labels into a Cloudflare record comment.
func DNSComment(runID string, expires time.Time) string {
	return fmt.Sprintf("%s=%s %s=%d", RunID, runID, ExpiresAt, expires.Unix())
}

// ParseDNSComment extracts labels from a record comment written by DNSComment.
func ParseDNSComment(c string) map[string]string {
	out := map[string]string{}
	for _, f := range strings.Fields(c) {
		k, v, ok := strings.Cut(f, "=")
		if ok && (k == RunID || k == ExpiresAt) {
			out[k] = v
		}
	}
	return out
}
