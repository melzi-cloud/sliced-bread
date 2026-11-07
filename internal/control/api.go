package control

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/melzi-cloud/sliced-bread/internal/apps"
	"github.com/melzi-cloud/sliced-bread/internal/kube"
)

type ctxKey string

const scopeKey ctxKey = "scope"

func scopeOf(r *http.Request) string { s, _ := r.Context().Value(scopeKey).(string); return s }

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	enc := json.NewEncoder(w)
	enc.SetEscapeHTML(false)
	enc.SetIndent("", "  ")
	enc.Encode(v)
}

// APIError is the error body every endpoint returns.
type APIError struct {
	Error string `json:"error"`
	Hint  string `json:"hint,omitempty"`
}

func fail(w http.ResponseWriter, code int, msg, hint string) {
	writeJSON(w, code, APIError{Error: msg, Hint: hint})
}

func decode(r *http.Request, v any) error {
	dec := json.NewDecoder(io.LimitReader(r.Body, 1<<20))
	dec.DisallowUnknownFields()
	return dec.Decode(v)
}

func (s *Server) apiMux() http.Handler {
	m := http.NewServeMux()
	m.HandleFunc("GET /v1/whoami", s.apiWhoami)
	m.HandleFunc("GET /v1/catalog", s.apiCatalog)
	m.HandleFunc("POST /v1/catalog/refresh", s.apiCatalogRefresh)
	m.HandleFunc("GET /v1/apps", s.apiListApps)
	m.HandleFunc("POST /v1/apps", s.apiCreateApp)
	m.HandleFunc("GET /v1/apps/{name}", s.apiGetApp)
	m.HandleFunc("DELETE /v1/apps/{name}", s.apiDeleteApp)
	m.HandleFunc("PATCH /v1/apps/{name}/env", s.apiEnv)
	m.HandleFunc("POST /v1/apps/{name}/settings", s.apiSettings)
	m.HandleFunc("POST /v1/apps/{name}/access", s.apiAccess)
	m.HandleFunc("POST /v1/apps/{name}/redeploy", s.apiRedeploy)
	m.HandleFunc("GET /v1/apps/{name}/logs", s.apiLogs)
	m.HandleFunc("POST /v1/apps/{name}/actions/{action}", s.apiAction)
	m.HandleFunc("GET /v1/apps/{name}/check", s.apiCheck)
	m.HandleFunc("POST /v1/uploads", s.apiUpload)
	m.HandleFunc("GET /v1/backups", s.apiBackups)
	m.HandleFunc("PUT /v1/backups/config", s.apiBackupConfig)
	m.HandleFunc("PUT /v1/backups/schedule", s.apiBackupSchedule)
	m.HandleFunc("GET /v1/backups/password", s.apiBackupPassword)
	m.HandleFunc("POST /v1/apps/{name}/backup", s.apiStartBackup)
	m.HandleFunc("POST /v1/apps/{name}/restore-test", s.apiStartRestoreTest)
	m.HandleFunc("POST /v1/apps/{name}/restore", s.apiRestore)
	m.HandleFunc("GET /v1/backups/repository", s.apiRepository)
	m.HandleFunc("POST /v1/recreate", s.apiRecreate)
	m.HandleFunc("GET /v1/export", s.apiExport)
	m.HandleFunc("GET /v1/alerts", s.apiAlerts)
	m.HandleFunc("GET /v1/nodes", s.apiNodes)
	m.HandleFunc("GET /v1/members", s.apiMembers)
	m.HandleFunc("POST /v1/members", s.apiInvite)
	m.HandleFunc("DELETE /v1/members/{email}", s.apiRemoveMember)
	m.HandleFunc("POST /v1/members/{email}/role", s.apiSetRole)
	m.HandleFunc("PUT /v1/members/settings", s.apiMemberSettings)
	m.HandleFunc("PUT /v1/alerts/webhook", s.apiAlertsWebhook)
	m.HandleFunc("POST /v1/alerts/test", s.apiAlertsTest)
	m.HandleFunc("GET /v1/approvals", s.apiListApprovals)
	m.HandleFunc("GET /v1/approvals/{id}", s.apiGetApproval)
	m.HandleFunc("POST /v1/approvals/{id}/{decision}", s.apiDecide)
	public := http.NewServeMux()
	public.HandleFunc("POST /v1/device", s.apiDeviceStart)
	public.HandleFunc("GET /v1/device/{poll}", s.apiDevicePoll)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/v1/device") {
			public.ServeHTTP(w, r)
			return
		}
		scope := s.auth.TokenScope(r)
		if scope == "" {
			fail(w, http.StatusUnauthorized, "missing or invalid API token", "run: sb login")
			return
		}
		m.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), scopeKey, scope)))
	})
}

