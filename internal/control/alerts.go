package control

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"time"

	"github.com/melzi-cloud/sliced-bread/internal/apps"
	"github.com/melzi-cloud/sliced-bread/internal/kube"
)

// Alerts are conditions that need a human: failed backups and restore tests, apps that
// stay down, certificates that don't renew, nodes that are gone. sb-control evaluates
// them every 30 seconds, shows open alerts on the dashboard and in `sb alerts`, and
// posts to a webhook when one opens and when it clears.

const (
	alertsConfigMap  = "sb-alerts"
	alertsWebhook    = "sb-alerts-webhook"
	alertsEvalEvery  = 30 * time.Second
	appDownGrace     = 15 * time.Minute
	nodeDownGrace    = 5 * time.Minute
	certGrace        = 30 * time.Minute
	firstBackupGrace = 3 * time.Hour
	backupMaxAge     = 26 * time.Hour
	certRenewWarning = 14 * 24 * time.Hour
)

// Alert is one open problem.
type Alert struct {
	ID       string `json:"id"`
	Severity string `json:"severity"` // critical or warning
	App      string `json:"app,omitempty"`
	Title    string `json:"title"`
	Detail   string `json:"detail,omitempty"`
	Hint     string `json:"hint,omitempty"`
	Since    string `json:"since"`
}

// condition is a candidate alert; it opens once it has held for Grace.
type condition struct {
	Alert
	Grace time.Duration
}

type alertEntry struct {
	FirstSeen time.Time `json:"firstSeen"`
	Open      bool      `json:"open"`
	Notified  bool      `json:"notified"`
	Alert     Alert     `json:"alert"`
}

type alertState struct {
	Entries map[string]*alertEntry `json:"entries"`
	// Resolved alerts whose "cleared" message has not been delivered yet.
	Resolved []Alert `json:"resolved,omitempty"`
}

// WebhookConfig is where alerts are posted.
type WebhookConfig struct {
	URL    string `json:"url"`
	Format string `json:"format"` // json, slack, discord, ntfy
}

var webhookFormats = map[string]bool{"json": true, "slack": true, "discord": true, "ntfy": true}

func (c WebhookConfig) validate() error {
	u, err := url.Parse(c.URL)
	if err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Host == "" {
		return errors.New("webhook must be an http(s) URL")
	}
	if !webhookFormats[c.Format] {
		return errors.New("format must be json, slack, discord or ntfy")
	}
	return nil
}

// --- conditions ---

