package control

import (
	"errors"
	"html/template"
	"net/http"
	"net/url"
	"strings"
	"time"
)

var pageTmpl = template.Must(template.New("page").Parse(`<!doctype html>
<html lang="en"><head><meta charset="utf-8"><meta name="viewport" content="width=device-width,initial-scale=1">
<title>SlicedBread</title>
<style>
body{font-family:system-ui,-apple-system,sans-serif;max-width:760px;margin:40px auto;padding:0 16px;color:#222}
h1{font-size:22px} table{border-collapse:collapse;width:100%} td,th{text-align:left;padding:6px 8px;border-bottom:1px solid #eee;font-size:14px}
input{padding:8px;margin:4px 0;width:100%;box-sizing:border-box} button{padding:8px 14px;margin-right:6px;cursor:pointer}
.err{color:#b00} .muted{color:#777;font-size:13px} code{background:#f4f4f4;padding:1px 4px}
.alerts{margin:16px 0;padding:0;list-style:none} .alerts li{padding:8px 12px;margin:6px 0;border-left:4px solid #c62828;background:#fdecea;font-size:14px}
.alerts li.warning{border-color:#ef6c00;background:#fff4e5} .ok{color:#2e7d32}
</style></head><body>
<h1>SlicedBread</h1>
{{if .Error}}<p class="err">{{.Error}}</p>{{end}}
{{if eq .Page "login"}}
<form method="post" action="/login">
<input type="hidden" name="csrf" value="{{.CSRF}}"><input type="hidden" name="rd" value="{{.RD}}">
<label>Email<input name="email" type="email" autocomplete="username" required></label>
<label>Password<input name="password" type="password" autocomplete="current-password" required></label>
<button type="submit">Sign in</button></form>
{{else if eq .Page "setup"}}
<h2>Set up {{.Domain}}</h2>
<p>Create the owner account. The owner is an admin: they approve what agents ask for, manage members, backups and settings, all here in the dashboard. Agents work through the <code>sb</code> CLI and cannot do these things.</p>
<form method="post" action="/setup"><input type="hidden" name="csrf" value="{{.CSRF}}"><input type="hidden" name="code" value="{{.Code}}">
<label>Email (also used for the ZeroSSL fallback certificate account)<input name="email" type="email" value="{{.Email}}" autocomplete="username" required></label>
<label>Password (at least 12 characters)<input name="password" type="password" autocomplete="new-password" minlength="12" required></label>
<label>Repeat password<input name="password2" type="password" autocomplete="new-password" minlength="12" required></label>
<button type="submit">Create owner account</button></form>
{{else if eq .Page "invite"}}
{{if .Done}}<p>{{.Done}}</p>{{else}}
<h2>Join {{.Domain}}</h2>
<p>You were invited as <b>{{.Email}}</b> ({{.Role}}). Choose a password (at least 12 characters).</p>
<form method="post" action="/invite"><input type="hidden" name="csrf" value="{{.CSRF}}"><input type="hidden" name="token" value="{{.Code}}">
<label>Password<input name="password" type="password" autocomplete="new-password" minlength="12" required></label>
<label>Repeat password<input name="password2" type="password" autocomplete="new-password" minlength="12" required></label>
<button type="submit">Join</button></form>{{end}}
{{else}}
<p class="muted">Signed in as {{.Email}} ({{.Role}}) &middot; <a href="/logout">sign out</a> &middot; <a href="/">apps</a> &middot; <a href="/approvals">approvals</a> &middot; <a href="/members">members</a> &middot; <a href="/settings">settings</a></p>
{{if eq .Page "device"}}
<h2>Connect a CLI</h2>
{{if .Done}}<p>{{.Done}}</p>{{else}}
<p>A command line client is asking for an <b>agent</b> token for this tenant. Agent tokens can deploy and configure apps; making things public or deleting them still needs your approval.</p>
<p>Only continue if the code below matches what your terminal shows.</p>
<p style="font-size:28px;letter-spacing:3px"><code>{{.Code}}</code></p>
<form method="post" action="/device"><input type="hidden" name="csrf" value="{{.CSRF}}"><input type="hidden" name="code" value="{{.Code}}">
<button name="decision" value="approve">Allow</button><button name="decision" value="deny">Deny</button></form>{{end}}
{{else if eq .Page "approvals"}}
<h2>Approvals</h2>
{{if not .Approvals}}<p>No approvals.</p>{{end}}
<table>{{range .Approvals}}<tr><td><code>{{.ID}}</code></td><td>{{.Summary}}<br><span class="muted">{{.RequestedAt}} &middot; {{.State}}{{if .Result}} &middot; {{.Result}}{{end}}</span></td>
<td>{{if eq .State "pending"}}{{if $.CanApprove}}<form method="post" action="/approvals/{{.ID}}"><input type="hidden" name="csrf" value="{{$.CSRF}}">
<button name="decision" value="approve">Approve</button><button name="decision" value="deny">Deny</button></form>{{else}}<span class="muted">an admin approves</span>{{end}}{{end}}</td></tr>{{end}}</table>
{{if not .CanApprove}}<p class="muted">Members cannot approve on this install. An admin can change that on the Members page.</p>{{end}}
{{else if eq .Page "settings"}}{{with .Settings}}
{{if .Welcome}}<p class="ok"><b>Setup complete.</b> You are the owner of this install. Save the backup encryption password sb init printed in your password manager if you have not yet. Your agent uses the <code>sb</code> CLI (already logged in on the machine that ran sb init with an agent token; elsewhere: <code>sb login --api {{.APIURL}}</code>).</p>{{end}}
{{if $.Done}}<p class="ok">{{$.Done}}</p>{{end}}
<h2>Backups</h2>
{{if .BackupsConfigured}}<p>To <code>{{.BackupTarget}}</code>, daily at {{.BackupHour}}:00 UTC, each followed by a restore test.</p>{{else}}<p class="err">Not configured: app data is not backed up.</p>{{end}}
{{if $.IsAdmin}}
<form method="post" action="/settings/backups"><input type="hidden" name="csrf" value="{{$.CSRF}}">
<label>S3 endpoint (https://...)<input name="endpoint" required></label><label>Bucket<input name="bucket" required></label>
<label>Region<input name="region" value="auto"></label><label>Prefix (optional)<input name="prefix"></label>
<label>Hour (UTC, 0-23)<input name="hour" value="3"></label>
<label>Access key id<input name="key" autocomplete="off" required></label><label>Secret access key<input name="secret" type="password" autocomplete="off" required></label>
<button type="submit">{{if .BackupsConfigured}}Change backup target{{else}}Set up backups{{end}}</button></form>
<form method="post" action="/settings/backup-password"><input type="hidden" name="csrf" value="{{$.CSRF}}"><button type="submit">Show the backup encryption password</button></form>
{{if .Password}}<p>Backup encryption password: <code>{{.Password}}</code><br><span class="muted">Keep it in your password manager. If the servers are lost, it is the only way to read the backups.</span></p>{{end}}
<h3>Restore from the backup repository</h3>
<p class="muted">Lists every app in the backup bucket, including apps that were deleted or lived on an earlier install that used the same bucket and password.</p>
<form method="post" action="/settings/repository"><input type="hidden" name="csrf" value="{{$.CSRF}}"><button type="submit">Look in the backup repository</button></form>
{{if .RepoLoaded}}<table>{{range .Repo}}<tr><td>{{.Name}}</td><td class="muted">{{.Snapshots}} snapshots, latest {{.LatestAt}}</td><td>{{if .Exists}}deployed{{else}}<form method="post" action="/settings/recreate"><input type="hidden" name="csrf" value="{{$.CSRF}}"><input type="hidden" name="app" value="{{.Name}}"><button type="submit">Recreate</button></form>{{end}}</td></tr>{{end}}</table>
{{if .RepoDeleted}}<form method="post" action="/settings/recreate"><input type="hidden" name="csrf" value="{{$.CSRF}}"><button type="submit">Recreate all {{.RepoDeleted}} missing apps</button></form>{{end}}{{end}}
{{end}}
<h2>Alerts</h2>
<p>Webhook: {{if .Webhook}}{{.Webhook}}{{else}}none (alerts show on the Apps page and in <code>sb alerts</code>){{end}}</p>
{{if $.IsAdmin}}<form method="post" action="/settings/webhook"><input type="hidden" name="csrf" value="{{$.CSRF}}">
<label>Webhook URL (empty to remove)<input name="url"></label>
<label>Format <select name="format"><option>json</option><option>slack</option><option>discord</option><option>ntfy</option></select></label>
<button type="submit">Save webhook</button><button name="action" value="test">Send test message</button></form>{{end}}
<h2>OS updates</h2>
<p class="muted">Security updates install automatically on every server. Servers that need a reboot are restarted one at a time in the nightly window (an hour after backups); on 3 servers, apps without a volume keep serving, apps with a volume pause for about a minute while it moves.</p>
{{if .Maint}}{{with .Maint}}<p>Last run: {{if .LastRun}}{{.LastRun}}{{else}}never{{end}}{{if .Running}} (running now){{end}}</p>
<table>{{range $n, $m := .Nodes}}<tr><td>{{$n}}</td><td class="muted">checked {{$m.CheckedAt}}{{if $m.Kernel}}, kernel {{$m.Kernel}}{{end}}{{if $m.LastRebootAt}}, last reboot {{$m.LastRebootAt}} (away {{$m.LastRebootFor}}){{end}}{{if $m.RebootNeeded}}, <b>reboot pending</b>{{end}}{{if $m.Error}}, <span class="err">{{$m.Error}}</span>{{end}}</td></tr>{{end}}</table>{{end}}{{end}}
{{if $.IsAdmin}}<form method="post" action="/settings/maintenance"><input type="hidden" name="csrf" value="{{$.CSRF}}"><button type="submit">Run OS maintenance now</button></form>{{end}}
<h2>Certificates</h2>
<p>{{.Fallback}}</p>
{{if $.IsAdmin}}<form method="post" action="/settings/fallback"><input type="hidden" name="csrf" value="{{$.CSRF}}"><button type="submit">Set up ZeroSSL fallback with my email</button></form>{{end}}
{{if $.IsAdmin}}
<h2>API tokens</h2>
<p class="muted">Agent tokens come from <code>sb login</code> (approved here) or sb init; they cannot approve, change settings or see secrets. Admin tokens are only for automation such as CI, and always expire.</p>
{{if .NewToken}}<p class="ok">New admin token (shown once): <code>{{.NewToken}}</code></p>{{end}}
<table>{{range .Tokens}}<tr><td>{{.Name}}</td><td>{{.Scope}}</td><td class="muted">{{if .Owner}}by {{.Owner}}, {{end}}created {{.Created}}, expires {{.Expires}}{{if .Expired}} (expired){{end}}</td>
<td><form method="post" action="/settings/tokens/{{.ID}}/revoke"><input type="hidden" name="csrf" value="{{$.CSRF}}"><button type="submit">Revoke</button></form></td></tr>{{end}}</table>
<form method="post" action="/settings/tokens"><input type="hidden" name="csrf" value="{{$.CSRF}}">
<label>Name<input name="name" placeholder="e.g. github-actions deploy" required></label><label>Expires after (days, 1-90)<input name="days" value="30"></label>
<button type="submit">Create admin token</button></form>
<h2>Export</h2>
<form method="post" action="/settings/export"><input type="hidden" name="csrf" value="{{$.CSRF}}">
<label><input type="checkbox" name="secrets" value="true" style="width:auto"> include secret values</label>
<button type="submit">Download every app as Kubernetes manifests</button></form>
{{end}}{{end}}
{{else if eq .Page "app"}}{{with .App}}
<h2>{{.Status.Name}}</h2>
<p><a href="{{.Status.URL}}">{{.Status.URL}}</a> &middot; {{.Status.State}} &middot; {{.Status.Access}}</p>
{{if .Status.Message}}<p class="muted">{{.Status.Message}}</p>{{end}}
{{if .Status.Backups}}<p>Backups: {{.Status.Backups}}</p>{{end}}
{{if .Status.Setup}}<h3>Setup</h3><ul>{{range .Status.Setup}}<li>{{if .Done}}<span class="ok">done</span>{{else}}<b>to do</b>{{end}}: {{.Description}}{{if and .Link (not .Done)}} <a href="{{.Link}}">open</a>{{end}}</li>{{end}}</ul>{{end}}
{{if .Provide}}<h3>Secrets to enter</h3><p class="muted">Values are stored in the cluster and given to the app. Nobody, agents included, can read them back here or through the API.</p>
<table>{{range .Provide}}<tr><td><b>{{.Name}}</b>{{if .Required}} (required){{end}}<br><span class="muted">{{.Description}}</span></td><td>{{if .Set}}<span class="ok">set</span>{{end}}
<form method="post" action="/apps/{{$.App.Status.Name}}/secrets/{{.Name}}"><input type="hidden" name="csrf" value="{{$.CSRF}}"><input name="value" type="password" autocomplete="off" placeholder="{{if .Set}}replace value{{else}}value{{end}}"><button type="submit">Save</button></form></td></tr>{{end}}</table>{{end}}
{{if .Result}}<h3>{{.Result.Action}}</h3><table>{{range .Result.Rows}}<tr><td>{{index . 0}}</td><td><code>{{index . 1}}</code></td></tr>{{end}}</table>{{end}}
{{if .Actions}}<h3>Actions</h3><table>{{range .Actions}}<tr><td><b>{{.Name}}</b>{{if .HumanOnly}} <span class="muted">(shows credentials)</span>{{end}}{{if .On}} <span class="muted">(runs on {{.On}})</span>{{end}}<br><span class="muted">{{.Description}}</span></td><td>
<form method="post" action="/apps/{{$.App.Status.Name}}/actions/{{.Name}}"><input type="hidden" name="csrf" value="{{$.CSRF}}">{{range $k, $v := .Input}}<input name="{{$k}}" placeholder="{{$k}}{{if $v.Required}} (required){{end}}: {{$v.Description}}">{{end}}<button type="submit">Run</button></form></td></tr>{{end}}</table>{{end}}
{{end}}
{{else if eq .Page "members"}}
<h2>Members</h2>
{{if .Done}}<p>{{.Done}}</p>{{end}}
<table>{{range .Members}}<tr><td>{{.Email}}</td><td>{{.Role}}</td>
<td>{{if $.IsAdmin}}<form method="post" action="/members/{{.Email}}"><input type="hidden" name="csrf" value="{{$.CSRF}}">
{{if eq .Role "admin"}}<button name="action" value="member">Make member</button>{{else}}<button name="action" value="admin">Make admin</button>{{end}}<button name="action" value="remove">Remove</button></form>{{end}}</td></tr>{{end}}
{{range .Invites}}<tr><td>{{.Email}}</td><td class="muted">invited as {{.Role}}, until {{.Expires}}</td><td>{{if $.IsAdmin}}<form method="post" action="/members/{{.Email}}"><input type="hidden" name="csrf" value="{{$.CSRF}}"><button name="action" value="remove">Revoke</button></form>{{end}}</td></tr>{{end}}</table>
<p>Members can approve risky changes (making apps public, deletes, restores, actions marked for approval): <b>{{if .MembersCanApprove}}yes{{else}}no, only admins{{end}}</b></p>
{{if .IsAdmin}}
<form method="post" action="/members/settings"><input type="hidden" name="csrf" value="{{.CSRF}}">
{{if .MembersCanApprove}}<button name="membersCanApprove" value="false">Only admins approve</button>{{else}}<button name="membersCanApprove" value="true">Let members approve</button>{{end}}</form>
<h2>Invite</h2>
<form method="post" action="/members/invite"><input type="hidden" name="csrf" value="{{.CSRF}}">
<label>Email<input name="email" type="email" required></label>
<label><input type="radio" name="role" value="member" checked style="width:auto"> member</label>
<label><input type="radio" name="role" value="admin" style="width:auto"> admin</label>
<button type="submit">Create invite link</button></form>
{{end}}
{{else}}
{{if .Alerts}}<h2>Alerts</h2><ul class="alerts">{{range .Alerts}}<li class="{{.Severity}}"><b>{{.Title}}</b>{{if .Detail}}<br>{{.Detail}}{{end}}<br><span class="muted">since {{.Since}}{{if .Hint}} &middot; next: <code>{{.Hint}}</code>{{end}}</span></li>{{end}}</ul>
{{else}}<p class="ok">No alerts.</p>{{end}}
<h2>Apps</h2>
{{if not .Apps}}<p>No apps yet. Ask your agent to deploy one, or run <code>sb deploy</code>.</p>{{end}}
<table>{{if .Apps}}<tr><th>App</th><th>State</th><th>Access</th><th>Backups</th><th></th><th>Source</th></tr>{{end}}{{range .Apps}}<tr><td><a href="{{.URL}}">{{.Name}}</a></td><td>{{.State}}</td><td>{{.Access}}</td><td>{{.BackupShort}}{{if .Update}}<br><span class="muted">update: {{.Update}}</span>{{end}}</td><td><a href="/apps/{{.Name}}">details</a></td><td class="muted">{{.Source}}</td></tr>{{end}}</table>
{{if .Pending}}<p><a href="/approvals">{{.Pending}} approval(s) waiting</a></p>{{end}}
{{end}}{{end}}
</body></html>`))

