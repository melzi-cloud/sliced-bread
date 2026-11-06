package control

import (
	"context"
	"net/http"
	"strings"
	"sync"
	"time"
)

// deviceGrant is a pending browser login for the CLI (device code flow).
type deviceGrant struct {
	Code      string // shown to the human, e.g. KX7P-2QMD
	Poll      string // secret the CLI polls with
	Expires   time.Time
	Token     string // set once approved; handed out once
	Denied    bool
	Requester string // free-text label from the CLI, e.g. hostname
}

type deviceStore struct {
	mu     sync.Mutex
	grants map[string]*deviceGrant // by poll secret
}

func (d *deviceStore) byCode(code string) *deviceGrant {
	for _, g := range d.grants {
		if g.Code == code && time.Now().Before(g.Expires) {
			return g
		}
	}
	return nil
}

func (d *deviceStore) gc() {
	for k, g := range d.grants {
		if time.Now().After(g.Expires) {
			delete(d.grants, k)
		}
	}
}

func userCode() string {
	const alphabet = "ABCDEFGHJKLMNPQRSTUVWXYZ23456789"
	b := []byte(randID(8))
	for i := range b {
		b[i] = alphabet[int(b[i])%len(alphabet)]
	}
	return string(b[:4]) + "-" + string(b[4:])
}

func (s *Server) apiDeviceStart(w http.ResponseWriter, r *http.Request) {
	s.devices.mu.Lock()
	defer s.devices.mu.Unlock()
	s.devices.gc()
	if len(s.devices.grants) >= 50 {
		fail(w, 429, "too many pending logins", "try again in a few minutes")
		return
	}
	label := strings.TrimSpace(r.URL.Query().Get("label"))
	if len(label) > 60 {
		label = label[:60]
	}
	g := &deviceGrant{Code: userCode(), Poll: randString(24), Expires: time.Now().Add(10 * time.Minute), Requester: label}
	s.devices.grants[g.Poll] = g
	writeJSON(w, 201, map[string]any{
		"code": g.Code, "poll": g.Poll, "expiresIn": 600,
		"verifyUrl": "https://auth." + s.cfg.Domain + "/device?code=" + g.Code,
	})
}

func (s *Server) apiDevicePoll(w http.ResponseWriter, r *http.Request) {
	s.devices.mu.Lock()
	defer s.devices.mu.Unlock()
	g := s.devices.grants[r.PathValue("poll")]
	switch {
	case g == nil || time.Now().After(g.Expires):
		fail(w, 404, "login request expired or unknown", "run sb login again")
	case g.Denied:
		delete(s.devices.grants, g.Poll)
		fail(w, 403, "login was denied in the browser", "")
	case g.Token == "":
		writeJSON(w, 202, map[string]any{"state": "pending"})
	default:
		tok := g.Token
		delete(s.devices.grants, g.Poll)
		writeJSON(w, 200, map[string]any{"state": "approved", "token": tok, "scope": ScopeAgent, "domain": s.cfg.Domain})
	}
}

// approveDevice mints an agent token for a device grant after a human approved it in the browser.
func (s *Server) approveDevice(ctx context.Context, code, approver string, allow bool) error {
	s.devices.mu.Lock()
	g := s.devices.byCode(code)
	s.devices.mu.Unlock()
	if g == nil {
		return errExpired
	}
	if !allow {
		s.devices.mu.Lock()
		g.Denied = true
		s.devices.mu.Unlock()
		return nil
	}
	tok, err := s.auth.MintToken(ctx, "cli "+code+" approved by "+approver, ScopeAgent)
	if err != nil {
		return err
	}
	s.devices.mu.Lock()
	g.Token = tok
	s.devices.mu.Unlock()
	return nil
}

type expiredErr struct{}

func (expiredErr) Error() string { return "this login code expired; run sb login again" }

var errExpired error = expiredErr{}
