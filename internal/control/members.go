package control

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"net/http"
	"net/mail"
	"sort"
	"strings"
	"time"

	"golang.org/x/crypto/bcrypt"
)

// Team members. Admins manage members and settings and can always approve risky
// changes. Members get past the login wall; whether they may approve is an install-wide
// setting only admins change (default off). A solo admin approves their own agent.

const (
	RoleAdmin       = "admin"
	RoleMember      = "member"
	inviteTTL       = 7 * 24 * time.Hour
	minPasswordLen  = 12
	invitePath      = "/invite"
	errLastAdminMsg = "an install needs at least one admin"
)

type invite struct {
	SHA256  string `json:"sha256"`
	Email   string `json:"email"`
	Role    string `json:"role"`
	Expires int64  `json:"expires"`
}

// Member is what the API and dashboard show.
type Member struct {
	Email string `json:"email"`
	Role  string `json:"role"`
}

// PendingInvite is an invite not yet accepted.
type PendingInvite struct {
	Email   string `json:"email"`
	Role    string `json:"role"`
	Expires string `json:"expires"`
}

func roleOf(u user) string {
	if u.Role == "" {
		return RoleAdmin // users from before roles existed were the bootstrap admin
	}
	return u.Role
}

func normEmail(e string) string { return strings.ToLower(strings.TrimSpace(e)) }

// mutate applies a change to the auth state and persists it.
func (a *Auth) mutate(ctx context.Context, f func(st *authState) error) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	before, _ := json.Marshal(a.st)
	if err := f(&a.st); err != nil {
		return err
	}
	if a.kube == nil {
		return nil
	}
	if err := a.save(ctx, a.kube, a.ns); err != nil {
		json.Unmarshal(before, &a.st) // keep memory and storage in step
		return err
	}
	return nil
}

func (a *Auth) hasUser(email string) bool {
	a.mu.RLock()
	defer a.mu.RUnlock()
	for _, u := range a.st.Users {
		if strings.EqualFold(u.Email, email) {
			return true
		}
	}
	return false
}

// Role returns a user's role, or "" if there is no such user.
func (a *Auth) Role(email string) string {
	a.mu.RLock()
	defer a.mu.RUnlock()
	for _, u := range a.st.Users {
		if strings.EqualFold(u.Email, email) {
			return roleOf(u)
		}
	}
	return ""
}

// CanApprove reports whether a signed-in user may approve risky changes.
func (a *Auth) CanApprove(email string) bool {
	switch a.Role(email) {
	case RoleAdmin:
		return true
	case RoleMember:
		a.mu.RLock()
		defer a.mu.RUnlock()
		return a.st.MembersCanApprove
	}
	return false
}

// Members lists users and pending invites.
func (a *Auth) Members(now time.Time) ([]Member, []PendingInvite, bool) {
	a.mu.RLock()
	defer a.mu.RUnlock()
	var ms []Member
	for _, u := range a.st.Users {
		ms = append(ms, Member{Email: u.Email, Role: roleOf(u)})
	}
	sort.Slice(ms, func(i, j int) bool { return ms[i].Email < ms[j].Email })
	var inv []PendingInvite
	for _, i := range a.st.Invites {
		if now.Unix() < i.Expires {
			inv = append(inv, PendingInvite{Email: i.Email, Role: i.Role, Expires: time.Unix(i.Expires, 0).UTC().Format(time.RFC3339)})
		}
	}
	return ms, inv, a.st.MembersCanApprove
}

