package labels

import (
	"testing"
	"time"
)

func TestRunID(t *testing.T) {
	seen := map[string]bool{}
	for i := 0; i < 200; i++ {
		id := NewRunID()
		if !ValidRunID(id) {
			t.Fatalf("invalid run id %q", id)
		}
		seen[id] = true
	}
	if len(seen) < 190 {
		t.Fatalf("run ids collide too often: %d unique of 200", len(seen))
	}
	for _, bad := range []string{"", "R1234567", "r123", "r1234567x", "r12_4567"} {
		if ValidRunID(bad) {
			t.Errorf("ValidRunID(%q) = true", bad)
		}
	}
}

func TestExpired(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	cases := []struct {
		name string
		l    map[string]string
		want bool
	}{
		{"future", For("rabcdefg", "me", now.Add(time.Hour)), false},
		{"past", For("rabcdefg", "me", now.Add(-time.Second)), true},
		{"exactly now", For("rabcdefg", "me", now), true},
		{"run id without expiry", map[string]string{RunID: "rabcdefg"}, true},
		{"garbage expiry", map[string]string{RunID: "rabcdefg", ExpiresAt: "soon"}, true},
		{"unrelated resource", map[string]string{"app": "x"}, false},
	}
	for _, c := range cases {
		if got := Expired(c.l, now); got != c.want {
			t.Errorf("%s: Expired = %v, want %v", c.name, got, c.want)
		}
	}
}

func TestDNSCommentRoundTrip(t *testing.T) {
	exp := time.Unix(1_800_000_123, 0)
	l := ParseDNSComment(DNSComment("rabcdefg", exp))
	if l[RunID] != "rabcdefg" || l[ExpiresAt] != "1800000123" {
		t.Fatalf("round trip failed: %v", l)
	}
	if len(ParseDNSComment("hand made record")) != 0 {
		t.Fatal("unrelated comment should parse to no labels")
	}
}

func TestWith(t *testing.T) {
	base := For("rabcdefg", "me", time.Unix(1, 0))
	l := With(base, Role, "server")
	if l[Role] != "server" || l[RunID] != "rabcdefg" {
		t.Fatalf("With: %v", l)
	}
	if _, ok := base[Role]; ok {
		t.Fatal("With must not mutate base")
	}
}
