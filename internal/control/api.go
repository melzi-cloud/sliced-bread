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
	m.HandleFunc("GET /v1/approvals", s.apiListApprovals)
	m.HandleFunc("POST /v1/approvals/{id}/{decision}", s.apiDecide)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
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
		"scope": scopeOf(r), "domain": s.cfg.Domain,
		"dashboard": "https://auth." + s.cfg.Domain, "approvals": "https://auth." + s.cfg.Domain + "/approvals",
		"appUrlPattern": "https://<app>." + s.cfg.Domain,
	})
}

func (s *Server) apiCatalog(w http.ResponseWriter, r *http.Request) {
	var out []*apps.Template
	for _, n := range apps.TemplateNames() {
		out = append(out, apps.Catalog[n])
	}
	writeJSON(w, 200, map[string]any{"templates": out})
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
	Name        string            `json:"name"`
	Template    string            `json:"template,omitempty"`
	Image       string            `json:"image,omitempty"`
	Repo        *apps.Repo        `json:"repo,omitempty"`
	Port        int               `json:"port,omitempty"`
	Env         map[string]string `json:"env,omitempty"`
	Secrets     map[string]string `json:"secrets,omitempty"`
	Access      string            `json:"access,omitempty"`
	PublicPaths []string          `json:"publicPaths,omitempty"`
	Settings    map[string]string `json:"settings,omitempty"`
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
	templateAccess := ""
	if t := apps.Catalog[next.Template]; t != nil {
		templateAccess = t.Access
	}
	if next.Access != prevAccess && next.Access != apps.AccessWall && next.Access != templateAccess {
		reasons = append(reasons, fmt.Sprintf("access %s -> %s", prevAccess, next.Access))
	}
	if prev != nil && len(next.PublicPaths) > len(prev.PublicPaths) && next.Template == "" {
		reasons = append(reasons, fmt.Sprintf("public paths %v", next.PublicPaths))
	}
	if prev == nil && len(next.PublicPaths) > 0 && next.Template == "" {
		reasons = append(reasons, fmt.Sprintf("public paths %v", next.PublicPaths))
	}
	if t := apps.Catalog[next.Template]; t != nil {
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
	b, _ := json.Marshal(reasons)
	return string(b)
}

func (s *Server) approvalResponse(w http.ResponseWriter, ap *Approval) {
	writeJSON(w, http.StatusAccepted, map[string]any{
		"approval":   ap.ID,
		"state":      "pending",
		"summary":    ap.Summary,
		"approveUrl": "https://auth." + s.cfg.Domain + "/approvals",
		"hint":       "A human must approve this at the approveUrl (or run `sb approve " + ap.ID + "` with an admin token). Check with: sb approvals",
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
		Env: req.Env, Settings: req.Settings, PublicPaths: req.PublicPaths, Access: apps.AccessWall,
	}
	if a.Template != "" {
		if a.Image != "" || a.Repo != nil {
			fail(w, 400, "use either a template, an image or a repo, not several", "")
			return
		}
		if err := apps.ApplyTemplate(a); err != nil {
			fail(w, 400, err.Error(), "see templates with: sb catalog")
			return
		}
	}
	if req.Access != "" {
		a.Access = req.Access
	}
	for k := range req.Secrets {
		if _, plain := a.Env[k]; plain {
			fail(w, 400, "key "+k+" is set both as env and secret", "")
			return
		}
		a.AddSecretKey(k)
	}
	if err := a.Validate(); err != nil {
		fail(w, 400, err.Error(), "")
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	ctx := r.Context()
	if _, err := s.getApp(ctx, a.Name); err == nil {
		fail(w, 409, "app "+a.Name+" already exists", "change it with sb env / sb settings / sb redeploy, or pick another name")
		return
	}
	if why := needsApproval(scopeOf(r), nil, a); why != "" {
		if len(req.Secrets) > 0 {
			fail(w, 400, "this deploy needs human approval, so secrets cannot be included", "deploy without secrets, then set them with: sb env set --secret after approval")
			return
		}
		ap, err := s.requestApproval(ctx, "create", a.Name, "create app "+a.Name+": "+why, a)
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
	if len(req.Secrets) > 0 {
		if err := s.setSecrets(ctx, a, req.Secrets, nil); err != nil {
			fail(w, 500, err.Error(), "")
			return
		}
		cur, _ := s.getApp(ctx, a.Name)
		if cur != nil {
			s.reconcile(ctx, cur)
		}
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
		ap, err := s.requestApproval(r.Context(), "delete", a.Name, "delete app "+a.Name+" and all its data", nil)
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
	Set     map[string]string `json:"set,omitempty"`
	Secrets map[string]string `json:"secrets,omitempty"`
	Unset   []string          `json:"unset,omitempty"`
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
	if t := apps.Catalog[a.Template]; t != nil {
		for _, k := range t.ForbidEnv {
			_, inSet := req.Set[k]
			_, inSec := req.Secrets[k]
			if inSet || inSec {
				fail(w, 400, "env "+k+" cannot be changed on "+t.Name+" apps", "use: sb settings")
				return
			}
		}
	}
	if a.Env == nil {
		a.Env = map[string]string{}
	}
	for k, v := range req.Set {
		a.Env[k] = v
		a.RemoveSecretKey(k)
	}
	for k := range req.Secrets {
		delete(a.Env, k)
		a.AddSecretKey(k)
	}
	for _, k := range req.Unset {
		delete(a.Env, k)
		a.RemoveSecretKey(k)
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
	if err := s.setSecrets(ctx, a, req.Secrets, req.Unset); err != nil {
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
		ap, err := s.requestApproval(ctx, "update", next.Name, summary+": "+why, next)
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
	s.mu.Lock()
	defer s.mu.Unlock()
	a := s.loadApp(w, r)
	if a == nil {
		return
	}
	ctx := r.Context()
	if a.Repo != nil {
		if a.Build != nil && a.Build.Phase == "running" {
			fail(w, 409, "a build is already running", "follow it with: sb logs "+a.Name+" --build")
			return
		}
		if err := s.startBuild(ctx, a); err != nil {
			fail(w, 500, err.Error(), "")
			return
		}
		writeJSON(w, 202, map[string]any{"build": a.Build, "hint": "Rebuilding from the latest commit. Follow with: sb logs " + a.Name + " --build, then: sb wait " + a.Name})
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
	pod, err := s.appPod(ctx, a)
	if err != nil {
		io.WriteString(w, "no running pod yet: "+err.Error()+"\n")
		return
	}
	l, err := s.kube.Logs(ctx, apps.Namespace(a.Name), pod, "app", tail)
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
	t := apps.Catalog[a.Template]
	var act *apps.Action
	if t != nil {
		act = t.Action(r.PathValue("action"))
	}
	if act == nil {
		fail(w, 404, fmt.Sprintf("app %s has no action %q", a.Name, r.PathValue("action")), "list actions with: sb status "+a.Name)
		return
	}
	if act.AdminOnly && scopeOf(r) != ScopeAdmin {
		fail(w, 403, "action "+act.Name+" is for humans (admin token only)", "ask the user to run: sb action "+a.Name+" "+act.Name)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 60*time.Second)
	defer cancel()
	out, err := s.runAction(ctx, a, act.Name, req.Input)
	if err != nil {
		fail(w, 400, err.Error(), "")
		return
	}
	writeJSON(w, 200, out)
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
		ID, App, Kind, Summary, RequestedAt, State, Result string
	}
	out := []view{}
	for _, ap := range list {
		out = append(out, view{ap.ID, ap.App, ap.Kind, ap.Summary, ap.RequestedAt, ap.State, ap.Result})
	}
	writeJSON(w, 200, map[string]any{"approvals": out, "approveUrl": "https://auth." + s.cfg.Domain + "/approvals"})
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
		fail(w, 403, "only a human with an admin token can approve", "ask the user to approve at https://auth."+s.cfg.Domain+"/approvals")
		return
	}
	ap, err := s.decide(r.Context(), r.PathValue("id"), r.PathValue("decision"))
	if err != nil {
		fail(w, 400, err.Error(), "")
		return
	}
	writeJSON(w, 200, ap)
}
