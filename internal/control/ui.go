package control

import (
	"html/template"
	"net/http"
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
</style></head><body>
<h1>SlicedBread</h1>
{{if .Error}}<p class="err">{{.Error}}</p>{{end}}
{{if eq .Page "login"}}
<form method="post" action="/login">
<input type="hidden" name="csrf" value="{{.CSRF}}"><input type="hidden" name="rd" value="{{.RD}}">
<label>Email<input name="email" type="email" autocomplete="username" required></label>
<label>Password<input name="password" type="password" autocomplete="current-password" required></label>
<button type="submit">Sign in</button></form>
{{else}}
<p class="muted">Signed in as {{.Email}} &middot; <a href="/logout">sign out</a> &middot; <a href="/">apps</a> &middot; <a href="/approvals">approvals</a></p>
{{if eq .Page "approvals"}}
<h2>Approvals</h2>
{{if not .Approvals}}<p>No approvals.</p>{{end}}
<table>{{range .Approvals}}<tr><td><code>{{.ID}}</code></td><td>{{.Summary}}<br><span class="muted">{{.RequestedAt}} &middot; {{.State}}{{if .Result}} &middot; {{.Result}}{{end}}</span></td>
<td>{{if eq .State "pending"}}<form method="post" action="/approvals/{{.ID}}"><input type="hidden" name="csrf" value="{{$.CSRF}}">
<button name="decision" value="approve">Approve</button><button name="decision" value="deny">Deny</button></form>{{end}}</td></tr>{{end}}</table>
{{else}}
<h2>Apps</h2>
{{if not .Apps}}<p>No apps yet. Ask your agent to deploy one, or run <code>sb deploy</code>.</p>{{end}}
<table>{{range .Apps}}<tr><td><a href="{{.URL}}">{{.Name}}</a></td><td>{{.State}}</td><td>{{.Access}}</td><td class="muted">{{.Source}}</td></tr>{{end}}</table>
{{if .Pending}}<p><a href="/approvals">{{.Pending}} approval(s) waiting</a></p>{{end}}
{{end}}{{end}}
</body></html>`))

type pageData struct {
	Page, Error, CSRF, RD, Email string
	Apps                         []AppStatus
	Approvals                    []*Approval
	Pending                      int
}

func (s *Server) render(w http.ResponseWriter, d pageData) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Content-Security-Policy", "default-src 'none'; style-src 'unsafe-inline'; form-action 'self'; frame-ancestors 'none'")
	w.Header().Set("X-Frame-Options", "DENY")
	pageTmpl.Execute(w, d)
}

func (s *Server) uiMux() http.Handler {
	m := http.NewServeMux()
	m.HandleFunc("GET /login", func(w http.ResponseWriter, r *http.Request) {
		s.render(w, pageData{Page: "login", CSRF: s.csrf(w, r), RD: r.URL.Query().Get("rd")})
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
		s.setCookie(w, SessionCookie, s.auth.NewSession(email, time.Now()), int(sessionTTL.Seconds()), true)
		if !SafeRedirect(rd, s.cfg.Domain) {
			rd = "/"
		}
		http.Redirect(w, r, rd, http.StatusFound)
	})
	m.HandleFunc("GET /logout", func(w http.ResponseWriter, r *http.Request) {
		s.setCookie(w, SessionCookie, "", -1, true)
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
		s.render(w, pageData{Page: "apps", Email: email, Apps: sts, Pending: pending, CSRF: s.csrf(w, r)})
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
		if _, err := s.decide(r.Context(), r.PathValue("id"), r.PostFormValue("decision")); err != nil {
			aps, _ := s.listApprovals(r.Context())
			s.render(w, pageData{Page: "approvals", Email: email, Approvals: aps, CSRF: s.csrf(w, r), Error: err.Error()})
			return
		}
		http.Redirect(w, r, "/approvals", http.StatusSeeOther)
	}))
	return m
}

func (s *Server) requireSession(h func(http.ResponseWriter, *http.Request, string)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		email, ok := s.auth.SessionFromRequest(r)
		if !ok {
			http.Redirect(w, r, "/login?rd="+"https://auth."+s.cfg.Domain+r.URL.Path, http.StatusFound)
			return
		}
		h(w, r, email)
	}
}
