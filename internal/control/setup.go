package control

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"net/http"
	"net/mail"
	"strconv"
	"strings"
	"sync"
	"time"

	"golang.org/x/crypto/bcrypt"

	"github.com/melzi-cloud/sliced-bread/internal/acme"
	"github.com/melzi-cloud/sliced-bread/internal/apps"
	"github.com/melzi-cloud/sliced-bread/internal/kube"
)

// First login. sb init installs a one-time setup code (secret sb-setup: its sha256 and
// an expiry) and prints the link. Opening it lets the person who ran sb init create the
// owner account. Then the code is deleted and the setup page is gone.

const (
	setupSecret       = "sb-setup"
	certNamespace     = "kube-system"
	certName          = "tenant-wildcard"
	primaryIssuer     = "letsencrypt"
	fallbackIssuer    = "fallback"
	fallbackSwitchAge = 30 * time.Minute
	renewalDeadline   = 20 * 24 * time.Hour
)

// HasUsers reports whether the owner account exists.
func (a *Auth) HasUsers() bool {
	a.mu.RLock()
	defer a.mu.RUnlock()
	return len(a.st.Users) > 0
}

// CreateOwner creates the first admin. It fails once any user exists.
func (a *Auth) CreateOwner(ctx context.Context, email, password string) error {
	email = normEmail(email)
	if _, err := mail.ParseAddress(email); err != nil || !strings.Contains(email, "@") {
		return errors.New("enter a valid email address")
	}
	if len(password) < minPasswordLen {
		return errors.New("use at least 12 characters")
	}
	hash, err := bcrypt.GenerateFromPassword([]byte(password), bcrypt.DefaultCost)
	if err != nil {
		return err
	}
	return a.mutate(ctx, func(st *authState) error {
		if len(st.Users) > 0 {
			return errors.New("this install is already set up; sign in instead")
		}
		st.Users = []user{{Email: email, Hash: string(hash), Role: RoleAdmin}}
		return nil
	})
}

// OwnerEmail returns the first admin's email.
func (a *Auth) OwnerEmail() string {
	a.mu.RLock()
	defer a.mu.RUnlock()
	for _, u := range a.st.Users {
		if roleOf(u) == RoleAdmin {
			return u.Email
		}
	}
	return ""
}

// setupCodeValid checks a code against the installed setup secret.
func (s *Server) setupCodeValid(ctx context.Context, code string) bool {
	if code == "" || s.auth.HasUsers() {
		return false
	}
	var sec kube.Obj
	if err := s.kube.Get(ctx, "v1", "Secret", s.cfg.Namespace, setupSecret, &sec); err != nil {
		return false
	}
	d := func(k string) string {
		b, _ := base64.StdEncoding.DecodeString(kube.Str(sec, "data", k))
		return string(b)
	}
	exp, _ := strconv.ParseInt(d("expires"), 10, 64)
	if time.Now().Unix() >= exp {
		return false
	}
	sum := sha256.Sum256([]byte(strings.ToUpper(strings.TrimSpace(code))))
	return subtle.ConstantTimeCompare([]byte(hex.EncodeToString(sum[:])), []byte(d("sha256"))) == 1
}

const setupInvalid = "This setup link is not valid: it expired, was already used, or the install is set up. On the machine that ran sb init, run: sb setup-link <install>"

func (s *Server) handleSetupGet(w http.ResponseWriter, r *http.Request) {
	code := r.URL.Query().Get("code")
	if !s.setupCodeValid(r.Context(), code) {
		s.render(w, pageData{Page: "login", Error: setupInvalid, CSRF: s.csrf(w, r)})
		return
	}
	s.render(w, pageData{Page: "setup", Code: code, CSRF: s.csrf(w, r)})
}

func (s *Server) handleSetupPost(w http.ResponseWriter, r *http.Request) {
	code := r.PostFormValue("code")
	d := pageData{Page: "setup", Code: code, CSRF: s.csrf(w, r), Email: r.PostFormValue("email")}
	switch {
	case !s.checkCSRF(r):
		d.Error = "Form expired, try again."
	case !s.setupCodeValid(r.Context(), code):
		d.Page, d.Error, d.Email = "login", setupInvalid, ""
	case r.PostFormValue("password") != r.PostFormValue("password2"):
		d.Error = "The passwords do not match."
	}
	if d.Error != "" {
		s.render(w, d)
		return
	}
	email := r.PostFormValue("email")
	if err := s.auth.CreateOwner(r.Context(), email, r.PostFormValue("password")); err != nil {
		d.Error = err.Error()
		s.render(w, d)
		return
	}
	if err := s.kube.Delete(r.Context(), "v1", "Secret", s.cfg.Namespace, setupSecret); err != nil && !kube.IsNotFound(err) {
		s.logf("setup: delete setup code: %v", err)
	}
	s.logf("setup: owner account created")
	// The owner's email registers the ZeroSSL fallback issuer.
	go func(email string) {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
		defer cancel()
		s.enableFallbackIssuer(ctx, normEmail(email))
	}(email)
	s.setCookie(w, SessionCookie, s.auth.NewSession(normEmail(email), requestHost(r), kindAuth, sessionTTL, time.Now()), int(sessionTTL.Seconds()))
	http.Redirect(w, r, "/settings?welcome=1", http.StatusSeeOther)
}

// --- certificate fallback ---

type fallbackState struct {
	mu     sync.Mutex
	status string
}