// alertConditions computes current problems from apps, nodes and certificates.
func (s *Server) alertConditions(ctx context.Context, list []*apps.App, bc *BackupConfig, now time.Time) []condition {
	var out []condition
	add := func(c condition) { out = append(out, c) }
	parse := func(ts string) time.Time { t, _ := time.Parse(time.RFC3339, ts); return t }
	hasData := false
	for _, a := range list {
		st := s.status(ctx, a)
		switch st.State {
		case "running", "paused":
		default:
			add(condition{Alert{ID: "app-down/" + a.Name, Severity: "critical", App: a.Name,
				Title:  a.Name + " is not running (" + st.State + ")",
				Detail: st.Message, Hint: "sb status " + a.Name + " and sb logs " + a.Name}, appDownGrace})
		}
		if !a.HasData() {
			continue
		}
		hasData = true
		b := a.Backup
		if b != nil && b.LastBackup != nil && b.LastBackup.Phase == "failed" {
			add(condition{Alert{ID: "backup-failed/" + a.Name, Severity: "critical", App: a.Name,
				Title: "backup of " + a.Name + " failed", Detail: b.LastBackup.Message, Hint: "sb backups " + a.Name}, 0})
		}
		if b != nil && b.LastTest != nil && b.LastTest.Phase == "failed" {
			add(condition{Alert{ID: "restore-test-failed/" + a.Name, Severity: "critical", App: a.Name,
				Title:  "restore test of " + a.Name + " failed (snapshot " + short(b.LastTest.Snapshot) + ")",
				Detail: b.LastTest.Message, Hint: "sb backups " + a.Name + "; a new backup runs a new test: sb backup " + a.Name + " --wait"}, 0})
		}
		if b != nil && b.LastRestore != nil && b.LastRestore.Phase == "failed" {
			add(condition{Alert{ID: "restore-failed/" + a.Name, Severity: "critical", App: a.Name,
				Title: "restore of " + a.Name + " failed", Detail: b.LastRestore.Message, Hint: "sb backups " + a.Name}, 0})
		}
		if bc != nil {
			if b == nil || len(b.History) == 0 {
				add(condition{Alert{ID: "backup-overdue/" + a.Name, Severity: "warning", App: a.Name,
					Title: a.Name + " has no successful backup yet", Hint: "sb backup " + a.Name + " --wait"}, firstBackupGrace})
			} else if last := parse(b.History[0].StartedAt); now.Sub(last) > backupMaxAge {
				add(condition{Alert{ID: "backup-overdue/" + a.Name, Severity: "warning", App: a.Name,
					Title: a.Name + " has not been backed up since " + b.History[0].StartedAt, Hint: "sb backups " + a.Name}, 0})
			}
		}
	}
	if hasData && bc == nil {
		add(condition{Alert{ID: "backups-not-configured", Severity: "warning",
			Title: "backups are not configured, so app data is not backed up", Hint: "an admin runs: sb backups configure"}, time.Hour})
	}
	if nodes, err := s.kube.List(ctx, "v1", "Node", "", ""); err == nil {
		for _, n := range nodes {
			if ready, why := conditionStatus(n, "Ready"); ready != "True" {
				name := kube.Str(n, "metadata", "name")
				add(condition{Alert{ID: "node-down/" + name, Severity: "critical",
					Title: "server " + name + " is not ready", Detail: why}, nodeDownGrace})
			}
		}
	}
	if certs, err := s.kube.List(ctx, "cert-manager.io/v1", "Certificate", "", ""); err == nil {
		for _, c := range certs {
			name := kube.Str(c, "metadata", "namespace") + "/" + kube.Str(c, "metadata", "name")
			ready, why := conditionStatus(c, "Ready")
			notAfter := parse(kube.Str(c, "status", "notAfter"))
			switch {
			case ready != "True":
				add(condition{Alert{ID: "certificate/" + name, Severity: "critical",
					Title: "certificate " + name + " is not ready", Detail: why}, certGrace})
			case !notAfter.IsZero() && notAfter.Sub(now) < certRenewWarning:
				add(condition{Alert{ID: "certificate/" + name, Severity: "critical",
					Title: "certificate " + name + " expires " + notAfter.UTC().Format(time.RFC3339) + " and has not renewed"}, 0})
			}
		}
	}
	return out
}

// conditionStatus returns status and message of a named status condition.
func conditionStatus(o kube.Obj, typ string) (string, string) {
	conds, _ := o["status"].(map[string]any)["conditions"].([]any)
	for _, c := range conds {
		m, _ := c.(map[string]any)
		if t, _ := m["type"].(string); t == typ {
			st, _ := m["status"].(string)
			msg, _ := m["message"].(string)
			return st, msg
		}
	}
	return "Unknown", "no " + typ + " condition"
}

// --- state machine ---

// updateAlerts merges current conditions into the stored state. It returns alerts that
// just opened and alerts that just cleared (only those that had opened).
func updateAlerts(st *alertState, conds []condition, now time.Time) (opened, cleared []Alert) {
	if st.Entries == nil {
		st.Entries = map[string]*alertEntry{}
	}
	seen := map[string]bool{}
	for _, c := range conds {
		seen[c.ID] = true
		e := st.Entries[c.ID]
		if e == nil {
			e = &alertEntry{FirstSeen: now}
			st.Entries[c.ID] = e
		}
		since := e.FirstSeen
		e.Alert = c.Alert
		e.Alert.Since = since.UTC().Format(time.RFC3339)
		if !e.Open && now.Sub(e.FirstSeen) >= c.Grace {
			e.Open = true
			opened = append(opened, e.Alert)
		}
	}
	for id, e := range st.Entries {
		if seen[id] {
			continue
		}
		if e.Open && e.Notified {
			cleared = append(cleared, e.Alert)
		}
		delete(st.Entries, id)
	}
	return opened, cleared
}