type pageData struct {
	Page, Error, CSRF, RD, Email string
	Code, Done                   string
	Apps                         []AppStatus
	Approvals                    []*Approval
	Pending                      int
	Alerts                       []Alert
	Role, Domain                 string
	CanApprove, IsAdmin          bool
	MembersCanApprove            bool
	Members                      []Member
	Invites                      []PendingInvite
	Settings                     *settingsView
	App                          *appView
}

func (s *Server) render(w http.ResponseWriter, d pageData) {
	if d.Email != "" && d.Page != "invite" {
		d.Role = s.auth.Role(d.Email)
		d.CanApprove = s.auth.CanApprove(d.Email)
		d.IsAdmin = d.Role == RoleAdmin
	}
	d.Domain = s.cfg.Domain
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Content-Security-Policy", "default-src 'none'; style-src 'unsafe-inline'; form-action 'self'; frame-ancestors 'none'")
	w.Header().Set("X-Frame-Options", "DENY")
	pageTmpl.Execute(w, d)
}

func (s *Server) uiMux() http.Handler {
	m := http.NewServeMux()
	s.adminRoutes(m)
	m.HandleFunc("GET /setup", s.handleSetupGet)
	m.HandleFunc("POST /setup", s.handleSetupPost)
	m.HandleFunc("GET /login", func(w http.ResponseWriter, r *http.Request) {
		rd := r.URL.Query().Get("rd")
		if !s.auth.HasUsers() {
			s.render(w, pageData{Page: "login", Error: "This install is not set up yet. Open the setup link sb init printed, or on that machine run: sb setup-link <install>", CSRF: s.csrf(w, r)})
			return
		}
		if email, ok := s.auth.SessionFromRequest(r); ok {
			// Already signed in on the auth host: hand the app host its own session.
			http.Redirect(w, r, s.afterLogin(email, rd), http.StatusFound)
			return
		}
		s.render(w, pageData{Page: "login", CSRF: s.csrf(w, r), RD: rd})
	})
	m.HandleFunc("POST /login", func(w http.ResponseWriter, r *http.Request) {
		rd := r.PostFormValue("rd")
		if !s.checkCSRF(r) {
			s.render(w, pageData{Page: "login", Error: "Form expired, try again.", CSRF: s.csrf(w, r), RD: rd})
			return
		}
		email := r.PostFormValue("email")
		if !s.auth.CheckPassword(email, r.PostFormValue("password")) {
			time.Sleep(500 * time.Millisecond)
			s.render(w, pageData{Page: "login", Error: "Wrong email or password.", CSRF: s.csrf(w, r), RD: rd})
			return
		}
		s.setCookie(w, SessionCookie, s.auth.NewSession(email, requestHost(r), kindAuth, sessionTTL, time.Now()), int(sessionTTL.Seconds()))
		http.Redirect(w, r, s.afterLogin(email, rd), http.StatusFound)
	})
	m.HandleFunc("GET /logout", func(w http.ResponseWriter, r *http.Request) {
		s.setCookie(w, SessionCookie, "", -1)
		http.Redirect(w, r, "/login", http.StatusFound)
	})
	m.HandleFunc("GET /{$}", s.requireSession(func(w http.ResponseWriter, r *http.Request, email string) {
		list, _ := s.listApps(r.Context())
		var sts []AppStatus
		for _, a := range list {
			sts = append(sts, s.status(r.Context(), a))
		}
		aps, _ := s.listApprovals(r.Context())
		pending := 0
		for _, ap := range aps {
			if ap.State == "pending" {
				pending++
			}
		}
		alerts := s.loadAlertState(r.Context()).openAlerts()
		s.render(w, pageData{Page: "apps", Email: email, Apps: sts, Pending: pending, Alerts: alerts, CSRF: s.csrf(w, r)})
	}))
	m.HandleFunc("GET /device", s.requireSession(func(w http.ResponseWriter, r *http.Request, email string) {
		code := strings.ToUpper(strings.TrimSpace(r.URL.Query().Get("code")))
		s.render(w, pageData{Page: "device", Email: email, Code: code, CSRF: s.csrf(w, r)})
	}))
	m.HandleFunc("POST /device", s.requireSession(func(w http.ResponseWriter, r *http.Request, email string) {
		if !s.checkCSRF(r) {
			http.Error(w, "form expired, go back and retry", http.StatusForbidden)
			return
		}
		code := strings.ToUpper(strings.TrimSpace(r.PostFormValue("code")))
		allow := r.PostFormValue("decision") == "approve"
		d := pageData{Page: "device", Email: email, Code: code, CSRF: s.csrf(w, r)}
		if err := s.approveDevice(r.Context(), code, email, allow); err != nil {
			d.Error = err.Error()
		} else if allow {
			d.Done = "Done. The CLI is now connected; you can close this tab."
		} else {
			d.Done = "Denied. The CLI did not get a token."
		}
		s.render(w, d)
	}))
	m.HandleFunc("GET /approvals", s.requireSession(func(w http.ResponseWriter, r *http.Request, email string) {
		aps, err := s.listApprovals(r.Context())
		d := pageData{Page: "approvals", Email: email, Approvals: aps, CSRF: s.csrf(w, r)}
		if err != nil {
			d.Error = err.Error()
		}
		s.render(w, d)
	}))
	m.HandleFunc("POST /approvals/{id}", s.requireSession(func(w http.ResponseWriter, r *http.Request, email string) {
		if !s.checkCSRF(r) {
			http.Error(w, "form expired, go back and retry", http.StatusForbidden)
			return
		}
		if !s.auth.CanApprove(email) {
			s.render(w, pageData{Page: "approvals", Email: email, CSRF: s.csrf(w, r),
				Error: "Only admins can approve on this install. An admin can let members approve on the Members page."})
			return
		}
		if _, err := s.decide(r.Context(), r.PathValue("id"), r.PostFormValue("decision")); err != nil {
			aps, _ := s.listApprovals(r.Context())
			s.render(w, pageData{Page: "approvals", Email: email, Approvals: aps, CSRF: s.csrf(w, r), Error: err.Error()})
			return
		}
		http.Redirect(w, r, "/approvals", http.StatusSeeOther)
	}))
	m.HandleFunc("GET /members", s.requireSession(func(w http.ResponseWriter, r *http.Request, email string) {
		s.renderMembers(w, r, email, "", "")
	}))
	m.HandleFunc("POST /members/invite", s.requireSession(func(w http.ResponseWriter, r *http.Request, email string) {
		if !s.checkCSRF(r) || s.auth.Role(email) != RoleAdmin {
			s.renderMembers(w, r, email, "", "Only admins can invite.")
			return
		}
		tok, err := s.auth.Invite(r.Context(), r.PostFormValue("email"), r.PostFormValue("role"), time.Now())
		if err != nil {
			s.renderMembers(w, r, email, "", err.Error())
			return
		}
		s.renderMembers(w, r, email, "Invite link (send it yourself, it works once for 7 days): "+s.inviteURL(tok), "")
	}))
	m.HandleFunc("POST /members/settings", s.requireSession(func(w http.ResponseWriter, r *http.Request, email string) {
		if !s.checkCSRF(r) || s.auth.Role(email) != RoleAdmin {
			s.renderMembers(w, r, email, "", "Only admins can change this.")
			return
		}
		if err := s.auth.SetMembersCanApprove(r.Context(), r.PostFormValue("membersCanApprove") == "true"); err != nil {
			s.renderMembers(w, r, email, "", err.Error())
			return
		}
		http.Redirect(w, r, "/members", http.StatusSeeOther)
	}))
	m.HandleFunc("POST /members/{member}", s.requireSession(func(w http.ResponseWriter, r *http.Request, email string) {
		if !s.checkCSRF(r) || s.auth.Role(email) != RoleAdmin {
			s.renderMembers(w, r, email, "", "Only admins can change members.")
			return
		}
		var err error
		switch action := r.PostFormValue("action"); action {
		case "remove":
			_, err = s.auth.RemoveMember(r.Context(), r.PathValue("member"))
		case RoleAdmin, RoleMember:
			err = s.auth.SetRole(r.Context(), r.PathValue("member"), action)
		default:
			err = errors.New("unknown action")
		}
		if err != nil {
			s.renderMembers(w, r, email, "", err.Error())
			return
		}
		http.Redirect(w, r, "/members", http.StatusSeeOther)
	}))
	m.HandleFunc("GET "+invitePath, func(w http.ResponseWriter, r *http.Request) {
		tok := r.URL.Query().Get("token")
		invited, ok := s.auth.InviteEmail(tok, time.Now())
		if !ok {
			s.render(w, pageData{Page: "login", Error: "This invite link is not valid (expired, used or revoked). Ask an admin for a new one.", CSRF: s.csrf(w, r)})
			return
		}
		s.render(w, pageData{Page: "invite", Email: invited, Role: s.inviteRole(tok), Code: tok, CSRF: s.csrf(w, r)})
	})
	m.HandleFunc("POST "+invitePath, func(w http.ResponseWriter, r *http.Request) {
		tok := r.PostFormValue("token")
		invited, ok := s.auth.InviteEmail(tok, time.Now())
		d := pageData{Page: "invite", Email: invited, Role: s.inviteRole(tok), Code: tok, CSRF: s.csrf(w, r)}
		switch {
		case !s.checkCSRF(r):
			d.Error = "Form expired, try again."
		case !ok:
			d.Page, d.Error = "login", "This invite link is not valid (expired, used or revoked)."
		case r.PostFormValue("password") != r.PostFormValue("password2"):
			d.Error = "The passwords do not match."
		}
		if d.Error != "" {
			s.render(w, d)
			return
		}
		joined, err := s.auth.AcceptInvite(r.Context(), tok, r.PostFormValue("password"), time.Now())
		if err != nil {
			d.Error = err.Error()
			s.render(w, d)
			return
		}
		s.setCookie(w, SessionCookie, s.auth.NewSession(joined, requestHost(r), kindAuth, sessionTTL, time.Now()), int(sessionTTL.Seconds()))
		http.Redirect(w, r, "/", http.StatusSeeOther)
	})
	return m
}

