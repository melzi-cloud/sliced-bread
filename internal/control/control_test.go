package control

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/melzi-cloud/sliced-bread/internal/apps"
)

func testServer() *Server {
	return &Server{
		cfg:  Config{Domain: "r1.test.slicedbread.dev", InternalHostname: "sb-control.sb-system.svc.cluster.local"},
		auth: &Auth{key: []byte("k"), domain: "r1.test.slicedbread.dev"},
		logf: func(string, ...any) {},
	}
}

func TestSessionRoundTrip(t *testing.T) {
	a := &Auth{key: []byte("secret")}
	now := time.Unix(1_800_000_000, 0)
	v := a.NewSession("me@x.dev", now)
	if e, err := a.ParseSession(v, now.Add(time.Hour)); err != nil || e != "me@x.dev" {
		t.Fatalf("parse = %q, %v", e, err)
	}
	if _, err := a.ParseSession(v, now.Add(sessionTTL+time.Second)); err == nil {
		t.Fatal("expired session accepted")
	}
	if _, err := a.ParseSession(v[:len(v)-2]+"xx", now); err == nil {
		t.Fatal("tampered signature accepted")
	}
	other := &Auth{key: []byte("other")}
	if _, err := other.ParseSession(v, now); err == nil {
		t.Fatal("session signed with another key accepted")
	}
}

func TestVerifyStripsSessionCookie(t *testing.T) {
	s := testServer()
	sess := s.auth.NewSession("me@x.dev", time.Now())
	r := httptest.NewRequest("GET", "http://sb-control.sb-system.svc.cluster.local:8080/auth/verify?app=crm", nil)
	r.Host = "sb-control.sb-system.svc.cluster.local:8080"
	r.Header.Set("Cookie", "theme=dark; "+SessionCookie+"="+sess+"; "+csrfCookie+"=abc; app_sid=42")
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, r)
	if w.Code != 200 {
		t.Fatalf("code %d", w.Code)
	}
	if got := w.Header().Get("X-Sb-User"); got != "me@x.dev" {
		t.Fatalf("X-Sb-User = %q", got)
	}
	c := w.Header().Get("Cookie")
	if strings.Contains(c, SessionCookie) || strings.Contains(c, csrfCookie) || c != "theme=dark; app_sid=42" {
		t.Fatalf("filtered cookie header = %q", c)
	}
}

func TestVerifyRedirectsBrowsersAndRejectsAPIs(t *testing.T) {
	s := testServer()
	mk := func(accept string) *httptest.ResponseRecorder {
		r := httptest.NewRequest("GET", "/auth/verify?app=crm", nil)
		r.Host = "sb-control.sb-system.svc.cluster.local:8080"
		r.Header.Set("Accept", accept)
		r.Header.Set("X-Forwarded-Host", "crm.r1.test.slicedbread.dev")
		r.Header.Set("X-Forwarded-Uri", "/deals?x=1")
		r.Header.Set("X-Forwarded-Proto", "https")
		w := httptest.NewRecorder()
		s.Handler().ServeHTTP(w, r)
		return w
	}
	w := mk("text/html,application/xhtml+xml")
	if w.Code != http.StatusFound || !strings.HasPrefix(w.Header().Get("Location"), "https://auth.r1.test.slicedbread.dev/login?rd=https%3A%2F%2Fcrm.r1.test.slicedbread.dev%2Fdeals") {
		t.Fatalf("browser: %d %s", w.Code, w.Header().Get("Location"))
	}
	if w := mk("application/json"); w.Code != http.StatusUnauthorized {
		t.Fatalf("api client: %d", w.Code)
	}
}

func TestStripNeverBlocks(t *testing.T) {
	s := testServer()
	r := httptest.NewRequest("GET", "/auth/strip?app=vault", nil)
	r.Host = "sb-control.sb-system.svc.cluster.local:8080"
	r.Header.Set("Cookie", SessionCookie+"=x")
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, r)
	if w.Code != 200 || w.Header().Get("Cookie") != "" {
		t.Fatalf("strip: %d cookie=%q", w.Code, w.Header().Get("Cookie"))
	}
}

func TestForwardAuthNotReachableFromPublicHosts(t *testing.T) {
	s := testServer()
	for _, host := range []string{"api.r1.test.slicedbread.dev", "auth.r1.test.slicedbread.dev", "evil.example.com"} {
		r := httptest.NewRequest("GET", "/auth/verify", nil)
		r.Host = host
		w := httptest.NewRecorder()
		s.Handler().ServeHTTP(w, r)
		if w.Code == 200 {
			t.Errorf("%s served /auth/verify", host)
		}
	}
}

func TestSafeRedirect(t *testing.T) {
	d := "r1.test.slicedbread.dev"
	ok := []string{"https://crm.r1.test.slicedbread.dev/x", "https://r1.test.slicedbread.dev"}
	bad := []string{"http://crm.r1.test.slicedbread.dev", "https://evil.com", "https://r1.test.slicedbread.dev.evil.com", "//evil.com", "https://user@crm.r1.test.slicedbread.dev", "javascript:alert(1)"}
	for _, u := range ok {
		if !SafeRedirect(u, d) {
			t.Errorf("%s should be allowed", u)
		}
	}
	for _, u := range bad {
		if SafeRedirect(u, d) {
			t.Errorf("%s should be refused", u)
		}
	}
}

func TestNeedsApproval(t *testing.T) {
	wall := &apps.App{Name: "crm", Image: "x", Access: apps.AccessWall}
	public := &apps.App{Name: "crm", Image: "x", Access: apps.AccessPublic}
	if needsApproval(ScopeAgent, nil, wall) != "" {
		t.Fatal("wall deploy should not need approval")
	}
	if needsApproval(ScopeAgent, nil, public) == "" {
		t.Fatal("public deploy by agent needs approval")
	}
	if needsApproval(ScopeAdmin, nil, public) != "" {
		t.Fatal("admin never needs approval")
	}
	if needsApproval(ScopeAgent, wall, public) == "" {
		t.Fatal("wall -> public needs approval")
	}
	if needsApproval(ScopeAgent, public, wall) != "" {
		t.Fatal("narrowing access needs no approval")
	}
	v := &apps.App{Name: "vault", Template: "vaultwarden"}
	if err := apps.ApplyTemplate(v); err != nil {
		t.Fatal(err)
	}
	if why := needsApproval(ScopeAgent, nil, v); why != "" {
		t.Fatalf("template-defined app-auth needs no approval, got %s", why)
	}
	open := cloneApp(v)
	open.Settings["signups"] = "true"
	if needsApproval(ScopeAgent, v, open) == "" {
		t.Fatal("opening vaultwarden signups needs approval")
	}
	if needsApproval(ScopeAgent, open, v) != "" {
		t.Fatal("closing signups needs no approval")
	}
	u := &apps.App{Name: "analytics", Template: "umami"}
	apps.ApplyTemplate(u)
	if why := needsApproval(ScopeAgent, nil, u); why != "" {
		t.Fatalf("template public paths need no approval, got %s", why)
	}
	withPaths := &apps.App{Name: "crm", Image: "x", Access: apps.AccessWall, PublicPaths: []string{"/hook"}}
	if needsApproval(ScopeAgent, wall, withPaths) == "" {
		t.Fatal("adding public paths needs approval")
	}
}