// Invite creates a one-time invite and returns its token.
func (a *Auth) Invite(ctx context.Context, email, role string, now time.Time) (string, error) {
	email = normEmail(email)
	if _, err := mail.ParseAddress(email); err != nil || !strings.Contains(email, "@") {
		return "", errors.New("not a valid email address")
	}
	if role != RoleAdmin && role != RoleMember {
		return "", errors.New("role must be admin or member")
	}
	tok := "sbi_" + randString(24)
	err := a.mutate(ctx, func(st *authState) error {
		for _, u := range st.Users {
			if strings.EqualFold(u.Email, email) {
				return errors.New(email + " is already a member")
			}
		}
		kept := st.Invites[:0]
		for _, i := range st.Invites {
			if i.Email != email && now.Unix() < i.Expires {
				kept = append(kept, i)
			}
		}
		st.Invites = append(kept, invite{SHA256: sha(tok), Email: email, Role: role, Expires: now.Add(inviteTTL).Unix()})
		return nil
	})
	return tok, err
}

// InviteEmail returns the email an invite token is for, if it is valid.
func (a *Auth) InviteEmail(tok string, now time.Time) (string, bool) {
	a.mu.RLock()
	defer a.mu.RUnlock()
	sum := sha(tok)
	for _, i := range a.st.Invites {
		if subtle.ConstantTimeCompare([]byte(i.SHA256), []byte(sum)) == 1 && now.Unix() < i.Expires {
			return i.Email, true
		}
	}
	return "", false
}

// AcceptInvite creates the user with the chosen password and consumes the invite.
func (a *Auth) AcceptInvite(ctx context.Context, tok, password string, now time.Time) (string, error) {
	if len(password) < minPasswordLen {
		return "", errors.New("use at least 12 characters")
	}
	hash, err := bcrypt.GenerateFromPassword([]byte(password), bcrypt.DefaultCost)
	if err != nil {
		return "", err
	}
	var email string
	err = a.mutate(ctx, func(st *authState) error {
		sum := sha(tok)
		for n, i := range st.Invites {
			if subtle.ConstantTimeCompare([]byte(i.SHA256), []byte(sum)) == 1 {
				if now.Unix() >= i.Expires {
					return errors.New("this invite expired; ask an admin for a new one")
				}
				email = i.Email
				st.Invites = append(st.Invites[:n], st.Invites[n+1:]...)
				st.Users = append(st.Users, user{Email: i.Email, Hash: string(hash), Role: i.Role})
				return nil
			}
		}
		return errors.New("this invite is not valid (already used or revoked)")
	})
	return email, err
}

func adminCount(st *authState) int {
	n := 0
	for _, u := range st.Users {
		if roleOf(u) == RoleAdmin {
			n++
		}
	}
	return n
}

// RemoveMember deletes a user, their pending invite and the agent tokens they approved.
// Their sessions stop working at once because every session check looks the user up.
func (a *Auth) RemoveMember(ctx context.Context, email string) (int, error) {
	email = normEmail(email)
	revoked := 0
	err := a.mutate(ctx, func(st *authState) error {
		found := false
		var users []user
		for _, u := range st.Users {
			if strings.EqualFold(u.Email, email) {
				found = true
				if roleOf(u) == RoleAdmin && adminCount(st) == 1 {
					return errors.New(errLastAdminMsg)
				}
				continue
			}
			users = append(users, u)
		}
		var invites []invite
		for _, i := range st.Invites {
			if i.Email == email {
				found = true
				continue
			}
			invites = append(invites, i)
		}
		if !found {
			return errors.New("no member or invite for " + email)
		}
		var toks []token
		for _, t := range st.Tokens {
			if strings.EqualFold(t.Owner, email) {
				revoked++
				continue
			}
			toks = append(toks, t)
		}
		st.Users, st.Invites, st.Tokens = users, invites, toks
		return nil
	})
	return revoked, err
}

// SetRole changes a member's role.
func (a *Auth) SetRole(ctx context.Context, email, role string) error {
	if role != RoleAdmin && role != RoleMember {
		return errors.New("role must be admin or member")
	}
	return a.mutate(ctx, func(st *authState) error {
		for n, u := range st.Users {
			if strings.EqualFold(u.Email, email) {
				if roleOf(u) == RoleAdmin && role == RoleMember && adminCount(st) == 1 {
					return errors.New(errLastAdminMsg)
				}
				st.Users[n].Role = role
				return nil
			}
		}
		return errors.New("no member " + email)
	})
}

