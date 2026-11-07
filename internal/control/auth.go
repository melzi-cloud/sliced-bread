package control

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"golang.org/x/crypto/bcrypt"

	"github.com/melzi-cloud/sliced-bread/internal/kube"
)

// Token scopes.
const (
	ScopeAdmin = "admin" // a human: may approve, delete, change exposure
	ScopeAgent = "agent" // an LLM agent: exposure changes and deletes need approval
)

const (
	// All SlicedBread cookies are host-only with the __Host- prefix: browsers refuse to let
	// any other host, including a sibling subdomain, set or overwrite them.
	SessionCookie = "__Host-sb_auth" // signed-in session on the auth host
	AppCookie     = "__Host-sb_s"    // per-app-host session, minted by the exchange
	csrfCookie    = "__Host-sb_csrf" // only for the login form, before a session exists
	sessionTTL    = 7 * 24 * time.Hour
	appSessionTTL = 12 * time.Hour
	exchangeTTL   = 60 * time.Second
	exchangePath  = "/__sb/exchange"
	authSecret    = "sb-auth"
	bootSecret    = "sb-bootstrap"
)

type user struct {
	Email string `json:"email"`
	Hash  string `json:"hash"`
	Role  string `json:"role,omitempty"` // admin or member; empty means admin (older state)
}

type token struct {
	Name   string `json:"name"`
	Scope  string `json:"scope"`
	SHA256 string `json:"sha256"`
	Owner  string `json:"owner,omitempty"` // email of the human who approved it
}

type authState struct {
	SessionKey        string   `json:"sessionKey"`
	Users             []user   `json:"users"`
	Tokens            []token  `json:"tokens"`
	Invites           []invite `json:"invites,omitempty"`
	MembersCanApprove bool     `json:"membersCanApprove,omitempty"`
}

// Auth holds users, API tokens and the session signing key.
type Auth struct {
	mu     sync.RWMutex
	st     authState
	key    []byte
	domain string
	kube   *kube.Client
	ns     string
	used   map[string]int64 // redeemed exchange nonces
}

// MintToken creates and persists a new API token, returning its plaintext once.
func (a *Auth) MintToken(ctx context.Context, name, scope, owner string) (string, error) {
	tok := "sbt_" + randString(24)
	return tok, a.mutate(ctx, func(st *authState) error {
		st.Tokens = append(st.Tokens, token{Name: name, Scope: scope, SHA256: sha(tok), Owner: owner})
		return nil
	})
}

func randString(n int) string {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	return base64.RawURLEncoding.EncodeToString(b)
}

func sha(s string) string {
	h := sha256.Sum256([]byte(s))
	return hex.EncodeToString(h[:])
}

// LoadAuth reads auth state from the sb-auth secret, creating it with an admin
// user and two API tokens on first start. Initial credentials go to the
// sb-bootstrap secret so the operator can fetch them once.
func LoadAuth(ctx context.Context, k *kube.Client, ns, domain, adminEmail string) (*Auth, error) {
	a := &Auth{domain: domain, kube: k, ns: ns}
	var sec kube.Obj
	err := k.Get(ctx, "v1", "Secret", ns, authSecret, &sec)
	if err == nil {
		raw, _ := base64.StdEncoding.DecodeString(kube.Str(sec, "data", "state.json"))
		if err := json.Unmarshal(raw, &a.st); err != nil {
			return nil, err
		}
	} else if kube.IsNotFound(err) {
		password := randString(15)
		adminTok, agentTok := "sbt_"+randString(24), "sbt_"+randString(24)
		hash, err := bcrypt.GenerateFromPassword([]byte(password), bcrypt.DefaultCost)
		if err != nil {
			return nil, err
		}
		a.st = authState{
			SessionKey: randString(32),
			Users:      []user{{Email: adminEmail, Hash: string(hash), Role: RoleAdmin}},
			Tokens:     []token{{Name: "admin", Scope: ScopeAdmin, SHA256: sha(adminTok)}, {Name: "agent", Scope: ScopeAgent, SHA256: sha(agentTok)}},
		}
		if err := a.save(ctx, k, ns); err != nil {
			return nil, err
		}
		boot := kube.Obj{
			"apiVersion": "v1", "kind": "Secret",
			"metadata": kube.Obj{"name": bootSecret, "namespace": ns},
			"type":     "Opaque",
			"stringData": kube.Obj{
				"admin-email": adminEmail, "admin-password": password,
				"admin-token": adminTok, "agent-token": agentTok,
			},
		}
		if err := k.Apply(ctx, boot); err != nil {
			return nil, err
		}
	} else {
		return nil, err
	}
	a.key = []byte(a.st.SessionKey)
	return a, nil
}

