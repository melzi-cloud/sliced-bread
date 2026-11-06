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
	SessionCookie = "sb_session"
	csrfCookie    = "sb_csrf"
	sessionTTL    = 7 * 24 * time.Hour
	authSecret    = "sb-auth"
	bootSecret    = "sb-bootstrap"
)

type user struct {
	Email string `json:"email"`
	Hash  string `json:"hash"`
}

type token struct {
	Name   string `json:"name"`
	Scope  string `json:"scope"`
	SHA256 string `json:"sha256"`
}

type authState struct {
	SessionKey string  `json:"sessionKey"`
	Users      []user  `json:"users"`
	Tokens     []token `json:"tokens"`
}

// Auth holds users, API tokens and the session signing key.
type Auth struct {
	mu     sync.RWMutex
	st     authState
	key    []byte
	domain string
	kube   *kube.Client
	ns     string
}

// MintToken creates and persists a new API token, returning its plaintext once.
func (a *Auth) MintToken(ctx context.Context, name, scope string) (string, error) {
	tok := "sbt_" + randString(24)
	a.mu.Lock()
	a.st.Tokens = append(a.st.Tokens, token{Name: name, Scope: scope, SHA256: sha(tok)})
	a.mu.Unlock()
	if a.kube == nil {
		return tok, nil
	}
	a.mu.RLock()
	defer a.mu.RUnlock()
	return tok, a.save(ctx, a.kube, a.ns)
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
			Users:      []user{{Email: adminEmail, Hash: string(hash)}},
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
	Email string `json:"e"`
	Exp   int64  `json:"x"`
}

// NewSession returns a signed session cookie value.
func (a *Auth) NewSession(email string, now time.Time) string {
	p, _ := json.Marshal(sessionPayload{Email: email, Exp: now.Add(sessionTTL).Unix()})
	body := base64.RawURLEncoding.EncodeToString(p)
	mac := hmac.New(sha256.New, a.key)
	mac.Write([]byte(body))
	return body + "." + base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
}

// ParseSession validates a session cookie value.
func (a *Auth) ParseSession(v string, now time.Time) (string, error) {
	body, sig, ok := strings.Cut(v, ".")
	if !ok {
		return "", errors.New("malformed session")
	}
	mac := hmac.New(sha256.New, a.key)
	mac.Write([]byte(body))
	want := mac.Sum(nil)
	got, err := base64.RawURLEncoding.DecodeString(sig)
	if err != nil || !hmac.Equal(want, got) {
		return "", errors.New("bad signature")
	}
	raw, err := base64.RawURLEncoding.DecodeString(body)
	if err != nil {
		return "", err
	}
	var p sessionPayload
	if err := json.Unmarshal(raw, &p); err != nil {
		return "", err
	}
	if now.Unix() >= p.Exp {
		return "", errors.New("session expired")
	}
	return p.Email, nil
}

// SessionFromRequest returns the signed-in user, if any.
func (a *Auth) SessionFromRequest(r *http.Request) (string, bool) {
	c, err := r.Cookie(SessionCookie)
	if err != nil {
		return "", false
	}
	email, err := a.ParseSession(c.Value, time.Now())
	return email, err == nil
}

// FilteredCookieHeader returns the request's Cookie header without SlicedBread cookies.
func FilteredCookieHeader(r *http.Request) string {
	var keep []string
	for _, c := range r.Cookies() {
		if c.Name == SessionCookie || c.Name == csrfCookie {
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

// handleVerify is the forward-auth endpoint for wall apps.
func (s *Server) handleVerify(w http.ResponseWriter, r *http.Request) {
	if email, ok := s.auth.SessionFromRequest(r); ok {
		w.Header().Set("X-Sb-User", email)
		w.Header().Set("X-Sb-Email", email)
		if c := FilteredCookieHeader(r); c != "" {
			w.Header().Set("Cookie", c)
		}
		w.WriteHeader(http.StatusOK)
		return
	}
	proto := r.Header.Get("X-Forwarded-Proto")
	if proto == "" {
		proto = "https"
	}
	orig := proto + "://" + r.Header.Get("X-Forwarded-Host") + r.Header.Get("X-Forwarded-Uri")
	method := r.Header.Get("X-Forwarded-Method")
	if (method == "" || method == http.MethodGet) && strings.Contains(r.Header.Get("Accept"), "text/html") {
		http.Redirect(w, r, "https://auth."+s.cfg.Domain+"/login?rd="+url.QueryEscape(orig), http.StatusFound)
		return
	}
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.WriteHeader(http.StatusUnauthorized)
	w.Write([]byte("SlicedBread login required: https://auth." + s.cfg.Domain + "/login\n"))
}

// handleStrip is the forward-auth endpoint for non-wall apps. It never blocks;
// it only removes SlicedBread cookies so apps cannot read sessions.
func (s *Server) handleStrip(w http.ResponseWriter, r *http.Request) {
	if c := FilteredCookieHeader(r); c != "" {
		w.Header().Set("Cookie", c)
	}
	w.WriteHeader(http.StatusOK)
}

func (s *Server) setCookie(w http.ResponseWriter, name, value string, maxAge int, domainWide bool) {
	c := &http.Cookie{Name: name, Value: value, Path: "/", MaxAge: maxAge, Secure: true, HttpOnly: true, SameSite: http.SameSiteLaxMode}
	if domainWide {
		c.Domain = s.cfg.Domain
	}
	http.SetCookie(w, c)
}

// csrf returns the CSRF token for the request, setting a cookie if needed.
func (s *Server) csrf(w http.ResponseWriter, r *http.Request) string {
	if c, err := r.Cookie(csrfCookie); err == nil && len(c.Value) >= 20 {
		return c.Value
	}
	v := randString(24)
	s.setCookie(w, csrfCookie, v, 3600*12, false)
	return v
}

func (s *Server) checkCSRF(r *http.Request) bool {
	c, err := r.Cookie(csrfCookie)
	if err != nil {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(c.Value), []byte(r.PostFormValue("csrf"))) == 1
}