func (s *Server) apiWhoami(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, 200, map[string]any{
		"scope": scopeOf(r), "domain": s.cfg.Domain, "team": s.auth.Emails(), "membersCanApprove": s.auth.CanApproveSetting(),
		"wallAdmits": "signed-in team members listed in team",
		"dashboard":  "https://" + s.hosts().Auth(), "approvals": "https://" + s.hosts().Auth() + "/approvals",
		"appUrlPattern": "https://<app>." + s.cfg.Domain,
	})
}

func (s *Server) apiCatalog(w http.ResponseWriter, r *http.Request) {
	var out []catalogEntry
	for _, n := range apps.TemplateNames() {
		out = append(out, catalogView(apps.CatalogTemplate(n)))
	}
	writeJSON(w, 200, map[string]any{"templates": out, "catalog": apps.CurrentCatalog()})
}

func (s *Server) apiListApps(w http.ResponseWriter, r *http.Request) {
	list, err := s.listApps(r.Context())
	if err != nil {
		fail(w, 500, err.Error(), "")
		return
	}
	out := []AppStatus{}
	for _, a := range list {
		out = append(out, s.status(r.Context(), a))
	}
	writeJSON(w, 200, map[string]any{"apps": out})
}

func (s *Server) loadApp(w http.ResponseWriter, r *http.Request) *apps.App {
	a, err := s.getApp(r.Context(), r.PathValue("name"))
	if kube.IsNotFound(err) {
		fail(w, 404, "no app named "+r.PathValue("name"), "list apps with: sb apps")
		return nil
	}
	if err != nil {
		fail(w, 500, err.Error(), "")
		return nil
	}
	return a
}

func (s *Server) apiGetApp(w http.ResponseWriter, r *http.Request) {
	if a := s.loadApp(w, r); a != nil {
		writeJSON(w, 200, s.status(r.Context(), a))
	}
}

// DeployRequest creates an app.
type DeployRequest struct {
	Name     string            `json:"name"`
	Template string            `json:"template,omitempty"`
	Image    string            `json:"image,omitempty"`
	Repo     *apps.Repo        `json:"repo,omitempty"`
	Upload   string            `json:"upload,omitempty"` // id returned by POST /v1/uploads
	Port     int               `json:"port,omitempty"`
	Env      map[string]string `json:"env,omitempty"`
	// TemplateFiles deploys a template from a local directory (sb deploy --template-dir):
	// template.yaml plus its other files by relative path.
	TemplateFiles map[string]string `json:"templateFiles,omitempty"`
	// Generate names env keys whose values sb-control generates; Request names env keys a
	// human enters in the dashboard. Secret values never travel through the API (D-030).
	Generate    []string             `json:"generate,omitempty"`
	Request     []apps.ProvideSecret `json:"request,omitempty"`
	Access      string               `json:"access,omitempty"`
	PublicPaths []string             `json:"publicPaths,omitempty"`
	Settings    map[string]string    `json:"settings,omitempty"`
	Database    bool                 `json:"database,omitempty"`
	HealthPath  string               `json:"healthPath,omitempty"`
}