func (a *Auth) save(ctx context.Context, k *kube.Client, ns string) error {
	b, err := json.Marshal(a.st)
	if err != nil {
		return err
	}
	return k.Apply(ctx, kube.Obj{
		"apiVersion": "v1", "kind": "Secret",
		"metadata": kube.Obj{"name": authSecret, "namespace": ns},
		"type":     "Opaque",
		"data":     kube.Obj{"state.json": base64.StdEncoding.EncodeToString(b)},
	})
}

// Emails lists the team members the login wall admits.
func (a *Auth) Emails() []string {
	a.mu.RLock()
	defer a.mu.RUnlock()
	out := []string{}
	for _, u := range a.st.Users {
		out = append(out, u.Email)
	}
	return out
}

// TokenScope returns the scope of a bearer token, or "" if invalid.
func (a *Auth) TokenScope(r *http.Request) string {
	h := r.Header.Get("Authorization")
	tok, ok := strings.CutPrefix(h, "Bearer ")
	if !ok || tok == "" {
		return ""
	}
	sum := sha(strings.TrimSpace(tok))
	a.mu.RLock()
	defer a.mu.RUnlock()
	for _, t := range a.st.Tokens {
		if subtle.ConstantTimeCompare([]byte(t.SHA256), []byte(sum)) == 1 {
			return t.Scope
		}
	}
	return ""
}

// CheckPassword verifies a user's password.
func (a *Auth) CheckPassword(email, password string) bool {
	a.mu.RLock()
	defer a.mu.RUnlock()
	for _, u := range a.st.Users {
		if strings.EqualFold(u.Email, email) {
			return bcrypt.CompareHashAndPassword([]byte(u.Hash), []byte(password)) == nil
		}
	}
	// Spend similar time for unknown users.
	_ = bcrypt.CompareHashAndPassword([]byte("$2a$10$7EqJtq98hPqEX7fNZaFWoO.HwGj9lSS7f6E9VQx8V9yjUpZ2a0ZQe"), []byte(password))
	return false
}

type sessionPayload struct {
	Email  string `json:"e"`
	Exp    int64  `json:"x"`
	Domain string `json:"d"` // install domain: a session from another install never validates
	Host   string `json:"h"` // host the token is for
	Kind   string `json:"k"` // auth, app or exchange
	Nonce  string `json:"n,omitempty"`
}

const (
	kindAuth     = "auth"
	kindApp      = "app"
	kindExchange = "exchange"
)

func (a *Auth) sign(p sessionPayload) string {
	b, _ := json.Marshal(p)
	body := base64.RawURLEncoding.EncodeToString(b)
	mac := hmac.New(sha256.New, a.key)
	mac.Write([]byte(body))
	return body + "." + base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
}

func (a *Auth) verify(v, host, kind string, now time.Time) (sessionPayload, error) {
	var p sessionPayload
	body, sig, ok := strings.Cut(v, ".")
	if !ok {
		return p, errors.New("malformed token")
	}
	mac := hmac.New(sha256.New, a.key)
	mac.Write([]byte(body))
	got, err := base64.RawURLEncoding.DecodeString(sig)
	if err != nil || !hmac.Equal(mac.Sum(nil), got) {
		return p, errors.New("bad signature")
	}
	raw, err := base64.RawURLEncoding.DecodeString(body)
	if err != nil || json.Unmarshal(raw, &p) != nil {
		return p, errors.New("malformed token")
	}
	switch {
	case p.Domain != a.domain:
		return p, errors.New("token is for another install")
	case p.Kind != kind:
		return p, errors.New("wrong token kind")
	case !strings.EqualFold(p.Host, host):
		return p, errors.New("token is for another host")
	case now.Unix() >= p.Exp:
		return p, errors.New("token expired")
	}
	return p, nil
}

// NewSession returns a signed session for a host.
func (a *Auth) NewSession(email, host, kind string, ttl time.Duration, now time.Time) string {
	return a.sign(sessionPayload{Email: email, Exp: now.Add(ttl).Unix(), Domain: a.domain, Host: strings.ToLower(host), Kind: kind})
}

