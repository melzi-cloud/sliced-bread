package control

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/melzi-cloud/sliced-bread/internal/apps"
)

func testServer() *Server {
	return &Server{
		cfg:  Config{Domain: "r1.test.slicedbread.dev", InternalHostname: "sb-control.sb-system.svc.cluster.local"},
		auth: &Auth{key: []byte("k"), domain: "r1.test.slicedbread.dev", st: authState{Users: []user{{Email: "me@x.dev", Role: RoleAdmin}}}},
		logf: func(string, ...any) {},
	}
}

func TestSessionRoundTrip(t *testing.T) {
	a := &Auth{key: []byte("secret"), domain: "r1.test.slicedbread.dev"}
	now := time.Unix(1_800_000_000, 0)
	v := a.NewSession("me@x.dev", "crm.r1.test.slicedbread.dev", kindApp, time.Hour, now)
	if e, err := a.ParseSession(v, "crm.r1.test.slicedbread.dev", kindApp, now); err != nil || e != "me@x.dev" {
		t.Fatalf("parse = %q, %v", e, err)
	}
	bad := []struct {
		name string
		v    string
		host string
		kind string
		at   time.Time
		auth *Auth
	}{
		{"expired", v, "crm.r1.test.slicedbread.dev", kindApp, now.Add(2 * time.Hour), a},
		{"other host", v, "vault.r1.test.slicedbread.dev", kindApp, now, a},
		{"other kind", v, "crm.r1.test.slicedbread.dev", kindAuth, now, a},
		{"tampered", v[:len(v)-2] + "xx", "crm.r1.test.slicedbread.dev", kindApp, now, a},
		{"other key", v, "crm.r1.test.slicedbread.dev", kindApp, now, &Auth{key: []byte("other"), domain: a.domain}},
		{"other install", v, "crm.r1.test.slicedbread.dev", kindApp, now, &Auth{key: []byte("secret"), domain: "r2.test.slicedbread.dev"}},
	}
	for _, b := range bad {
		if _, err := b.auth.ParseSession(b.v, b.host, b.kind, b.at); err == nil {
			t.Errorf("%s: accepted", b.name)
		}
	}
}

func verifyReq(host, uri string, cookies ...*http.Cookie) *http.Request {
	r := httptest.NewRequest("GET", "/auth/verify?app=crm", nil)
	r.Host = "sb-control.sb-system.svc.cluster.local:8080"
	r.Header.Set("X-Forwarded-Host", host)
	r.Header.Set("X-Forwarded-Uri", uri)
	r.Header.Set("X-Forwarded-Proto", "https")
	r.Header.Set("Accept", "text/html")
	for _, c := range cookies {
		r.AddCookie(c)
	}
	return r
}