// needsApproval reports why an agent cannot apply this app state directly, or "".
func needsApproval(scope string, prev, next *apps.App) string {
	if scope == ScopeAdmin {
		return ""
	}
	var reasons []string
	prevAccess := apps.AccessWall
	if prev != nil {
		prevAccess = prev.Access
	}
	// Only built-in (trusted) templates may open access or add public paths without a
	// human: a template from a local directory is treated like any agent change.
	templateAccess := ""
	var templatePublic []string
	if t := apps.TemplateOf(next); t != nil && t.Trusted {
		templateAccess = t.Access.Mode
		templatePublic = t.Access.PublicPaths
	}
	if next.Access != prevAccess && next.Access != apps.AccessWall && next.Access != templateAccess {
		reasons = append(reasons, fmt.Sprintf("access %s -> %s", prevAccess, next.Access))
	}
	var had []string
	if prev != nil {
		had = prev.PublicPaths
	}
	for _, p := range next.PublicPaths {
		if !containsStr(had, p) && !containsStr(templatePublic, p) {
			reasons = append(reasons, fmt.Sprintf("public path %s", p))
		}
	}
	if t := apps.TemplateOf(next); t != nil {
		for _, set := range t.Settings {
			v := next.Settings[set.Name]
			old := ""
			if prev != nil {
				old = prev.Settings[set.Name]
			}
			for _, needs := range set.ApprovalFor {
				if v == needs && old != needs {
					reasons = append(reasons, fmt.Sprintf("setting %s=%s", set.Name, v))
				}
			}
		}
	}
	if len(reasons) == 0 {
		return ""
	}
	return strings.Join(reasons, "; ")
}

func (s *Server) approvalResponse(w http.ResponseWriter, ap *Approval) {
	writeJSON(w, http.StatusAccepted, map[string]any{
		"approval":   ap.ID,
		"state":      "pending",
		"summary":    ap.Summary,
		"approveUrl": "https://" + s.hosts().Auth() + "/approvals",
		"hint":       "A human must approve this. Give the user the approve URL. Then wait with: sb approvals --wait " + ap.ID,
	})
}