// ParseSession validates a session for a host and returns the email.
func (a *Auth) ParseSession(v, host, kind string, now time.Time) (string, error) {
	p, err := a.verify(v, host, kind, now)
	return p.Email, err
}

// NewExchange returns a one-time token that the app host trades for its own session.
func (a *Auth) NewExchange(email, host string, now time.Time) string {
	return a.sign(sessionPayload{Email: email, Exp: now.Add(exchangeTTL).Unix(), Domain: a.domain, Host: strings.ToLower(host), Kind: kindExchange, Nonce: randString(12)})
}

// RedeemExchange validates an exchange token once.
func (a *Auth) RedeemExchange(v, host string, now time.Time) (string, error) {
	p, err := a.verify(v, host, kindExchange, now)
	if err != nil {
		return "", err
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.used == nil {
		a.used = map[string]int64{}
	}
	for n, exp := range a.used {
		if now.Unix() > exp {
			delete(a.used, n)
		}
	}
	if _, seen := a.used[p.Nonce]; seen {
		return "", errors.New("token already used")
	}
	a.used[p.Nonce] = p.Exp
	return p.Email, nil
}

// SessionFromRequest returns the signed-in user on the auth host, if any.
func (a *Auth) SessionFromRequest(r *http.Request) (string, bool) {
	email, _, ok := a.findSession(r, SessionCookie, requestHost(r), kindAuth)
	return email, ok
}

// findSession checks every cookie with the given name, so a planted duplicate cannot shadow a real one.
func (a *Auth) findSession(r *http.Request, name, host, kind string) (email, value string, ok bool) {
	now := time.Now()
	for _, c := range r.Cookies() {
		if c.Name != name {
			continue
		}
		if e, err := a.ParseSession(c.Value, host, kind, now); err == nil && a.hasUser(e) {
			return e, c.Value, true
		}
	}
	return "", "", false
}

func requestHost(r *http.Request) string {
	h := r.Host
	if i := strings.LastIndex(h, ":"); i > 0 && !strings.Contains(h[i:], "]") {
		h = h[:i]
	}
	return strings.ToLower(h)
}

// ourCookie reports cookie names SlicedBread uses or used, including look-alikes set by other hosts.
func ourCookie(name string) bool {
	return strings.HasPrefix(name, "__Host-sb_") || name == "sb_session" || name == "sb_csrf" || strings.HasPrefix(name, "sb_s_")
}

// FilteredCookieHeader returns the request's Cookie header without SlicedBread cookies.
func FilteredCookieHeader(r *http.Request) string {
	var keep []string
	for _, c := range r.Cookies() {
		if ourCookie(c.Name) {
			continue
		}
		keep = append(keep, c.Name+"="+c.Value)
	}
	return strings.Join(keep, "; ")
}

// SafeRedirect reports whether rd is an https URL inside the tenant domain.
func SafeRedirect(rd, domain string) bool {
	u, err := url.Parse(rd)
	if err != nil || u.Scheme != "https" || u.User != nil {
		return false
	}
	h := strings.ToLower(u.Hostname())
	return h == domain || strings.HasSuffix(h, "."+domain)
}

// safeLocalPath reports whether p is a path on the same host (no scheme, no //host).
func safeLocalPath(p string) bool {
	return strings.HasPrefix(p, "/") && !strings.HasPrefix(p, "//") && !strings.Contains(p, "\\") && !strings.ContainsAny(p, "\r\n")
}

// exchangeURL is where the auth host sends a signed-in browser to get a session for rd's host.
func (s *Server) exchangeURL(email, rd string) string {
	u, err := url.Parse(rd)
	if err != nil {
		return "/"
	}
	path := u.EscapedPath()
	if path == "" {
		path = "/"
	}
	if u.RawQuery != "" {
		path += "?" + u.RawQuery
	}
	tok := s.auth.NewExchange(email, u.Hostname(), time.Now())
	return "https://" + u.Host + exchangePath + "?token=" + url.QueryEscape(tok) + "&rd=" + url.QueryEscape(path)
}

// handleVerify is the forward-auth endpoint for wall apps. Each app host has its own
// session cookie; the auth host hands it over through a one-time exchange token.
func (s *Server) handleVerify(w http.ResponseWriter, r *http.Request) {
	host := strings.ToLower(r.Header.Get("X-Forwarded-Host"))
	uri := r.Header.Get("X-Forwarded-Uri")
	if strings.HasPrefix(uri, exchangePath) {
		s.handleExchange(w, host, uri)
		return
	}
	if email, _, ok := s.auth.findSession(r, AppCookie, host, kindApp); ok {
		w.Header().Set("X-Sb-User", email)
		w.Header().Set("X-Sb-Email", email)
		if c := FilteredCookieHeader(r); c != "" {
			w.Header().Set("Cookie", c)
		}
		w.WriteHeader(http.StatusOK)
		return
	}
	orig := "https://" + host + uri
	method := r.Header.Get("X-Forwarded-Method")
	if (method == "" || method == http.MethodGet) && strings.Contains(r.Header.Get("Accept"), "text/html") {
		http.Redirect(w, r, "https://"+s.hosts().Auth()+"/login?rd="+url.QueryEscape(orig), http.StatusFound)
		return
	}
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.WriteHeader(http.StatusUnauthorized)
	w.Write([]byte("SlicedBread login required: https://" + s.hosts().Auth() + "/login\n"))
}

// handleExchange trades a one-time token for a host-only session on the app host.
func (s *Server) handleExchange(w http.ResponseWriter, host, uri string) {
	u, err := url.Parse(uri)
	if err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	email, err := s.auth.RedeemExchange(u.Query().Get("token"), host, time.Now())
	if err != nil {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		w.WriteHeader(http.StatusForbidden)
		w.Write([]byte("sign-in link invalid (" + err.Error() + "); open the app again to sign in\n"))
		return
	}
	rd := u.Query().Get("rd")
	if !safeLocalPath(rd) {
		rd = "/"
	}
	http.SetCookie(w, &http.Cookie{Name: AppCookie, Value: s.auth.NewSession(email, host, kindApp, appSessionTTL, time.Now()),
		Path: "/", MaxAge: int(appSessionTTL.Seconds()), Secure: true, HttpOnly: true, SameSite: http.SameSiteLaxMode})
	// Absolute: Traefik resolves a relative Location against the auth service's own address.
	w.Header().Set("Location", "https://"+host+rd)
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(http.StatusFound)
}

// handleStrip is the forward-auth endpoint for non-wall apps. It never blocks;
// it only removes SlicedBread cookies so apps cannot read sessions.
func (s *Server) handleStrip(w http.ResponseWriter, r *http.Request) {
	if c := FilteredCookieHeader(r); c != "" {
		w.Header().Set("Cookie", c)
	}
	w.WriteHeader(http.StatusOK)
}

func (s *Server) setCookie(w http.ResponseWriter, name, value string, maxAge int) {
	http.SetCookie(w, &http.Cookie{Name: name, Value: value, Path: "/", MaxAge: maxAge, Secure: true, HttpOnly: true, SameSite: http.SameSiteLaxMode})
}

// csrf returns the CSRF token for a form. Signed-in pages use a token derived from the
// session, so no separate cookie can be shadowed; the login form uses a __Host- cookie.
func (s *Server) csrf(w http.ResponseWriter, r *http.Request) string {
	if _, v, ok := s.auth.findSession(r, SessionCookie, requestHost(r), kindAuth); ok {
		return s.sessionCSRF(v)
	}
	if c, err := r.Cookie(csrfCookie); err == nil && len(c.Value) >= 20 {
		return c.Value
	}
	v := randString(24)
	s.setCookie(w, csrfCookie, v, 3600*12)
	return v
}

func (s *Server) sessionCSRF(session string) string {
	m := hmac.New(sha256.New, append([]byte("csrf:"), s.auth.key...))
	m.Write([]byte(session))
	return hex.EncodeToString(m.Sum(nil))
}

func (s *Server) checkCSRF(r *http.Request) bool {
	got := r.PostFormValue("csrf")
	if _, v, ok := s.auth.findSession(r, SessionCookie, requestHost(r), kindAuth); ok {
		return subtle.ConstantTimeCompare([]byte(s.sessionCSRF(v)), []byte(got)) == 1
	}
	c, err := r.Cookie(csrfCookie)
	if err != nil {
		return false
	}
	return got != "" && subtle.ConstantTimeCompare([]byte(c.Value), []byte(got)) == 1
}
