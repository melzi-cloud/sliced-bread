package control

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"strings"
	"time"
)

var titleRE = regexp.MustCompile(`(?is)<title[^>]*>(.*?)</title>`)

// CheckResult is what `sb check` reports.
type CheckResult struct {
	App         string `json:"app"`
	Path        string `json:"path"`
	Status      int    `json:"status"`
	ContentType string `json:"contentType"`
	Bytes       int    `json:"bytes"`
	Title       string `json:"title,omitempty"`
	Snippet     string `json:"snippet"`
	Contains    *bool  `json:"contains,omitempty"`
	Note        string `json:"note"`
}

// apiCheck fetches a path from the app inside the cluster, behind the login wall,
// so an agent can verify a private app actually serves what it expects.
func (s *Server) apiCheck(w http.ResponseWriter, r *http.Request) {
	a := s.loadApp(w, r)
	if a == nil {
		return
	}
	path := r.URL.Query().Get("path")
	if path == "" {
		path = "/"
	}
	if !strings.HasPrefix(path, "/") || strings.ContainsAny(path, " \r\n") {
		fail(w, 400, "path must start with /", "")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 15*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, internalURL(a)+path, nil)
	req.Host = s.hosts().App(a.Name)
	req.Header.Set("Accept", "text/html,*/*")
	req.Header.Set("X-Sb-User", "sb-check")
	// A fresh connection per check: a kept-alive connection would stay pinned to a pod
	// that is shutting down after a rollout and keep returning the old version.
	client := &http.Client{
		Timeout:       15 * time.Second,
		Transport:     &http.Transport{DisableKeepAlives: true},
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
	resp, err := client.Do(req)
	if err != nil {
		fail(w, 502, "app did not respond: "+err.Error(), "check state with: sb status "+a.Name+" and logs with: sb logs "+a.Name)
		return
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 512<<10))
	res := CheckResult{App: a.Name, Path: path, Status: resp.StatusCode, ContentType: resp.Header.Get("Content-Type"), Bytes: len(body),
		Note: "fetched inside the cluster, past the login wall; this is what a signed-in user's browser receives"}
	if m := titleRE.FindSubmatch(body); m != nil {
		res.Title = strings.TrimSpace(string(m[1]))
	}
	snip := string(body)
	if len(snip) > 1500 {
		snip = snip[:1500] + "..."
	}
	res.Snippet = snip
	if want := r.URL.Query().Get("contains"); want != "" {
		ok := strings.Contains(string(body), want)
		res.Contains = &ok
	}
	if resp.StatusCode >= 300 && resp.StatusCode < 400 {
		res.Note += fmt.Sprintf("; redirect to %s", resp.Header.Get("Location"))
	}
	writeJSON(w, 200, res)
}