// SetMembersCanApprove changes the install-wide approval setting.
func (a *Auth) SetMembersCanApprove(ctx context.Context, v bool) error {
	return a.mutate(ctx, func(st *authState) error { st.MembersCanApprove = v; return nil })
}

// --- API (admin token for changes) ---

func (s *Server) apiMembers(w http.ResponseWriter, r *http.Request) {
	ms, inv, can := s.auth.Members(time.Now())
	writeJSON(w, 200, map[string]any{"members": ms, "invites": inv, "membersCanApprove": can})
}

func (s *Server) requireAdminToken(w http.ResponseWriter, r *http.Request) bool {
	if scopeOf(r) != ScopeAdmin {
		fail(w, 403, "members are managed by a human in the dashboard", "https://"+s.hosts().Auth()+"/members")
		return false
	}
	return true
}

func (s *Server) apiInvite(w http.ResponseWriter, r *http.Request) {
	if !s.requireAdminToken(w, r) {
		return
	}
	var req struct{ Email, Role string }
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<12)).Decode(&req); err != nil {
		fail(w, 400, "bad JSON", "")
		return
	}
	if req.Role == "" {
		req.Role = RoleMember
	}
	tok, err := s.auth.Invite(r.Context(), req.Email, req.Role, time.Now())
	if err != nil {
		fail(w, 400, err.Error(), "")
		return
	}
	writeJSON(w, 201, map[string]any{"email": normEmail(req.Email), "role": req.Role, "inviteURL": s.inviteURL(tok),
		"expires": time.Now().Add(inviteTTL).UTC().Format(time.RFC3339),
		"note":    "Send this link to them yourself (SlicedBread does not send email). It works once, for 7 days."})
}

func (s *Server) inviteURL(tok string) string {
	return "https://" + s.hosts().Auth() + invitePath + "?token=" + tok
}

func (s *Server) apiRemoveMember(w http.ResponseWriter, r *http.Request) {
	if !s.requireAdminToken(w, r) {
		return
	}
	n, err := s.auth.RemoveMember(r.Context(), r.PathValue("email"))
	if err != nil {
		fail(w, 400, err.Error(), "")
		return
	}
	writeJSON(w, 200, map[string]any{"removed": r.PathValue("email"), "tokensRevoked": n})
}

func (s *Server) apiSetRole(w http.ResponseWriter, r *http.Request) {
	if !s.requireAdminToken(w, r) {
		return
	}
	var req struct{ Role string }
	json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<12)).Decode(&req)
	if err := s.auth.SetRole(r.Context(), r.PathValue("email"), req.Role); err != nil {
		fail(w, 400, err.Error(), "")
		return
	}
	writeJSON(w, 200, map[string]any{"email": r.PathValue("email"), "role": req.Role})
}

func (s *Server) apiMemberSettings(w http.ResponseWriter, r *http.Request) {
	if !s.requireAdminToken(w, r) {
		return
	}
	var req struct {
		MembersCanApprove *bool `json:"membersCanApprove"`
	}
	json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<12)).Decode(&req)
	if req.MembersCanApprove == nil {
		fail(w, 400, "set membersCanApprove to true or false", "")
		return
	}
	if err := s.auth.SetMembersCanApprove(r.Context(), *req.MembersCanApprove); err != nil {
		fail(w, 500, err.Error(), "")
		return
	}
	writeJSON(w, 200, map[string]any{"membersCanApprove": *req.MembersCanApprove})
}

// CanApproveSetting returns the install-wide "members can approve" setting.
func (a *Auth) CanApproveSetting() bool {
	a.mu.RLock()
	defer a.mu.RUnlock()
	return a.st.MembersCanApprove
}
