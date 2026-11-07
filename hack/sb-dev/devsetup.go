package main

import (
	"context"
	"encoding/json"
	"fmt"
	"html"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/melzi-cloud/sliced-bread/internal/tenant"
)

var (
	csrfRE  = regexp.MustCompile(`name="csrf" value="([^"]+)"`)
	tokenRE = regexp.MustCompile(`New admin token \(shown once\): <code>(sbt_[A-Za-z0-9_-]+)</code>`)
)

// devSetup completes the browser setup of a development tenant the way a human would:
// opens the setup link, creates the owner account, signs in and creates a short-lived admin
// token in the dashboard. It writes admin-login.json and cli-admin.json for test scripts.
func devSetup(ctx context.Context, p *tenant.Provisioner, st *tenant.State) error {
	link, err := p.SetupCode(ctx, st)
	if err != nil {
		return err
	}
	jar, _ := cookiejar.New(nil)
	hc := &http.Client{Jar: jar, Timeout: 30 * time.Second}
	get := func(u string) (string, error) {
		var last error
		for i := 0; i < 30; i++ {
			resp, err := hc.Get(u)
			if err == nil {
				b, _ := io.ReadAll(resp.Body)
				resp.Body.Close()
				if resp.StatusCode == 200 {
					return string(b), nil
				}
				err = fmt.Errorf("GET %s: HTTP %d", u, resp.StatusCode)
			}
			last = err
			time.Sleep(5 * time.Second)
		}
		return "", last
	}
	post := func(u string, form url.Values) (string, error) {
		resp, err := hc.PostForm(u, form)
		if err != nil {
			return "", err
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		if resp.StatusCode != 200 {
			return "", fmt.Errorf("POST %s: HTTP %d", u, resp.StatusCode)
		}
		return string(b), nil
	}
	csrf := func(page string) string {
		if m := csrfRE.FindStringSubmatch(page); m != nil {
			return html.UnescapeString(m[1])
		}
		return ""
	}
	auth := "https://auth." + st.Domain
	page, err := get(link)
	if err != nil {
		return err
	}
	email, password := "admin@"+st.Domain, tenant.HumanCode(20)
	code := strings.TrimPrefix(link, auth+"/setup?code=")
	page, err = post(auth+"/setup", url.Values{"csrf": {csrf(page)}, "code": {code}, "email": {email}, "password": {password}, "password2": {password}})
	if err != nil {
		return err
	}
	if !strings.Contains(page, "Setup complete") {
		return fmt.Errorf("setup did not complete")
	}
	page, err = post(auth+"/settings/tokens", url.Values{"csrf": {csrf(page)}, "name": {"dev admin (" + st.RunID + ")"}, "days": {"1"}})
	if err != nil {
		return err
	}
	m := tokenRE.FindStringSubmatch(page)
	if m == nil {
		return fmt.Errorf("no admin token on the settings page")
	}
	write := func(name string, v any) error {
		b, _ := json.MarshalIndent(v, "", "  ")
		return os.WriteFile(filepath.Join(st.Dir, name), b, 0o600)
	}
	if err := write("admin-login.json", map[string]string{"url": auth, "email": email, "password": password}); err != nil {
		return err
	}
	return write("cli-admin.json", tenant.CLIConfig{API: "https://api." + st.Domain, Token: m[1]})
}