// openAlerts lists open alerts, critical first.
func (st *alertState) openAlerts() []Alert {
	var out []Alert
	for _, e := range st.Entries {
		if e.Open {
			out = append(out, e.Alert)
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Severity != out[j].Severity {
			return out[i].Severity == "critical"
		}
		return out[i].ID < out[j].ID
	})
	return out
}

func (s *Server) loadAlertState(ctx context.Context) *alertState {
	st := &alertState{Entries: map[string]*alertEntry{}}
	var cm kube.Obj
	if err := s.kube.Get(ctx, "v1", "ConfigMap", s.cfg.Namespace, alertsConfigMap, &cm); err == nil {
		json.Unmarshal([]byte(kube.Str(cm, "data", "state")), st)
	}
	if st.Entries == nil {
		st.Entries = map[string]*alertEntry{}
	}
	return st
}

func (s *Server) saveAlertState(ctx context.Context, st *alertState) error {
	b, _ := json.Marshal(st)
	return s.kube.Apply(ctx, kube.Obj{
		"apiVersion": "v1", "kind": "ConfigMap",
		"metadata": kube.Obj{"name": alertsConfigMap, "namespace": s.cfg.Namespace, "labels": kube.Obj{apps.ManagedByLabel: "slicedbread"}},
		"data":     kube.Obj{"state": string(b)},
	})
}

// evaluateAlerts runs the alert cycle; called from the tick.
func (s *Server) evaluateAlerts(ctx context.Context, list []*apps.App, bc *BackupConfig) {
	if time.Since(s.alertsAt) < alertsEvalEvery {
		return
	}
	s.alertsAt = time.Now()
	st := s.loadAlertState(ctx)
	opened, cleared := updateAlerts(st, s.alertConditions(ctx, list, bc, time.Now()), time.Now())
	for _, a := range opened {
		s.logf("alert opened: %s", a.Title)
	}
	st.Resolved = append(st.Resolved, cleared...)
	if hook, err := s.webhookConfig(ctx); err == nil && hook != nil {
		for _, e := range st.Entries {
			if e.Open && !e.Notified {
				if err := s.sendWebhook(ctx, hook, "opened", e.Alert); err != nil {
					s.logf("alert webhook: %v", err)
					break
				}
				e.Notified = true
			}
		}
		var left []Alert
		for _, a := range st.Resolved {
			if err := s.sendWebhook(ctx, hook, "resolved", a); err != nil {
				left = append(left, a)
			}
		}
		st.Resolved = left
	} else {
		st.Resolved = nil
	}
	s.alertsCache = st.openAlerts()
	if err := s.saveAlertState(ctx, st); err != nil {
		s.logf("save alerts: %v", err)
	}
}

// --- webhook ---

func (s *Server) webhookConfig(ctx context.Context) (*WebhookConfig, error) {
	var sec kube.Obj
	if err := s.kube.Get(ctx, "v1", "Secret", s.cfg.Namespace, alertsWebhook, &sec); err != nil {
		if kube.IsNotFound(err) {
			return nil, nil
		}
		return nil, err
	}
	d := func(k string) string {
		b, _ := base64.StdEncoding.DecodeString(kube.Str(sec, "data", k))
		return string(b)
	}
	return &WebhookConfig{URL: d("url"), Format: d("format")}, nil
}

var webhookHTTP = &http.Client{Timeout: 10 * time.Second}