func (s *Server) renderMembers(w http.ResponseWriter, r *http.Request, email, done, errMsg string) {
	ms, inv, can := s.auth.Members(time.Now())
	s.render(w, pageData{Page: "members", Email: email, Members: ms, Invites: inv, MembersCanApprove: can, CSRF: s.csrf(w, r), Done: done, Error: errMsg})
}

func (s *Server) inviteRole(tok string) string {
	s.auth.mu.RLock()
	defer s.auth.mu.RUnlock()
	sum := sha(tok)
	for _, i := range s.auth.st.Invites {
		if i.SHA256 == sum {
			return i.Role
		}
	}
	return ""
}

// afterLogin returns where to send a signed-in browser: an app host gets an exchange link.
func (s *Server) afterLogin(email, rd string) string {
	if !SafeRedirect(rd, s.cfg.Domain) {
		return "/"
	}
	u, _ := url.Parse(rd)
	if strings.EqualFold(u.Hostname(), s.hosts().Auth()) {
		return rd
	}
	return s.exchangeURL(email, rd)
}

func (s *Server) requireSession(h func(http.ResponseWriter, *http.Request, string)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		email, ok := s.auth.SessionFromRequest(r)
		if !ok {
			http.Redirect(w, r, "/login?rd="+url.QueryEscape("https://"+s.hosts().Auth()+r.URL.RequestURI()), http.StatusFound)
			return
		}
		h(w, r, email)
	}
}