func TestExchangeGivesAppHostItsOwnSession(t *testing.T) {
	s := testServer()
	host := "crm.r1.test.slicedbread.dev"
	loc := s.exchangeURL("me@x.dev", "https://"+host+"/deals?id=7")
	u, _ := url.Parse(loc)
	if u.Host != host || u.Path != exchangePath {
		t.Fatalf("exchange url %s", loc)
	}
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, verifyReq(host, u.RequestURI()))
	if w.Code != http.StatusFound || w.Header().Get("Location") != "https://"+host+"/deals?id=7" {
		t.Fatalf("exchange: %d %s", w.Code, w.Header().Get("Location"))
	}
	sc := w.Result().Cookies()
	if len(sc) != 1 || sc[0].Name != AppCookie || sc[0].Domain != "" || !sc[0].Secure || sc[0].Path != "/" {
		t.Fatalf("app cookie must be host-only __Host-: %+v", sc)
	}
	// The same link cannot be used twice.
	w2 := httptest.NewRecorder()
	s.Handler().ServeHTTP(w2, verifyReq(host, u.RequestURI()))
	if w2.Code != http.StatusForbidden {
		t.Fatalf("replayed exchange: %d", w2.Code)
	}
	// A link minted for one app does not work on another.
	other := s.exchangeURL("me@x.dev", "https://vault.r1.test.slicedbread.dev/")
	ou, _ := url.Parse(other)
	w3 := httptest.NewRecorder()
	s.Handler().ServeHTTP(w3, verifyReq(host, ou.RequestURI()))
	if w3.Code != http.StatusForbidden {
		t.Fatalf("cross-host exchange: %d", w3.Code)
	}
	// The minted cookie opens the wall, and the app never sees it.
	w4 := httptest.NewRecorder()
	s.Handler().ServeHTTP(w4, verifyReq(host, "/deals", sc[0], &http.Cookie{Name: "theme", Value: "dark"}))
	if w4.Code != 200 || w4.Header().Get("X-Sb-User") != "me@x.dev" || w4.Header().Get("Cookie") != "theme=dark" {
		t.Fatalf("verify: %d user=%q cookie=%q", w4.Code, w4.Header().Get("X-Sb-User"), w4.Header().Get("Cookie"))
	}
	// Open-redirect attempts fall back to /.
	evil := s.exchangeURL("me@x.dev", "https://"+host+"/")
	eu, _ := url.Parse(evil)
	q := eu.Query()
	q.Set("rd", "//evil.com/x")
	eu.RawQuery = q.Encode()
	w5 := httptest.NewRecorder()
	s.Handler().ServeHTTP(w5, verifyReq(host, eu.RequestURI()))
	if w5.Header().Get("Location") != "https://"+host+"/" {
		t.Fatalf("open redirect: %s", w5.Header().Get("Location"))
	}
}

