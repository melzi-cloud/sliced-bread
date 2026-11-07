package control

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/melzi-cloud/sliced-bread/internal/apps"
	"github.com/melzi-cloud/sliced-bread/internal/guard"
	"github.com/melzi-cloud/sliced-bread/internal/kube"
)

// Admin operations are done by humans in the dashboard. API tokens, including the one
// sb init leaves in the CLI, have agent rights; admin tokens exist only when an admin
// creates one here for automation, with a name and an expiry.

// --- tokens ---

// TokenView is a token as the dashboard lists it.
type TokenView struct {
	ID, Name, Scope, Owner, Created, Expires string
	Expired                                  bool
}

func tokenID(t token) string { return t.SHA256[:10] }

// Tokens lists API tokens, newest first.
func (a *Auth) Tokens(now time.Time) []TokenView {
	a.mu.RLock()
	defer a.mu.RUnlock()
	var out []TokenView
	for _, t := range a.st.Tokens {
		v := TokenView{ID: tokenID(t), Name: t.Name, Scope: t.Scope, Owner: t.Owner, Expires: "never"}
		if t.Created > 0 {
			v.Created = time.Unix(t.Created, 0).UTC().Format("2006-01-02 15:04")
		}
		if t.Expires > 0 {
			v.Expires = time.Unix(t.Expires, 0).UTC().Format("2006-01-02 15:04")
			v.Expired = now.Unix() >= t.Expires
		}
		out = append(out, v)
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].Created > out[j].Created })
	return out
}

// CreateAdminToken mints an admin token for automation. It must expire.
func (a *Auth) CreateAdminToken(ctx context.Context, name, owner string, ttl time.Duration) (string, error) {
	name = strings.TrimSpace(name)
	if name == "" || len(name) > 60 {
		return "", errors.New("give the token a name (up to 60 characters), e.g. the CI job using it")
	}
	if ttl <= 0 || ttl > 90*24*time.Hour {
		return "", errors.New("admin tokens expire after 1 to 90 days")
	}
	tok := "sbt_" + randString(24)
	now := time.Now()
	return tok, a.mutate(ctx, func(st *authState) error {
		st.Tokens = append(st.Tokens, token{Name: name, Scope: ScopeAdmin, SHA256: sha(tok), Owner: owner, Created: now.Unix(), Expires: now.Add(ttl).Unix()})
		return nil
	})
}

// RevokeToken deletes a token by its listed id.
func (a *Auth) RevokeToken(ctx context.Context, id string) error {
	return a.mutate(ctx, func(st *authState) error {
		for n, t := range st.Tokens {
			if tokenID(t) == id {
				st.Tokens = append(st.Tokens[:n], st.Tokens[n+1:]...)
				return nil
			}
		}
		return errors.New("no such token")
	})
}

// --- settings page ---

type settingsView struct {
	Welcome           bool
	BackupsConfigured bool
	BackupTarget      string
	BackupHour        int
	Password          string
	RepoLoaded        bool
	Repo              []*RepoApp
	RepoDeleted       int
	Webhook           string
	Fallback          string
	OwnerEmail        string
	Tokens            []TokenView
	NewToken          string
	APIURL            string
	Maint             *guard.MaintState
}

func (s *Server) settingsData(ctx context.Context) *settingsView {
	v := &settingsView{OwnerEmail: s.auth.OwnerEmail(), Tokens: s.auth.Tokens(time.Now()), APIURL: "https://" + s.hosts().API(),
		Maint: guard.LoadMaint(ctx, s.kube)}
	if c, _ := s.backupConfig(ctx); c != nil {
		v.BackupsConfigured, v.BackupTarget, v.BackupHour = true, strings.TrimPrefix(c.Repository(), "s3:"), c.Hour
	}
	if hook, err := s.webhookConfig(ctx); err == nil && hook != nil {
		if u, err := url.Parse(hook.URL); err == nil {
			v.Webhook = hook.Format + " to " + u.Host
		}
	}
	exists, ready, msg := s.issuerReady(ctx, fallbackIssuer)
	switch {
	case ready:
		v.Fallback = "ZeroSSL is set up as the fallback certificate authority. If Let's Encrypt cannot issue or renew, SlicedBread switches to it."
	case exists:
		v.Fallback = "ZeroSSL fallback is being registered: " + msg
	default:
		v.Fallback = "Not set up. " + s.fallback.get()
	}
	return v
}

