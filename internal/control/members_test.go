package control

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"
)

func TestInviteAcceptRolesAndRemoval(t *testing.T) {
	s := testServer()
	ctx := context.Background()
	now := time.Now()
	s.auth.st.Users = []user{{Email: "boss@x.dev", Hash: "h"}} // pre-role state: counts as admin
	if s.auth.Role("boss@x.dev") != RoleAdmin || !s.auth.CanApprove("boss@x.dev") {
		t.Fatal("existing users are admins and can approve their own agent")
	}
	tok, err := s.auth.Invite(ctx, "Mem@X.dev", RoleMember, now)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.auth.Invite(ctx, "boss@x.dev", RoleMember, now); err == nil {
		t.Fatal("cannot invite an existing member")
	}
	if _, err := s.auth.AcceptInvite(ctx, tok, "short", now); err == nil {
		t.Fatal("short password accepted")
	}
	email, err := s.auth.AcceptInvite(ctx, tok, "a long enough password", now)
	if err != nil || email != "mem@x.dev" {
		t.Fatalf("accept: %q %v", email, err)
	}
	if _, err := s.auth.AcceptInvite(ctx, tok, "a long enough password", now); err == nil {
		t.Fatal("invite reused")
	}
	if !s.auth.CheckPassword("mem@x.dev", "a long enough password") {
		t.Fatal("member cannot sign in")
	}
	if s.auth.CanApprove("mem@x.dev") {
		t.Fatal("members cannot approve by default")
	}
	s.auth.SetMembersCanApprove(ctx, true)
	if !s.auth.CanApprove("mem@x.dev") {
		t.Fatal("setting lets members approve")
	}
	if s.auth.CanApprove("stranger@x.dev") {
		t.Fatal("unknown users never approve")
	}
	if err := s.auth.SetRole(ctx, "boss@x.dev", RoleMember); err == nil {
		t.Fatal("last admin demoted")
	}
	if _, err := s.auth.RemoveMember(ctx, "boss@x.dev"); err == nil {
		t.Fatal("last admin removed")
	}
	// A removed member's session and agent tokens stop working at once.
	s.auth.st.Tokens = append(s.auth.st.Tokens, token{Name: "cli", Scope: ScopeAgent, SHA256: sha("sbt_x"), Owner: "mem@x.dev"})
	host := "crm.r1.test.slicedbread.dev"
	cookie := &http.Cookie{Name: AppCookie, Value: s.auth.NewSession("mem@x.dev", host, kindApp, time.Hour, now)}
	if w := verify(s, host, cookie); w.Code != 200 {
		t.Fatalf("member reaches the app: %d", w.Code)
	}
	n, err := s.auth.RemoveMember(ctx, "mem@x.dev")
	if err != nil || n != 1 {
		t.Fatalf("remove: %d %v", n, err)
	}
	if w := verify(s, host, cookie); w.Code == 200 {
		t.Fatal("removed member still passes the wall")
	}
	r := httptest.NewRequest("GET", "/", nil)
	r.Header.Set("Authorization", "Bearer sbt_x")
	if s.auth.TokenScope(r) != "" {
		t.Fatal("tokens the removed member approved still work")
	}
	if _, err := s.auth.Invite(ctx, "not-an-email", RoleMember, now); err == nil {
		t.Fatal("bad email accepted")
	}
	expired, _ := s.auth.Invite(ctx, "late@x.dev", RoleMember, now.Add(-8*24*time.Hour))
	if _, err := s.auth.AcceptInvite(ctx, expired, "a long enough password", now); err == nil {
		t.Fatal("expired invite accepted")
	}
}

func verify(s *Server, host string, c *http.Cookie) *httptest.ResponseRecorder {
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, verifyReq(host, "/", c))
	return w
}

func TestMemberCannotApproveInBrowser(t *testing.T) {
	s := testServer()
	ctx := context.Background()
	s.auth.st.Users = []user{{Email: "boss@x.dev", Role: RoleAdmin}, {Email: "mem@x.dev", Role: RoleMember}}
	auth := "auth.r1.test.slicedbread.dev"
	sess := &http.Cookie{Name: SessionCookie, Value: s.auth.NewSession("mem@x.dev", auth, kindAuth, time.Hour, time.Now())}
	r := httptest.NewRequest("POST", "https://"+auth+"/approvals/ap-x", strings.NewReader("decision=approve&csrf="+url.QueryEscape(s.sessionCSRF(sess.Value))))
	r.Host = auth
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	r.AddCookie(sess)
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, r)
	if !strings.Contains(w.Body.String(), "Only admins can approve") {
		t.Fatalf("member approval not refused: %d %s", w.Code, w.Body.String())
	}
	_ = ctx
}