func (s *Server) apiCreateApp(w http.ResponseWriter, r *http.Request) {
	var req DeployRequest
	if err := decode(r, &req); err != nil {
		fail(w, 400, "invalid request: "+err.Error(), "")
		return
	}
	a := &apps.App{
		Name: req.Name, Template: req.Template, Image: req.Image, Repo: req.Repo, Port: req.Port,
		Env: req.Env, Settings: req.Settings, PublicPaths: req.PublicPaths,
	}
	if len(req.TemplateFiles) > 0 {
		raw := req.TemplateFiles["template.yaml"]
		files := map[string]string{}
		for k, v := range req.TemplateFiles {
			if k != "template.yaml" {
				files[k] = v
			}
		}
		t, err := apps.ParseTemplate([]byte(raw), files)
		if err != nil {
			fail(w, 400, err.Error(), "fix the template; the format is in the skill file and docs/template-format.md")
			return
		}
		a.TemplateDef, a.Template = t, t.Name
	}
	if req.Upload != "" {
		up, ok := s.uploadInfo(req.Upload)
		if !ok {
			fail(w, 400, "unknown upload "+req.Upload, "upload the directory again with: sb deploy --dir")
			return
		}
		a.Upload = up
	}
	envKeys := append([]string{}, req.Generate...)
	for k := range req.Env {
		envKeys = append(envKeys, k)
	}
	for _, p := range req.Request {
		envKeys = append(envKeys, p.Name)
	}
	if a.Template != "" {
		if a.Image != "" || a.Repo != nil || a.Upload != nil {
			fail(w, 400, "use exactly one of: template, image, repo, directory", "")
			return
		}
		if err := apps.ApplyTemplate(a); err != nil {
			fail(w, 400, err.Error(), "see templates with: sb catalog")
			return
		}
	}
	if err := apps.CheckUserEnv(a, envKeys...); err != nil {
		fail(w, 400, err.Error(), "")
		return
	}
	if req.Access != "" {
		a.Access = req.Access
	}
	if a.Access == "" {
		a.Access = apps.AccessWall
	}
	if req.Database {
		a.Database = true
		if a.Env == nil {
			a.Env = map[string]string{}
		}
		if _, set := a.Env["DATABASE_URL"]; !set {
			a.Env["DATABASE_URL"] = "${db.url}"
		}
	}
	if req.HealthPath != "" {
		a.HealthPath = req.HealthPath
	}
	if err := addSecretKeys(a, req.Generate, req.Request); err != nil {
		fail(w, 400, err.Error(), "")
		return
	}
	if err := a.Validate(); err != nil {
		fail(w, 400, err.Error(), "")
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	ctx := r.Context()
	if _, err := s.getApp(ctx, a.Name); err == nil {
		fail(w, 409, "app "+a.Name+" already exists", "to ship new code run: sb redeploy "+a.Name+" [--dir <path>]; change config with sb env / sb settings; or pick another name")
		return
	}
	if why := needsApproval(scopeOf(r), nil, a); why != "" {
		ap, err := s.requestApproval(ctx, "create", a.Name, "create app "+a.Name+": "+why, a, scopeOf(r))
		if err != nil {
			fail(w, 500, err.Error(), "")
			return
		}
		s.approvalResponse(w, ap)
		return
	}
	if err := s.saveApp(ctx, a); err != nil {
		fail(w, 500, err.Error(), "")
		return
	}
	if err := s.reconcile(ctx, a); err != nil {
		fail(w, 500, "saved but failed to apply: "+err.Error(), "")
		return
	}
	st := s.status(ctx, a)
	writeJSON(w, 201, map[string]any{"app": st, "hint": "Wait until it is running with: sb wait " + a.Name})
}

func (s *Server) apiDeleteApp(w http.ResponseWriter, r *http.Request) {
	a := s.loadApp(w, r)
	if a == nil {
		return
	}
	if scopeOf(r) != ScopeAdmin {
		ap, err := s.requestApproval(r.Context(), "delete", a.Name, "delete app "+a.Name+" and all its data", nil, scopeOf(r))
		if err != nil {
			fail(w, 500, err.Error(), "")
			return
		}
		s.approvalResponse(w, ap)
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.deleteApp(r.Context(), a.Name); err != nil {
		fail(w, 500, err.Error(), "")
		return
	}
	writeJSON(w, 200, map[string]any{"deleted": a.Name})
}

// EnvRequest changes env vars and secrets.
type EnvRequest struct {
	Set      map[string]string    `json:"set,omitempty"`
	Unset    []string             `json:"unset,omitempty"`
	Generate []string             `json:"generate,omitempty"` // sb-control creates the value
	Request  []apps.ProvideSecret `json:"request,omitempty"`  // a human enters the value in the dashboard
}

// addSecretKeys records generated and requested secret env keys on an app.
func addSecretKeys(a *apps.App, generate []string, request []apps.ProvideSecret) error {
	for _, k := range generate {
		if _, plain := a.Env[k]; plain {
			return fmt.Errorf("key %s is set as plain env; unset it first", k)
		}
		if !hasGen(a, k) {
			a.Generate = append(a.Generate, apps.GenSecret{Name: k, Bytes: 32, Format: "hex"})
		}
		a.AddSecretKey(k)
	}
	for _, p := range request {
		if _, plain := a.Env[p.Name]; plain {
			return fmt.Errorf("key %s is set as plain env; unset it first", p.Name)
		}
		found := false
		for i := range a.Provide {
			if a.Provide[i].Name == p.Name {
				a.Provide[i], found = p, true
			}
		}
		if !found {
			a.Provide = append(a.Provide, p)
		}
		a.AddSecretKey(p.Name)
	}
	return nil
}

func hasGen(a *apps.App, k string) bool {
	for _, g := range a.Generate {
		if g.Name == k {
			return true
		}
	}
	return false
}

func (s *Server) apiEnv(w http.ResponseWriter, r *http.Request) {
	var req EnvRequest
	if err := decode(r, &req); err != nil {
		fail(w, 400, "invalid request: "+err.Error(), "")
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	a := s.loadApp(w, r)
	if a == nil {
		return
	}
	keys := append(append([]string{}, req.Unset...), req.Generate...)
	for k := range req.Set {
		keys = append(keys, k)
	}
	for _, p := range req.Request {
		keys = append(keys, p.Name)
	}
	if err := apps.CheckUserEnv(a, keys...); err != nil {
		fail(w, 400, err.Error(), "use: sb settings")
		return
	}
	if a.Env == nil {
		a.Env = map[string]string{}
	}
	for k, v := range req.Set {
		a.Env[k] = v
		a.RemoveSecretKey(k)
	}
	for _, k := range req.Unset {
		delete(a.Env, k)
		a.RemoveSecretKey(k)
		var gen []apps.GenSecret
		for _, g := range a.Generate {
			if g.Name != k || (a.TemplateDef != nil && a.TemplateDef.Secrets.Generate != nil && templateGenerates(a, k)) {
				gen = append(gen, g)
			}
		}
		a.Generate = gen
		var prov []apps.ProvideSecret
		for _, p := range a.Provide {
			if p.Name != k {
				prov = append(prov, p)
			}
		}
		a.Provide = prov
	}
	if err := addSecretKeys(a, req.Generate, req.Request); err != nil {
		fail(w, 400, err.Error(), "")
		return
	}
	if err := a.Validate(); err != nil {
		fail(w, 400, err.Error(), "")
		return
	}
	if _, err := apps.Render(a, s.params(a, "")); err != nil {
		fail(w, 400, err.Error(), "")
		return
	}
	ctx := r.Context()
	if err := s.setSecrets(ctx, a, nil, req.Unset); err != nil {
		fail(w, 500, err.Error(), "")
		return
	}
	if err := s.saveApp(ctx, a); err != nil {
		fail(w, 500, err.Error(), "")
		return
	}
	if err := s.reconcile(ctx, a); err != nil {
		fail(w, 500, err.Error(), "")
		return
	}
	writeJSON(w, 200, map[string]any{"app": s.status(ctx, a), "hint": "Pods restart with the new environment. Check with: sb wait " + a.Name})
}

func (s *Server) applyOrApprove(w http.ResponseWriter, r *http.Request, prev, next *apps.App, summary string) {
	ctx := r.Context()
	if why := needsApproval(scopeOf(r), prev, next); why != "" {
		ap, err := s.requestApproval(ctx, "update", next.Name, summary+": "+why, next, scopeOf(r))
		if err != nil {
			fail(w, 500, err.Error(), "")
			return
		}
		s.approvalResponse(w, ap)
		return
	}
	if err := s.saveApp(ctx, next); err != nil {
		fail(w, 500, err.Error(), "")
		return
	}
	if err := s.reconcile(ctx, next); err != nil {
		fail(w, 500, err.Error(), "")
		return
	}
	writeJSON(w, 200, map[string]any{"app": s.status(ctx, next)})
}

func cloneApp(a *apps.App) *apps.App {
	b, _ := json.Marshal(a)
	var c apps.App
	json.Unmarshal(b, &c)
	return &c
}

func (s *Server) apiSettings(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Settings map[string]string `json:"settings"`
	}
	if err := decode(r, &req); err != nil {
		fail(w, 400, "invalid request: "+err.Error(), "")
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	prev := s.loadApp(w, r)
	if prev == nil {
		return
	}
	if prev.Template == "" {
		fail(w, 400, prev.Name+" is not a template app and has no settings", "use: sb env set")
		return
	}
	next := cloneApp(prev)
	for k, v := range req.Settings {
		next.Settings[k] = v
	}
	if err := apps.ApplyTemplate(next); err != nil {
		fail(w, 400, err.Error(), "see settings with: sb catalog")
		return
	}
	s.applyOrApprove(w, r, prev, next, "change settings of "+next.Name)
}

func (s *Server) apiAccess(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Access      string   `json:"access"`
		PublicPaths []string `json:"publicPaths"`
	}
	if err := decode(r, &req); err != nil {
		fail(w, 400, "invalid request: "+err.Error(), "")
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	prev := s.loadApp(w, r)
	if prev == nil {
		return
	}
	next := cloneApp(prev)
	if req.Access != "" {
		next.Access = req.Access
	}
	if req.PublicPaths != nil {
		next.PublicPaths = req.PublicPaths
	}
	if err := next.Validate(); err != nil {
		fail(w, 400, err.Error(), "")
		return
	}
	s.applyOrApprove(w, r, prev, next, "change access of "+next.Name)
}

func (s *Server) apiRedeploy(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Upload string `json:"upload"`
	}
	if r.ContentLength != 0 {
		if err := decode(r, &req); err != nil {
			fail(w, 400, "invalid request: "+err.Error(), "")
			return
		}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	a := s.loadApp(w, r)
	if a == nil {
		return
	}
	ctx := r.Context()
	if req.Upload != "" {
		if a.Upload == nil {
			fail(w, 400, a.Name+" was not deployed from a directory", "")
			return
		}
		up, ok := s.uploadInfo(req.Upload)
		if !ok {
			fail(w, 400, "unknown upload "+req.Upload, "")
			return
		}
		a.Upload = up
	}
	if a.Builds() {
		if a.Build != nil && a.Build.Phase == "running" {
			fail(w, 409, "a build is already running", "follow it with: sb logs "+a.Name+" --build")
			return
		}
		if err := s.startBuild(ctx, a); err != nil {
			fail(w, 500, err.Error(), "")
			return
		}
		writeJSON(w, 202, map[string]any{"app": s.status(ctx, a), "hint": "Rebuilding. Follow with: sb logs " + a.Name + " --build, then: sb wait " + a.Name})
		return
	}
	if a.Env == nil {
		a.Env = map[string]string{}
	}
	a.Env["SB_RESTARTED_AT"] = time.Now().UTC().Format(time.RFC3339)
	if err := s.saveApp(ctx, a); err == nil {
		err = s.reconcile(ctx, a)
	}
	writeJSON(w, 202, map[string]any{"app": s.status(ctx, a), "hint": "Restarting. Check with: sb wait " + a.Name})
}

func (s *Server) apiLogs(w http.ResponseWriter, r *http.Request) {
	a := s.loadApp(w, r)
	if a == nil {
		return
	}
	tail, _ := strconv.Atoi(r.URL.Query().Get("tail"))
	if tail <= 0 || tail > 2000 {
		tail = 100
	}
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	ctx := r.Context()
	if r.URL.Query().Get("build") != "" {
		io.WriteString(w, s.buildLogs(ctx, a, tail))
		return
	}
	ns := apps.Namespace(a.Name)
	if act := r.URL.Query().Get("action"); act != "" {
		pods, _ := s.kube.List(ctx, "v1", "Pod", ns, apps.ActionLabel+"="+dashed(act))
		if len(pods) == 0 {
			io.WriteString(w, "no recent run of action "+act+" (runs are kept for 15 minutes)\n")
			return
		}
		sort.Slice(pods, func(i, j int) bool {
			return kube.Str(pods[i], "metadata", "creationTimestamp") > kube.Str(pods[j], "metadata", "creationTimestamp")
		})
		l, err := s.kube.Logs(ctx, ns, kube.Str(pods[0], "metadata", "name"), "action", tail)
		if err != nil {
			io.WriteString(w, "logs unavailable: "+err.Error()+"\n")
			return
		}
		// Action output can contain secret values (scripts receive them); mask them.
		io.WriteString(w, maskSecrets(l, s.secretValuesForMask(ctx, a)).(string))
		return
	}
	pod, err := s.componentPod(ctx, a, r.URL.Query().Get("component"))
	if err != nil {
		io.WriteString(w, "no running pod yet: "+err.Error()+"\n")
		return
	}
	l, err := s.kube.Logs(ctx, ns, pod, "app", tail)
	if err != nil {
		io.WriteString(w, "logs unavailable: "+err.Error()+"\n")
		return
	}
	io.WriteString(w, l)
}

func (s *Server) apiAction(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Input map[string]string `json:"input"`
	}
	if r.ContentLength != 0 {
		if err := decode(r, &req); err != nil {
			fail(w, 400, "invalid request: "+err.Error(), "")
			return
		}
	}
	a := s.loadApp(w, r)
	if a == nil {
		return
	}
	var act *apps.Action
	if a.TemplateDef != nil {
		act = a.TemplateDef.Action(r.PathValue("action"))
	}
	if act == nil {
		fail(w, 404, fmt.Sprintf("app %s has no action %q", a.Name, r.PathValue("action")), "list actions with: sb status "+a.Name)
		return
	}
	if err := checkInput(act, req.Input); err != nil {
		fail(w, 400, err.Error(), "")
		return
	}
	appPage := "https://" + s.hosts().Auth() + "/apps/" + a.Name
	if act.Approval && scopeOf(r) != ScopeAdmin {
		ap, err := s.requestApproval(r.Context(), "action", a.Name, "run action "+act.Name+" on "+a.Name+inputSummary(req.Input), nil, scopeOf(r))
		if err != nil {
			fail(w, 500, err.Error(), "")
			return
		}
		ap.Action, ap.Input = act.Name, req.Input
		s.saveApproval(r.Context(), ap)
		s.approvalResponse(w, ap)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), act.TimeoutDuration()+actionStartSlack+10*time.Second)
	defer cancel()
	res, err := s.runAction(ctx, a, act.Name, req.Input)
	if err != nil {
		fail(w, 400, err.Error(), "")
		return
	}
	out := res.Result
	if len(res.Secret) > 0 {
		// Secret outputs never reach API clients, admin tokens included (D-030).
		var names []string
		for k := range res.Secret {
			names = append(names, k)
			out[k] = "[secret: shown only in the dashboard]"
		}
		sort.Strings(names)
		out["dashboard"] = appPage
		out["note"] = "This action returns " + strings.Join(names, ", ") + ", which only a signed-in human sees: give the user the dashboard link (run " + act.Name + " there)."
	}
	writeJSON(w, 200, out)
}

func inputSummary(in map[string]string) string {
	if len(in) == 0 {
		return ""
	}
	var parts []string
	for k, v := range in {
		parts = append(parts, k+"="+v)
	}
	sort.Strings(parts)
	return " (" + strings.Join(parts, ", ") + ")"
}

func (s *Server) apiListApprovals(w http.ResponseWriter, r *http.Request) {
	list, err := s.listApprovals(r.Context())
	if err != nil {
		fail(w, 500, err.Error(), "")
		return
	}
	if list == nil {
		list = []*Approval{}
	}
	sort.SliceStable(list, func(i, j int) bool { return list[i].State == "pending" && list[j].State != "pending" })
	type view struct {
		ID          string `json:"id"`
		App         string `json:"app"`
		Kind        string `json:"kind"`
		Summary     string `json:"summary"`
		RequestedAt string `json:"requestedAt"`
		RequestedBy string `json:"requestedBy,omitempty"`
		State       string `json:"state"`
		Result      string `json:"result,omitempty"`
	}
	out := []view{}
	for _, ap := range list {
		out = append(out, view{ap.ID, ap.App, ap.Kind, ap.Summary, ap.RequestedAt, ap.RequestedBy, ap.State, ap.Result})
	}
	writeJSON(w, 200, map[string]any{"approvals": out, "approveUrl": "https://" + s.hosts().Auth() + "/approvals"})
}

func (s *Server) decide(ctx context.Context, id, decision string) (*Approval, error) {
	ap, err := s.getApproval(ctx, id)
	if err != nil {
		return nil, err
	}
	if ap.State != "pending" {
		return ap, fmt.Errorf("approval %s is already %s", id, ap.State)
	}
	switch decision {
	case "approve":
		return ap, s.executeApproval(ctx, ap)
	case "deny":
		ap.State = "denied"
		return ap, s.saveApproval(ctx, ap)
	}
	return ap, errors.New("decision must be approve or deny")
}

func (s *Server) apiDecide(w http.ResponseWriter, r *http.Request) {
	if scopeOf(r) != ScopeAdmin {
		fail(w, 403, "approvals are made by a human in the dashboard", "ask the user to approve at https://"+s.hosts().Auth()+"/approvals")
		return
	}
	ap, err := s.decide(r.Context(), r.PathValue("id"), r.PathValue("decision"))
	if err != nil {
		fail(w, 400, err.Error(), "")
		return
	}
	writeJSON(w, 200, ap)
}

func (s *Server) apiGetApproval(w http.ResponseWriter, r *http.Request) {
	ap, err := s.getApproval(r.Context(), r.PathValue("id"))
	if kube.IsNotFound(err) {
		fail(w, 404, "no approval "+r.PathValue("id"), "list with: sb approvals")
		return
	}
	if err != nil {
		fail(w, 500, err.Error(), "")
		return
	}
	writeJSON(w, 200, map[string]any{"id": ap.ID, "app": ap.App, "state": ap.State, "result": ap.Result, "summary": ap.Summary})
}

func containsStr(xs []string, s string) bool {
	for _, x := range xs {
		if x == s {
			return true
		}
	}
	return false
}

func templateGenerates(a *apps.App, k string) bool {
	for _, g := range a.TemplateDef.Secrets.Generate {
		if g.Name == k {
			return true
		}
	}
	return false
}

// catalogEntry is what agents see of a template: what it is and how to operate it.
type catalogEntry struct {
	Name        string            `json:"name"`
	Version     string            `json:"version"`
	Description string            `json:"description"`
	Access      string            `json:"access"`
	Components  []string          `json:"components"`
	Database    bool              `json:"database,omitempty"`
	Settings    []settingView     `json:"settings,omitempty"`
	Actions     []actionView      `json:"actions,omitempty"`
	Setup       []apps.SetupStep  `json:"setup,omitempty"`
	Secrets     []string          `json:"secrets,omitempty"`
	Notes       string            `json:"notes,omitempty"`
	Upstream    apps.Upstream     `json:"upstream"`
	Extra       map[string]string `json:"-"`
}

type settingView struct {
	Name        string   `json:"name"`
	Description string   `json:"description"`
	Type        string   `json:"type"`
	Values      []string `json:"values,omitempty"`
	Default     string   `json:"default"`
	ApprovalFor []string `json:"approvalFor,omitempty"`
}

type actionView struct {
	Name        string                `json:"name"`
	Description string                `json:"description"`
	Input       map[string]apps.Field `json:"input,omitempty"`
	Output      map[string]apps.Field `json:"output,omitempty"`
	On          string                `json:"on,omitempty"`
	Approval    bool                  `json:"approval,omitempty"`
	HumanOnly   bool                  `json:"humanOnly,omitempty"` // has secret outputs: run it in the dashboard
}

func actionViews(t *apps.Template) []actionView {
	var out []actionView
	for _, a := range t.Actions {
		out = append(out, actionView{Name: a.Name, Description: a.Description, Input: a.Input, Output: a.Output, On: a.On, Approval: a.Approval, HumanOnly: a.HasSecretOutput()})
	}
	return out
}

func catalogView(t *apps.Template) catalogEntry {
	e := catalogEntry{Name: t.Name, Version: t.Version, Description: t.Description, Access: t.Access.Mode,
		Database: t.Database != nil, Setup: t.Setup, Notes: t.Notes, Upstream: t.Upstream, Actions: actionViews(t)}
	for _, c := range t.Components {
		e.Components = append(e.Components, c.Name)
	}
	for _, s := range t.Settings {
		e.Settings = append(e.Settings, settingView{s.Name, s.Description, s.Type, s.Values, string(s.Default), s.ApprovalFor})
	}
	for _, g := range t.Secrets.Generate {
		e.Secrets = append(e.Secrets, g.Name+" (generated)")
	}
	for _, p := range t.Secrets.Provide {
		e.Secrets = append(e.Secrets, p.Name+" (entered by a human in the dashboard)")
	}
	return e
}