// webhookRequest builds the HTTP request for one event in the configured format.
func webhookRequest(ctx context.Context, c *WebhookConfig, domain, event string, a Alert) (*http.Request, error) {
	verb := map[string]string{"opened": "ALERT", "resolved": "RESOLVED", "test": "TEST"}[event]
	text := fmt.Sprintf("[%s] %s: %s", verb, domain, a.Title)
	if a.Detail != "" && event != "resolved" {
		text += "\n" + a.Detail
	}
	if a.Hint != "" && event == "opened" {
		text += "\nNext: " + a.Hint
	}
	var body []byte
	ctype := "application/json"
	switch c.Format {
	case "slack":
		body, _ = json.Marshal(map[string]string{"text": text})
	case "discord":
		body, _ = json.Marshal(map[string]string{"content": text})
	case "ntfy":
		body, ctype = []byte(text), "text/plain; charset=utf-8"
	default:
		body, _ = json.Marshal(map[string]any{"event": event, "install": domain, "alert": a, "text": text})
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.URL, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", ctype)
	req.Header.Set("User-Agent", "SlicedBread-alerts")
	if c.Format == "ntfy" {
		req.Header.Set("Title", "SlicedBread "+strings.ToLower(verb)+": "+domain)
		if event == "opened" && a.Severity == "critical" {
			req.Header.Set("Priority", "high")
		}
		req.Header.Set("Tags", map[string]string{"opened": "warning", "resolved": "white_check_mark", "test": "bell"}[event])
	}
	return req, nil
}

func (s *Server) sendWebhook(ctx context.Context, c *WebhookConfig, event string, a Alert) error {
	req, err := webhookRequest(ctx, c, s.cfg.Domain, event, a)
	if err != nil {
		return err
	}
	resp, err := webhookHTTP.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<16))
	if resp.StatusCode >= 300 {
		return fmt.Errorf("webhook answered HTTP %d", resp.StatusCode)
	}
	return nil
}

// --- API ---

func (s *Server) apiAlerts(w http.ResponseWriter, r *http.Request) {
	st := s.loadAlertState(r.Context())
	resp := map[string]any{"alerts": st.openAlerts()}
	if hook, err := s.webhookConfig(r.Context()); err == nil && hook != nil {
		if u, err := url.Parse(hook.URL); err == nil {
			resp["webhook"] = hook.Format + " to " + u.Host
		}
	} else {
		resp["webhook"] = "not configured (dashboard and sb alerts only)"
	}
	writeJSON(w, 200, resp)
}

func (s *Server) apiAlertsWebhook(w http.ResponseWriter, r *http.Request) {
	if scopeOf(r) != ScopeAdmin {
		fail(w, 403, "configuring alerts needs an admin token", "")
		return
	}
	var c WebhookConfig
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<14)).Decode(&c); err != nil {
		fail(w, 400, "bad JSON: "+err.Error(), "")
		return
	}
	ctx := r.Context()
	if c.URL == "" {
		if err := s.kube.Delete(ctx, "v1", "Secret", s.cfg.Namespace, alertsWebhook); err != nil && !kube.IsNotFound(err) {
			fail(w, 500, err.Error(), "")
			return
		}
		writeJSON(w, 200, map[string]any{"webhook": "off"})
		return
	}
	if c.Format == "" {
		c.Format = "json"
	}
	if err := c.validate(); err != nil {
		fail(w, 400, err.Error(), "")
		return
	}
	if err := s.kube.Apply(ctx, kube.Obj{
		"apiVersion": "v1", "kind": "Secret",
		"metadata": kube.Obj{"name": alertsWebhook, "namespace": s.cfg.Namespace, "labels": kube.Obj{apps.ManagedByLabel: "slicedbread"}},
		"type":     "Opaque", "stringData": kube.Obj{"url": c.URL, "format": c.Format},
	}); err != nil {
		fail(w, 500, err.Error(), "")
		return
	}
	writeJSON(w, 200, map[string]any{"webhook": c.Format, "hint": "send a test message with: sb alerts test"})
}

func (s *Server) apiAlertsTest(w http.ResponseWriter, r *http.Request) {
	if scopeOf(r) != ScopeAdmin {
		fail(w, 403, "needs an admin token", "")
		return
	}
	hook, err := s.webhookConfig(r.Context())
	if err != nil || hook == nil {
		fail(w, 400, "no webhook configured", "sb alerts configure --webhook <url>")
		return
	}
	a := Alert{ID: "test", Severity: "warning", Title: "test message from SlicedBread; alerts will arrive like this", Since: now()}
	if err := s.sendWebhook(r.Context(), hook, "test", a); err != nil {
		fail(w, 502, "webhook delivery failed: "+err.Error(), "")
		return
	}
	writeJSON(w, 200, map[string]any{"sent": true})
}