func (s *Server) renderSettings(w http.ResponseWriter, r *http.Request, email string, v *settingsView, done, errMsg string) {
	if v == nil {
		v = s.settingsData(r.Context())
	}
	s.render(w, pageData{Page: "settings", Email: email, Settings: v, CSRF: s.csrf(w, r), Done: done, Error: errMsg})
}

// adminPost wraps a POST handler that only admins may use.
func (s *Server) adminPost(h func(w http.ResponseWriter, r *http.Request, email string)) http.HandlerFunc {
	return s.requireSession(func(w http.ResponseWriter, r *http.Request, email string) {
		if !s.checkCSRF(r) {
			s.renderSettings(w, r, email, nil, "", "Form expired, try again.")
			return
		}
		if s.auth.Role(email) != RoleAdmin {
			s.renderSettings(w, r, email, nil, "", "Only admins can change settings.")
			return
		}
		h(w, r, email)
	})
}

func (s *Server) adminRoutes(m *http.ServeMux) {
	m.HandleFunc("GET /settings", s.requireSession(func(w http.ResponseWriter, r *http.Request, email string) {
		v := s.settingsData(r.Context())
		v.Welcome = r.URL.Query().Get("welcome") == "1"
		s.renderSettings(w, r, email, v, "", "")
	}))
	m.HandleFunc("POST /settings/backups", s.adminPost(func(w http.ResponseWriter, r *http.Request, email string) {
		hour, _ := strconv.Atoi(r.PostFormValue("hour"))
		c := BackupConfig{Endpoint: strings.TrimSpace(r.PostFormValue("endpoint")), Bucket: strings.TrimSpace(r.PostFormValue("bucket")),
			Region: strings.TrimSpace(r.PostFormValue("region")), Prefix: strings.TrimSpace(r.PostFormValue("prefix")), Hour: hour,
			AccessKeyID: strings.TrimSpace(r.PostFormValue("key")), SecretAccessKey: strings.TrimSpace(r.PostFormValue("secret"))}
		generated, err := s.configureBackups(r.Context(), &c, true)
		if err != nil {
			s.renderSettings(w, r, email, nil, "", "Backups not saved: "+err.Error())
			return
		}
		msg := "Backups configured. The first backup of each app runs within a minute, followed by a restore test."
		if generated {
			msg += " This install had no encryption password yet, so one was created: use \"Show the backup encryption password\" and save it."
		}
		s.renderSettings(w, r, email, nil, msg, "")
	}))
	m.HandleFunc("POST /settings/backup-password", s.adminPost(func(w http.ResponseWriter, r *http.Request, email string) {
		c, err := s.backupSecret(r.Context())
		if err != nil || c == nil || c.Password == "" {
			s.renderSettings(w, r, email, nil, "", "No backup encryption password is installed.")
			return
		}
		v := s.settingsData(r.Context())
		v.Password = c.Password
		s.logf("backup encryption password shown to %s", email)
		s.renderSettings(w, r, email, v, "", "")
	}))
	m.HandleFunc("POST /settings/repository", s.adminPost(func(w http.ResponseWriter, r *http.Request, email string) {
		ctx, cancel := context.WithTimeout(r.Context(), 6*time.Minute)
		defer cancel()
		list, err := s.repositoryApps(ctx)
		if err != nil {
			s.renderSettings(w, r, email, nil, "", "Could not read the backup repository: "+err.Error())
			return
		}
		v := s.settingsData(r.Context())
		v.RepoLoaded, v.Repo = true, list
		for _, ra := range list {
			if !ra.Exists {
				v.RepoDeleted++
			}
		}
		s.renderSettings(w, r, email, v, "", "")
	}))
	m.HandleFunc("POST /settings/recreate", s.adminPost(func(w http.ResponseWriter, r *http.Request, email string) {
		ctx, cancel := context.WithTimeout(r.Context(), 15*time.Minute)
		defer cancel()
		want := map[string]string{}
		if app := r.PostFormValue("app"); app != "" {
			want[app] = ""
		} else {
			list, err := s.repositoryApps(ctx)
			if err != nil {
				s.renderSettings(w, r, email, nil, "", err.Error())
				return
			}
			for _, ra := range list {
				if !ra.Exists {
					want[ra.Name] = ""
				}
			}
		}
		if len(want) == 0 {
			s.renderSettings(w, r, email, nil, "Every app in the backup repository already exists.", "")
			return
		}
		done, err := s.recreate(ctx, want)
		if err != nil {
			s.renderSettings(w, r, email, nil, "", "Restore stopped: "+err.Error()+". Recreated so far: "+strings.Join(done, ", "))
			return
		}
		s.renderSettings(w, r, email, nil, "Recreated "+strings.Join(done, ", ")+". Each app starts once its data is restored; watch the Apps page.", "")
	}))
	m.HandleFunc("POST /settings/webhook", s.adminPost(func(w http.ResponseWriter, r *http.Request, email string) {
		ctx := r.Context()
		if r.PostFormValue("action") == "test" {
			hook, err := s.webhookConfig(ctx)
			if err != nil || hook == nil {
				s.renderSettings(w, r, email, nil, "", "No webhook configured.")
				return
			}
			if err := s.sendWebhook(ctx, hook, "test", Alert{ID: "test", Severity: "warning", Title: "test message from SlicedBread; alerts will arrive like this", Since: now()}); err != nil {
				s.renderSettings(w, r, email, nil, "", "Webhook delivery failed: "+err.Error())
				return
			}
			s.renderSettings(w, r, email, nil, "Test message sent.", "")
			return
		}
		if err := s.setWebhook(ctx, WebhookConfig{URL: strings.TrimSpace(r.PostFormValue("url")), Format: r.PostFormValue("format")}); err != nil {
			s.renderSettings(w, r, email, nil, "", err.Error())
			return
		}
		s.renderSettings(w, r, email, nil, "Webhook saved.", "")
	}))
	m.HandleFunc("POST /settings/maintenance", s.adminPost(func(w http.ResponseWriter, r *http.Request, email string) {
		if err := guard.RequestMaintenance(r.Context(), s.kube); err != nil {
			s.renderSettings(w, r, email, nil, "", err.Error())
			return
		}
		s.logf("OS maintenance requested by %s", email)
		s.renderSettings(w, r, email, nil, "OS maintenance requested: the guard starts within a minute and checks each server, rebooting those that need it one at a time.", "")
	}))
	m.HandleFunc("POST /settings/fallback", s.adminPost(func(w http.ResponseWriter, r *http.Request, email string) {
		ctx, cancel := context.WithTimeout(r.Context(), time.Minute)
		defer cancel()
		if err := s.enableFallbackIssuer(ctx, email); err != nil {
			s.renderSettings(w, r, email, nil, "", "ZeroSSL fallback not set up: "+err.Error())
			return
		}
		s.renderSettings(w, r, email, nil, "ZeroSSL fallback set up with "+email+".", "")
	}))
	m.HandleFunc("POST /settings/tokens", s.adminPost(func(w http.ResponseWriter, r *http.Request, email string) {
		days, _ := strconv.Atoi(r.PostFormValue("days"))
		tok, err := s.auth.CreateAdminToken(r.Context(), r.PostFormValue("name"), email, time.Duration(days)*24*time.Hour)
		if err != nil {
			s.renderSettings(w, r, email, nil, "", err.Error())
			return
		}
		v := s.settingsData(r.Context())
		v.NewToken = tok
		s.logf("admin token %q created by %s", r.PostFormValue("name"), email)
		s.renderSettings(w, r, email, v, "", "")
	}))
	m.HandleFunc("POST /settings/tokens/{id}/revoke", s.adminPost(func(w http.ResponseWriter, r *http.Request, email string) {
		if err := s.auth.RevokeToken(r.Context(), r.PathValue("id")); err != nil {
			s.renderSettings(w, r, email, nil, "", err.Error())
			return
		}
		s.renderSettings(w, r, email, nil, "Token revoked.", "")
	}))
	m.HandleFunc("POST /settings/export", s.adminPost(func(w http.ResponseWriter, r *http.Request, email string) {
		withSecrets := r.PostFormValue("secrets") == "true"
		data, err := s.exportData(r.Context(), withSecrets)
		if err != nil {
			s.renderSettings(w, r, email, nil, "", err.Error())
			return
		}
		tgz, err := exportTarball(data, withSecrets)
		if err != nil {
			s.renderSettings(w, r, email, nil, "", err.Error())
			return
		}
		name := "slicedbread-export-" + time.Now().UTC().Format("20060102-1504") + ".tar.gz"
		w.Header().Set("Content-Type", "application/gzip")
		w.Header().Set("Content-Disposition", `attachment; filename="`+name+`"`)
		w.Write(tgz)
	}))
	m.HandleFunc("GET /apps/{name}", s.requireSession(func(w http.ResponseWriter, r *http.Request, email string) {
		s.renderApp(w, r, email, nil, "")
	}))
	m.HandleFunc("POST /apps/{name}/actions/{action}", s.requireSession(func(w http.ResponseWriter, r *http.Request, email string) {
		if !s.checkCSRF(r) {
			s.renderApp(w, r, email, nil, "Form expired, try again.")
			return
		}
		a, err := s.getApp(r.Context(), r.PathValue("name"))
		if err != nil {
			http.NotFound(w, r)
			return
		}
		act := findAction(a, r.PathValue("action"))
		if act == nil {
			s.renderApp(w, r, email, nil, "No such action.")
			return
		}
		if act.AdminOnly && s.auth.Role(email) != RoleAdmin {
			s.renderApp(w, r, email, nil, "Only admins can run this action.")
			return
		}
		input := map[string]string{}
		for k := range act.Input {
			if v := strings.TrimSpace(r.PostFormValue(k)); v != "" {
				input[k] = v
			}
		}
		res, err := s.runAction(r.Context(), a, act.Name, input)
		if err != nil {
			s.renderApp(w, r, email, nil, act.Name+" failed: "+err.Error())
			return
		}
		s.logf("action %s/%s run by %s in the dashboard", a.Name, act.Name, email)
		s.renderApp(w, r, email, &actionResult{Action: act.Name, Rows: resultRows(res)}, "")
	}))
}

