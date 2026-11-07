package control

import (
	"context"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/melzi-cloud/sliced-bread/internal/kube"
)

func TestOwnerAndTokens(t *testing.T) {
	ctx := context.Background()
	a := &Auth{key: []byte("k"), domain: "d"}
	if a.HasUsers() {
		t.Fatal("fresh install has no users")
	}
	if err := a.CreateOwner(ctx, "owner@x.dev", "short"); err == nil {
		t.Fatal("short password accepted")
	}
	if err := a.CreateOwner(ctx, "Owner@X.dev", "a long enough password"); err != nil {
		t.Fatal(err)
	}
	if a.Role("owner@x.dev") != RoleAdmin || a.OwnerEmail() != "owner@x.dev" {
		t.Fatal("owner is an admin")
	}
	if err := a.CreateOwner(ctx, "second@x.dev", "a long enough password"); err == nil {
		t.Fatal("a second owner was created through setup")
	}
	if _, err := a.CreateAdminToken(ctx, "ci", "owner@x.dev", 0); err == nil {
		t.Fatal("admin tokens must expire")
	}
	if _, err := a.CreateAdminToken(ctx, "ci", "owner@x.dev", 91*24*time.Hour); err == nil {
		t.Fatal("admin tokens expire within 90 days")
	}
	tok, err := a.CreateAdminToken(ctx, "ci", "owner@x.dev", time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	r := httptest.NewRequest("GET", "/", nil)
	r.Header.Set("Authorization", "Bearer "+tok)
	if a.TokenScope(r) != ScopeAdmin {
		t.Fatal("admin token works")
	}
	a.st.Tokens[len(a.st.Tokens)-1].Expires = time.Now().Add(-time.Second).Unix()
	if a.TokenScope(r) != "" {
		t.Fatal("expired token still works")
	}
	list := a.Tokens(time.Now())
	if len(list) != 1 || !list[0].Expired {
		t.Fatalf("token list %+v", list)
	}
	if err := a.RevokeToken(ctx, list[0].ID); err != nil || len(a.Tokens(time.Now())) != 0 {
		t.Fatal("revoke")
	}
}

func TestSwitchDecision(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	cases := []struct {
		issuer, ready string
		since, after  time.Time
		want          bool
	}{
		{"letsencrypt", "True", now.Add(-time.Hour), now.Add(60 * 24 * time.Hour), false},
		{"letsencrypt", "False", now.Add(-10 * time.Minute), time.Time{}, false},
		{"letsencrypt", "False", now.Add(-time.Hour), time.Time{}, true},
		{"letsencrypt", "True", now.Add(-time.Hour), now.Add(10 * 24 * time.Hour), true},
		{"fallback", "False", now.Add(-time.Hour), now.Add(time.Hour), false},
	}
	for i, c := range cases {
		if got := switchDecision(c.issuer, c.ready, c.since, c.after, now); got != c.want {
			t.Errorf("case %d: %v", i, got)
		}
	}
}

func TestConditionStatusWithoutStatus(t *testing.T) {
	if st, _ := conditionStatus(map[string]any{"metadata": map[string]any{}}, "Ready"); st != "Unknown" {
		t.Fatal(st)
	}
	if len(nodeSources([]kube.Obj{{"spec": map[string]any{}}})) != 0 {
		t.Fatal("node without status")
	}
}
