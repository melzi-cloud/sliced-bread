package control

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/melzi-cloud/sliced-bread/internal/apps"
)

var hookHTTP = &http.Client{Timeout: 20 * time.Second}

func internalURL(a *apps.App) string {
	return "http://web." + apps.Namespace(a.Name) + ".svc.cluster.local"
}

func postJSON(ctx context.Context, url, bearer string, in, out any) (int, error) {
	b, _ := json.Marshal(in)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(b))
	if err != nil {
		return 0, err
	}
	req.Header.Set("Content-Type", "application/json")
	if bearer != "" {
		req.Header.Set("Authorization", "Bearer "+bearer)
	}
	resp, err := hookHTTP.Do(req)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode >= 300 {
		return resp.StatusCode, fmt.Errorf("HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}
	if out != nil {
		if err := json.Unmarshal(body, out); err != nil {
			return resp.StatusCode, err
		}
	}
	return resp.StatusCode, nil
}

type umamiLogin struct {
	Token string `json:"token"`
	User  struct {
		ID string `json:"id"`
	} `json:"user"`
}

func umamiLoginAs(ctx context.Context, a *apps.App, password string) (umamiLogin, error) {
	var out umamiLogin
	_, err := postJSON(ctx, internalURL(a)+"/api/auth/login", "", map[string]string{"username": "admin", "password": password}, &out)
	if err == nil && out.Token == "" {
		err = fmt.Errorf("login returned no token")
	}
	return out, err
}

// signupWindow is how long Vaultwarden signups stay open.
const signupWindow = 15 * time.Minute

// vaultwardenHooks closes signups automatically after the window.
func (s *Server) vaultwardenHooks(ctx context.Context, a *apps.App) bool {
	if a.Settings["signups"] != "true" {
		if a.Hooks["signups-opened-at"] != "" {
			delete(a.Hooks, "signups-opened-at")
			return true
		}
		return false
	}
	opened, err := time.Parse(time.RFC3339, a.Hooks["signups-opened-at"])
	if err != nil {
		return markHook(a, "signups-opened-at", time.Now().UTC().Format(time.RFC3339))
	}
	if time.Since(opened) < signupWindow {
		return false
	}
	a.Settings["signups"] = "false"
	if err := apps.ApplyTemplate(a); err != nil {
		s.logf("vaultwarden %s: close signups: %v", a.Name, err)
		return false
	}
	delete(a.Hooks, "signups-opened-at")
	if err := s.reconcile(ctx, a); err != nil {
		s.logf("vaultwarden %s: reconcile after closing signups: %v", a.Name, err)
	}
	s.logf("vaultwarden %s: signups closed automatically after %s", a.Name, signupWindow)
	return true
}

// runHooks runs template lifecycle hooks once the app is running. Returns true if the app changed.
func (s *Server) runHooks(ctx context.Context, a *apps.App) bool {
	if a.Template == "vaultwarden" {
		return s.vaultwardenHooks(ctx, a)
	}
	if a.Template != "umami" || (a.Hooks != nil && a.Hooks["admin-password"] == "set") {
		return false
	}
	if st := s.status(ctx, a); st.State != "running" && st.State != "configuring" {
		return false
	}
	vals, err := s.appSecret(ctx, a)
	if err != nil || vals["UMAMI_ADMIN_PASSWORD"] == "" {
		return false
	}
	newPW := vals["UMAMI_ADMIN_PASSWORD"]
	if _, err := umamiLoginAs(ctx, a, newPW); err == nil {
		return markHook(a, "admin-password", "set")
	}
	login, err := umamiLoginAs(ctx, a, "umami") // Umami's documented default
	if err != nil {
		s.logf("umami %s: default login failed: %v", a.Name, err)
		return false
	}
	if _, err := postJSON(ctx, internalURL(a)+"/api/users/"+login.User.ID, login.Token, map[string]string{"password": newPW}, nil); err != nil {
		s.logf("umami %s: set admin password: %v", a.Name, err)
		return false
	}
	s.logf("umami %s: default admin password replaced", a.Name)
	return markHook(a, "admin-password", "set")
}

func markHook(a *apps.App, k, v string) bool {
	if a.Hooks == nil {
		a.Hooks = map[string]string{}
	}
	a.Hooks[k] = v
	return true
}

// runAction executes a template action.
func (s *Server) runAction(ctx context.Context, a *apps.App, action string, input map[string]string) (map[string]any, error) {
	switch a.Template + "/" + action {
	case "umami/create_website":
		if a.Hooks["admin-password"] != "set" {
			return nil, fmt.Errorf("umami is still starting (admin setup not finished); run `sb wait %s` and retry", a.Name)
		}
		name, domain := strings.TrimSpace(input["name"]), strings.TrimSpace(input["domain"])
		domain = strings.TrimPrefix(strings.TrimPrefix(domain, "https://"), "http://")
		domain = strings.TrimSuffix(domain, "/")
		if name == "" || domain == "" {
			return nil, fmt.Errorf("create_website needs input name and domain, e.g. --input name=Shop --input domain=shop.example.com")
		}
		vals, err := s.appSecret(ctx, a)
		if err != nil {
			return nil, err
		}
		login, err := umamiLoginAs(ctx, a, vals["UMAMI_ADMIN_PASSWORD"])
		if err != nil {
			return nil, fmt.Errorf("umami login: %w", err)
		}
		var site struct {
			ID     string `json:"id"`
			Name   string `json:"name"`
			Domain string `json:"domain"`
		}
		if _, err := postJSON(ctx, internalURL(a)+"/api/websites", login.Token, map[string]string{"name": name, "domain": domain}, &site); err != nil {
			return nil, fmt.Errorf("create website: %w", err)
		}
		script := "https://" + s.hosts().App(a.Name) + "/script.js"
		return map[string]any{
			"website_id": site.ID,
			"name":       site.Name,
			"domain":     site.Domain,
			"script_url": script,
			"snippet":    fmt.Sprintf(`<script defer src="%s" data-website-id="%s"></script>`, script, site.ID),
			"next":       "Add the snippet inside <head> of the site (for a Vite/React app: index.html), then redeploy that app.",
		}, nil
	case "umami/test_event":
		if a.Hooks["admin-password"] != "set" {
			return nil, fmt.Errorf("umami is still starting; run `sb wait %s` and retry", a.Name)
		}
		vals, err := s.appSecret(ctx, a)
		if err != nil {
			return nil, err
		}
		login, err := umamiLoginAs(ctx, a, vals["UMAMI_ADMIN_PASSWORD"])
		if err != nil {
			return nil, fmt.Errorf("umami login: %w", err)
		}
		var sites struct {
			Data []struct{ ID, Name, Domain string } `json:"data"`
		}
		if err := getJSON(ctx, internalURL(a)+"/api/websites", login.Token, &sites); err != nil {
			return nil, err
		}
		id := strings.TrimSpace(input["website_id"])
		host := ""
		for _, w := range sites.Data {
			if id == "" && len(sites.Data) == 1 || w.ID == id {
				id, host = w.ID, w.Domain
			}
		}
		if host == "" {
			return nil, fmt.Errorf("pass --input website_id=<id> (there are %d websites; see the stats action)", len(sites.Data))
		}
		count := func() float64 {
			var st map[string]any
			end := time.Now().Add(time.Minute)
			u := fmt.Sprintf("%s/api/websites/%s/stats?startAt=%d&endAt=%d", internalURL(a), id, end.Add(-24*time.Hour).UnixMilli(), end.UnixMilli())
			if getJSON(ctx, u, login.Token, &st) != nil {
				return -1
			}
			f, _ := statNum(st["pageviews"]).(float64)
			return f
		}
		before := count()
		body, _ := json.Marshal(map[string]any{"type": "event", "payload": map[string]any{
			"website": id, "hostname": host, "url": "/sb-test-event", "title": "SlicedBread test event", "language": "en-US", "screen": "1920x1080",
		}})
		req, _ := http.NewRequestWithContext(ctx, http.MethodPost, internalURL(a)+"/api/send", bytes.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("User-Agent", "Mozilla/5.0 (Macintosh; Intel Mac OS X 14_0) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/130.0 Safari/537.36 SlicedBread-test")
		resp, err := hookHTTP.Do(req)
		if err != nil {
			return nil, fmt.Errorf("send event: %w", err)
		}
		resp.Body.Close()
		if resp.StatusCode >= 300 {
			return nil, fmt.Errorf("umami rejected the event: HTTP %d", resp.StatusCode)
		}
		after := before
		for i := 0; i < 10 && after <= before; i++ {
			time.Sleep(time.Second)
			after = count()
		}
		return map[string]any{"website_id": id, "domain": host, "pageviews_before": before, "pageviews_after": after,
			"counted": after > before, "note": "one page view at /sb-test-event was added to this website's stats"}, nil
	case "umami/stats":
		if a.Hooks["admin-password"] != "set" {
			return nil, fmt.Errorf("umami is still starting; run `sb wait %s` and retry", a.Name)
		}
		hours := 24
		if h := input["hours"]; h != "" {
			fmt.Sscanf(h, "%d", &hours)
			if hours <= 0 || hours > 24*90 {
				hours = 24
			}
		}
		vals, err := s.appSecret(ctx, a)
		if err != nil {
			return nil, err
		}
		login, err := umamiLoginAs(ctx, a, vals["UMAMI_ADMIN_PASSWORD"])
		if err != nil {
			return nil, fmt.Errorf("umami login: %w", err)
		}
		var sites struct {
			Data []struct{ ID, Name, Domain string } `json:"data"`
		}
		if err := getJSON(ctx, internalURL(a)+"/api/websites", login.Token, &sites); err != nil {
			return nil, err
		}
		end := time.Now()
		start := end.Add(-time.Duration(hours) * time.Hour)
		var out []map[string]any
		for _, w := range sites.Data {
			var st map[string]any
			url := fmt.Sprintf("%s/api/websites/%s/stats?startAt=%d&endAt=%d", internalURL(a), w.ID, start.UnixMilli(), end.UnixMilli())
			if err := getJSON(ctx, url, login.Token, &st); err != nil {
				out = append(out, map[string]any{"name": w.Name, "domain": w.Domain, "website_id": w.ID, "error": err.Error()})
				continue
			}
			out = append(out, map[string]any{"name": w.Name, "domain": w.Domain, "website_id": w.ID,
				"pageviews": statNum(st["pageviews"]), "visitors": statNum(st["visitors"]), "visits": statNum(st["visits"])})
		}
		return map[string]any{"hours": hours, "websites": out}, nil
	case "umami/dashboard_login":
		vals, err := s.appSecret(ctx, a)
		if err != nil {
			return nil, err
		}
		pw := vals["UMAMI_ADMIN_PASSWORD"]
		if a.Hooks["admin-password"] != "set" {
			pw = "umami (default, will be replaced once the app is running)"
		}
		return map[string]any{"url": "https://" + s.hosts().App(a.Name), "username": "admin", "password": pw}, nil
	}
	return nil, fmt.Errorf("app %s (template %q) has no action %q", a.Name, a.Template, action)
}

func getJSON(ctx context.Context, url, bearer string, out any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+bearer)
	resp, err := hookHTTP.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode >= 300 {
		return fmt.Errorf("HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}
	return json.Unmarshal(body, out)
}

// statNum accepts Umami's stat values, which are numbers or {"value": n} depending on version.
func statNum(v any) any {
	if m, ok := v.(map[string]any); ok {
		return m["value"]
	}
	return v
}