// repositoryApps lists the apps in the backup repository.
func (s *Server) repositoryApps(ctx context.Context) ([]*RepoApp, error) {
	snaps, err := s.repoSnapshots(ctx)
	if err != nil {
		return nil, err
	}
	byApp := map[string]*RepoApp{}
	for _, sn := range snaps {
		ra := byApp[sn.App]
		if ra == nil {
			ra = &RepoApp{Name: sn.App, Latest: sn.ID, LatestAt: sn.Time.UTC().Format(time.RFC3339)}
			if _, err := s.getApp(ctx, sn.App); err == nil {
				ra.Exists = true
			}
			byApp[sn.App] = ra
		}
		ra.Snapshots++
	}
	out := []*RepoApp{}
	for _, ra := range byApp {
		out = append(out, ra)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

// setWebhook stores or removes the alerts webhook.
func (s *Server) setWebhook(ctx context.Context, c WebhookConfig) error {
	if c.URL == "" {
		err := s.kube.Delete(ctx, "v1", "Secret", s.cfg.Namespace, alertsWebhook)
		if err != nil && !kube.IsNotFound(err) {
			return err
		}
		return nil
	}
	if c.Format == "" {
		c.Format = "json"
	}
	if err := c.validate(); err != nil {
		return err
	}
	return s.kube.Apply(ctx, webhookSecret(s.cfg.Namespace, c))
}

// --- app page ---

type actionResult struct {
	Action string
	Rows   [][2]string
}

type appView struct {
	Status  AppStatus
	Actions []apps.Action
	Result  *actionResult
}

func findAction(a *apps.App, name string) *apps.Action {
	if t := apps.Catalog[a.Template]; t != nil {
		for i := range t.Actions {
			if t.Actions[i].Name == name {
				return &t.Actions[i]
			}
		}
	}
	return nil
}

func resultRows(res map[string]any) [][2]string {
	var rows [][2]string
	for k, v := range res {
		var s string
		switch x := v.(type) {
		case string:
			s = x
		default:
			b, _ := json.Marshal(x)
			s = string(b)
		}
		rows = append(rows, [2]string{k, s})
	}
	sort.Slice(rows, func(i, j int) bool { return rows[i][0] < rows[j][0] })
	return rows
}

func (s *Server) renderApp(w http.ResponseWriter, r *http.Request, email string, res *actionResult, errMsg string) {
	a, err := s.getApp(r.Context(), r.PathValue("name"))
	if err != nil {
		http.NotFound(w, r)
		return
	}
	v := &appView{Status: s.status(r.Context(), a), Result: res}
	if t := apps.Catalog[a.Template]; t != nil {
		v.Actions = t.Actions
	}
	s.render(w, pageData{Page: "app", Email: email, App: v, CSRF: s.csrf(w, r), Error: errMsg})
}

// --- export bundle ---

func exportTarball(data map[string]any, withSecrets bool) ([]byte, error) {
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	add := func(name string, b []byte) error {
		if err := tw.WriteHeader(&tar.Header{Name: name, Mode: 0o600, Size: int64(len(b)), ModTime: time.Now()}); err != nil {
			return err
		}
		_, err := tw.Write(b)
		return err
	}
	indent := func(v any) []byte { b, _ := json.MarshalIndent(v, "", "  "); return append(b, '\n') }
	appsMap, _ := data["apps"].(map[string]exported)
	names := make([]string, 0, len(appsMap))
	for n := range appsMap {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, n := range names {
		e := appsMap[n]
		if err := add("apps/"+n+"/slicedbread-app.json", indent(e.Spec)); err != nil {
			return nil, err
		}
		if err := add("apps/"+n+"/kubernetes.json", indent(e.Manifests)); err != nil {
			return nil, err
		}
	}
	if b, ok := data["backups"]; ok {
		add("backups.json", indent(b))
	}
	secrets := "redacted"
	if withSecrets {
		secrets = "included: keep this file private"
	}
	readme := fmt.Sprintf(`Export of the SlicedBread install %v, %s.

apps/<name>/slicedbread-app.json  the app as SlicedBread stores it
apps/<name>/kubernetes.json       the Kubernetes objects SlicedBread applies for it (a v1 List):
                                  kubectl apply -f apps/<name>/kubernetes.json on any cluster with
                                  Traefik and CloudNativePG; the login wall middlewares need sb-control.
Secret values are %s.
App data lives in the backup repository (backups.json) and is read with restic and the backup
encryption password sb init printed (also shown on the dashboard's Settings page).
`, data["domain"], time.Now().UTC().Format(time.RFC3339), secrets)
	if err := add("README.txt", []byte(readme)); err != nil {
		return nil, err
	}
	if err := tw.Close(); err != nil {
		return nil, err
	}
	if err := gz.Close(); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}