// A sibling host (another tenant, or any app) can set cookies for the parent domain.
// Those must not open the wall, shadow a real session, or break approvals.
func TestConflictingParentDomainCookies(t *testing.T) {
	s := testServer()
	host := "crm.r1.test.slicedbread.dev"
	good := &http.Cookie{Name: AppCookie, Value: s.auth.NewSession("me@x.dev", host, kindApp, time.Hour, time.Now())}
	junk := []*http.Cookie{
		{Name: "sb_session", Value: "attacker"}, {Name: "sb_csrf", Value: "attacker"},
		{Name: "sb_s_r1", Value: "attacker"},
		// A session minted for another host of the same install, replayed here.
		{Name: AppCookie, Value: s.auth.NewSession("me@x.dev", "evil.r1.test.slicedbread.dev", kindApp, time.Hour, time.Now())},
	}
	// Junk alone does not open the wall.
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, verifyReq(host, "/", junk...))
	if w.Code == 200 {
		t.Fatal("junk cookies opened the wall")
	}
	// Junk next to a real session (in either order) does not break it, and none of it reaches the app.
	for _, order := range [][]*http.Cookie{append(append([]*http.Cookie{}, junk...), good), append([]*http.Cookie{good}, junk...)} {
		w := httptest.NewRecorder()
		s.Handler().ServeHTTP(w, verifyReq(host, "/", order...))
		if w.Code != 200 || w.Header().Get("X-Sb-User") != "me@x.dev" {
			t.Fatalf("real session shadowed: %d", w.Code)
		}
		if strings.Contains(w.Header().Get("Cookie"), "attacker") || strings.Contains(w.Header().Get("Cookie"), "sb_") {
			t.Fatalf("our cookies leaked to the app: %q", w.Header().Get("Cookie"))
		}
	}

	// Approvals form: CSRF is bound to the session, so a planted sb_csrf cookie neither helps nor hurts.
	auth := "auth.r1.test.slicedbread.dev"
	sess := &http.Cookie{Name: SessionCookie, Value: s.auth.NewSession("me@x.dev", auth, kindAuth, time.Hour, time.Now())}
	post := func(token string, cookies ...*http.Cookie) bool {
		r := httptest.NewRequest("POST", "https://"+auth+"/approvals/x", strings.NewReader("csrf="+url.QueryEscape(token)))
		r.Host = auth
		r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		for _, c := range cookies {
			r.AddCookie(c)
		}
		return s.checkCSRF(r)
	}
	if !post(s.sessionCSRF(sess.Value), append([]*http.Cookie{sess}, junk...)...) {
		t.Fatal("planted cookies broke a valid approval")
	}
	if post("attacker", append([]*http.Cookie{sess}, junk...)...) {
		t.Fatal("attacker-chosen CSRF value accepted")
	}
	if post("attacker", &http.Cookie{Name: "__Host-sb_csrf", Value: "attacker"}, &http.Cookie{Name: "sb_csrf", Value: "attacker"}, sess) {
		t.Fatal("double-submit cookie must not apply once signed in")
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
	// The same template from a local directory is not trusted: its app-auth default and
	// public paths need a human.
	local := cloneApp(v)
	local.TemplateDef.Trusted = false
	if needsApproval(ScopeAgent, nil, local) == "" {
		t.Fatal("app-auth from an untrusted template needs approval")
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

func TestDeviceFlow(t *testing.T) {
	s := testServer()
	s.devices = deviceStore{grants: map[string]*deviceGrant{}}
	api := s.apiMux()
	call := func(method, path string) *httptest.ResponseRecorder {
		r := httptest.NewRequest(method, path, nil)
		w := httptest.NewRecorder()
		api.ServeHTTP(w, r)
		return w
	}
	w := call("POST", "/v1/device?label=laptop")
	if w.Code != 201 {
		t.Fatalf("start: %d %s", w.Code, w.Body)
	}
	var start struct{ Code, Poll, VerifyURL string }
	json.Unmarshal(w.Body.Bytes(), &start)
	if !strings.Contains(start.VerifyURL, "https://auth.r1.test.slicedbread.dev/device?code="+start.Code) || len(start.Code) != 9 {
		t.Fatalf("bad start response %+v", start)
	}
	if w := call("GET", "/v1/device/"+start.Poll); w.Code != 202 {
		t.Fatalf("pending poll: %d", w.Code)
	}
	if w := call("GET", "/v1/device/wrong"); w.Code != 404 {
		t.Fatalf("unknown poll: %d", w.Code)
	}
	if err := s.approveDevice(context.Background(), "NOPE-NOPE", "me", true); err == nil {
		t.Fatal("unknown code must fail")
	}
	if err := s.approveDevice(context.Background(), start.Code, "me", true); err != nil {
		t.Fatal(err)
	}
	w = call("GET", "/v1/device/"+start.Poll)
	var got struct{ Token, Scope string }
	json.Unmarshal(w.Body.Bytes(), &got)
	if w.Code != 200 || !strings.HasPrefix(got.Token, "sbt_") || got.Scope != ScopeAgent {
		t.Fatalf("approved poll: %d %s", w.Code, w.Body)
	}
	r := httptest.NewRequest("GET", "/v1/whoami", nil)
	r.Header.Set("Authorization", "Bearer "+got.Token)
	if s.auth.TokenScope(r) != ScopeAgent {
		t.Fatal("minted token must work with agent scope")
	}
	if w := call("GET", "/v1/device/"+start.Poll); w.Code != 404 {
		t.Fatal("token must be handed out only once")
	}
	// Device endpoints are the only unauthenticated API paths.
	if w := call("GET", "/v1/apps"); w.Code != 401 {
		t.Fatalf("apps without token: %d", w.Code)
	}
}

func TestActionOutputAndMasking(t *testing.T) {
	act := apps.Catalog["umami"].Action("dashboard_login")
	out, err := checkOutput(act, map[string]any{"url": "https://x", "username": "admin", "password": "pw-123456", "extra": 1})
	if err != nil || out["extra"] != nil || out["password"] != "pw-123456" {
		t.Fatalf("output %v %v", out, err)
	}
	if _, err := checkOutput(act, map[string]any{"username": 5}); err == nil {
		t.Fatal("wrong type accepted")
	}
	m := maskSecrets(map[string]any{"a": "token sekrit-value-1 here", "b": []any{"sekrit-value-1"}}, []string{"sekrit-value-1"})
	if js(m) != `{"a":"token [secret] here","b":["[secret]"]}` {
		t.Fatalf("masked %s", js(m))
	}
	create := apps.Catalog["umami"].Action("create_website")
	if err := checkInput(create, map[string]string{"name": "x"}); err == nil {
		t.Fatal("missing required input accepted")
	}
	if err := checkInput(create, map[string]string{"name": "x", "domain": "a.b", "bogus": "1"}); err == nil {
		t.Fatal("unknown input accepted")
	}
	stats := apps.Catalog["umami"].Action("stats")
	if err := checkInput(stats, map[string]string{"hours": "abc"}); err == nil {
		t.Fatal("number input must be a number")
	}
}

func TestGenerateSecretFormats(t *testing.T) {
	h := generateSecret(apps.GenSecret{Bytes: 16, Format: "hex"})
	a := generateSecret(apps.GenSecret{Bytes: 24, Format: "alnum"})
	b := generateSecret(apps.GenSecret{Bytes: 32, Format: "base64"})
	if len(h) != 32 || len(a) != 24 || len(b) != 43 || h == generateSecret(apps.GenSecret{Bytes: 16, Format: "hex"}) {
		t.Fatalf("%q %q %q", h, a, b)
	}
	for _, c := range a {
		if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9') {
			t.Fatalf("alnum has %q", c)
		}
	}
}

func TestBackoff(t *testing.T) {
	if backoff(0) != 15*time.Second || backoff(2) != time.Minute || backoff(20) != 10*time.Minute {
		t.Fatal("backoff")
	}
}

func TestEnterPassesWallThenOpensPath(t *testing.T) {
	s := testServer()
	host := "vault.r1.test.slicedbread.dev"
	uri := apps.EnterPath + "?to=" + url.QueryEscape("/#/signup")
	w := httptest.NewRecorder()
	r := verifyReq(host, uri)
	r.Header.Set("Accept", "application/json") // even non-browser requests go to the login
	s.Handler().ServeHTTP(w, r)
	loc := w.Header().Get("Location")
	if w.Code != 302 || !strings.HasPrefix(loc, "https://auth.r1.test.slicedbread.dev/login?rd=") || !strings.Contains(loc, url.QueryEscape("https://"+host+uri)) {
		t.Fatalf("no session: %d %s", w.Code, loc)
	}
	cookie := &http.Cookie{Name: AppCookie, Value: s.auth.NewSession("me@x.dev", host, kindApp, time.Hour, time.Now())}
	w = httptest.NewRecorder()
	s.Handler().ServeHTTP(w, verifyReq(host, uri, cookie))
	if w.Code != 302 || w.Header().Get("Location") != "https://"+host+"/#/signup" {
		t.Fatalf("with session: %d %s", w.Code, w.Header().Get("Location"))
	}
	w = httptest.NewRecorder()
	s.Handler().ServeHTTP(w, verifyReq(host, apps.EnterPath+"?to="+url.QueryEscape("//evil.example"), cookie))
	if w.Header().Get("Location") != "https://"+host+"/" {
		t.Fatalf("open redirect: %s", w.Header().Get("Location"))
	}
	ex := s.exchangeURL("me@x.dev", "https://"+host+uri)
	if !strings.Contains(ex, "rd="+url.QueryEscape(uri)) {
		t.Fatalf("exchange must keep the enter query: %s", ex)
	}
}

func TestErrorLine(t *testing.T) {
	node := strings.Split(`node:internal/modules/run_main:107
    triggerUncaughtException(
    ^

[TypeError: fetch failed] {
  [cause]: Error: connect ECONNREFUSED 10.43.26.186:3000
      at TCPConnectWrap.afterConnect [as oncomplete] (node:net:2083:16) {
    errno: -111,
  }
}

Node.js v24.22.2`, "\n")
	if got := errorLine(node); !strings.Contains(got, "fetch failed") || !strings.Contains(got, "ECONNREFUSED") {
		t.Fatalf("got %q", got)
	}
	if got := errorLine([]string{"working", "umami login failed: HTTP 401"}); got != "umami login failed: HTTP 401" {
		t.Fatalf("got %q", got)
	}
}