func (f *fallbackState) set(s string) { f.mu.Lock(); f.status = s; f.mu.Unlock() }
func (f *fallbackState) get() string  { f.mu.Lock(); defer f.mu.Unlock(); return f.status }

// issuerReady returns whether a ClusterIssuer exists and is Ready.
func (s *Server) issuerReady(ctx context.Context, name string) (exists, ready bool, msg string) {
	var iss kube.Obj
	if err := s.kube.Get(ctx, "cert-manager.io/v1", "ClusterIssuer", "", name, &iss); err != nil {
		return false, false, ""
	}
	st, m := conditionStatus(iss, "Ready")
	return true, st == "True", m
}

// enableFallbackIssuer registers a ZeroSSL account with the owner's email and creates the
// "fallback" ClusterIssuer with the same DNS solver as the primary issuer.
func (s *Server) enableFallbackIssuer(ctx context.Context, email string) error {
	if exists, _, _ := s.issuerReady(ctx, fallbackIssuer); exists {
		s.fallback.set("ZeroSSL fallback is configured")
		return nil
	}
	var primary kube.Obj
	if err := s.kube.Get(ctx, "cert-manager.io/v1", "ClusterIssuer", "", primaryIssuer, &primary); err != nil {
		s.fallback.set("could not read the primary issuer: " + err.Error())
		return err
	}
	acmeSpec, _ := primary["spec"].(map[string]any)["acme"].(map[string]any)
	solvers, _ := acmeSpec["solvers"].([]any)
	if len(solvers) == 0 {
		err := errors.New("the primary issuer has no DNS solver to copy")
		s.fallback.set(err.Error())
		return err
	}
	kid, hmac, err := acme.ZeroSSLEAB(ctx, email)
	if err != nil {
		s.fallback.set("ZeroSSL did not issue account credentials: " + err.Error())
		s.logf("fallback issuer: %v", err)
		return err
	}
	if err := s.kube.Apply(ctx, kube.Obj{
		"apiVersion": "v1", "kind": "Secret",
		"metadata": kube.Obj{"name": "fallback-eab", "namespace": "cert-manager", "labels": kube.Obj{apps.ManagedByLabel: "slicedbread"}},
		"type":     "Opaque", "stringData": kube.Obj{"hmac": hmac},
	}); err != nil {
		s.fallback.set("could not store the ZeroSSL key: " + err.Error())
		return err
	}
	if err := s.kube.Apply(ctx, kube.Obj{
		"apiVersion": "cert-manager.io/v1", "kind": "ClusterIssuer",
		"metadata": kube.Obj{"name": fallbackIssuer, "labels": kube.Obj{apps.ManagedByLabel: "slicedbread"}},
		"spec": kube.Obj{"acme": kube.Obj{
			"server": acme.ZeroSSLDirectory, "email": email,
			"externalAccountBinding": kube.Obj{"keyID": kid, "keySecretRef": kube.Obj{"name": "fallback-eab", "key": "hmac"}},
			"privateKeySecretRef":    kube.Obj{"name": fallbackIssuer + "-account"},
			"solvers":                solvers,
		}},
	}); err != nil {
		s.fallback.set("could not create the fallback issuer: " + err.Error())
		return err
	}
	s.fallback.set("ZeroSSL fallback configured for " + email)
	s.logf("fallback issuer: ZeroSSL configured")
	return nil
}

// switchDecision says whether the wildcard certificate should move to the fallback issuer:
// it has not been Ready for a while, or it is close to expiry without renewing.
func switchDecision(issuer, ready string, readySince, notAfter, now time.Time) bool {
	if issuer != primaryIssuer {
		return false
	}
	if ready != "True" && !readySince.IsZero() && now.Sub(readySince) > fallbackSwitchAge {
		return true
	}
	return !notAfter.IsZero() && notAfter.Sub(now) < renewalDeadline
}

// maybeSwitchIssuer moves the wildcard certificate to ZeroSSL when Let's Encrypt fails.
func (s *Server) maybeSwitchIssuer(ctx context.Context) {
	var c kube.Obj
	if err := s.kube.Get(ctx, "cert-manager.io/v1", "Certificate", certNamespace, certName, &c); err != nil {
		return
	}
	issuer := kube.Str(c, "spec", "issuerRef", "name")
	ready, _ := conditionStatus(c, "Ready")
	var since time.Time
	conds, _ := statusMap(c)["conditions"].([]any)
	for _, x := range conds {
		m, _ := x.(map[string]any)
		if t, _ := m["type"].(string); t == "Ready" {
			since, _ = time.Parse(time.RFC3339, kube.Str(m, "lastTransitionTime"))
		}
	}
	notAfter, _ := time.Parse(time.RFC3339, kube.Str(c, "status", "notAfter"))
	if !switchDecision(issuer, ready, since, notAfter, time.Now()) {
		return
	}
	if _, ok, _ := s.issuerReady(ctx, fallbackIssuer); !ok {
		return
	}
	if err := s.kube.MergePatch(ctx, "cert-manager.io/v1", "Certificate", certNamespace, certName,
		map[string]any{"spec": map[string]any{"issuerRef": map[string]any{"name": fallbackIssuer}}}); err != nil {
		s.logf("switch certificate to fallback: %v", err)
		return
	}
	s.logf("certificate %s/%s: Let's Encrypt did not issue or renew; switched to the ZeroSSL fallback", certNamespace, certName)
}
