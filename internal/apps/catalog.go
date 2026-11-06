package apps

import (
	"fmt"
	"sort"
)

// Template is a catalog entry. Apply fills defaults into a new app.
type Template struct {
	Name        string    `json:"name"`
	Description string    `json:"description"`
	Access      string    `json:"access"`
	Settings    []Setting `json:"settings,omitempty"`
	Actions     []Action  `json:"actions,omitempty"`
	Notes       string    `json:"notes,omitempty"`
	ForbidEnv   []string  `json:"-"`
	apply       func(a *App)
}

// Setting is a template-specific option mapped onto env.
type Setting struct {
	Name        string   `json:"name"`
	Description string   `json:"description"`
	Values      []string `json:"values"`
	Default     string   `json:"default"`
	// ApprovalFor lists values that widen exposure and need human approval when set by an agent.
	ApprovalFor []string `json:"approvalFor,omitempty"`
}

// Action is a typed operation implemented by the control plane.
type Action struct {
	Name        string            `json:"name"`
	Description string            `json:"description"`
	Input       map[string]string `json:"input,omitempty"` // field -> description
	AdminOnly   bool              `json:"adminOnly,omitempty"`
}

// Catalog lists the available templates.
var Catalog = map[string]*Template{
	"vaultwarden": {
		Name:        "vaultwarden",
		Description: "Password manager compatible with Bitwarden apps and browser extensions.",
		Access:      AccessAppAuth,
		Settings: []Setting{{
			Name: "signups", Description: "Allow new accounts to register. Turn on to create your account, then turn off.",
			Values: []string{"false", "true"}, Default: "false", ApprovalFor: []string{"true"},
		}},
		Notes: "Runs in app-auth mode: the SlicedBread login wall is off because Bitwarden apps and extensions cannot pass it. " +
			"Signups are off by default and the admin page is disabled. To create the first account, set signups=true " +
			"(needs human approval), register at the app URL, then set signups=false.",
		ForbidEnv: []string{"ADMIN_TOKEN", "SIGNUPS_ALLOWED"},
		apply: func(a *App) {
			a.Image = "vaultwarden/server:1.37.5"
			a.Port = 8080
			a.HealthPath = "/alive"
			a.Access = AccessAppAuth
			setDefault(a, "ROCKET_PORT", "8080")
			setDefault(a, "DOMAIN", "${app.url}")
			setDefault(a, "INVITATIONS_ALLOWED", "false")
			setDefault(a, "SHOW_PASSWORD_HINT", "false")
			a.Volumes = []Volume{{Name: "data", Path: "/data", Size: "10Gi"}}
		},
	},
	"umami": {
		Name:        "umami",
		Description: "Privacy-friendly web analytics with a Postgres database.",
		Access:      AccessWall,
		Actions: []Action{
			{
				Name:        "create_website",
				Description: "Register a website and return the tracking snippet to add to its HTML <head>.",
				Input:       map[string]string{"name": "display name", "domain": "site domain without scheme, e.g. shop.example.com"},
			},
			{Name: "dashboard_login", Description: "Return the Umami dashboard username and password (human use).", AdminOnly: true},
		},
		Notes: "The dashboard sits behind the SlicedBread login wall and also has its own Umami login (see dashboard_login). " +
			"The tracker script (/script.js) and event endpoint (/api/send) are public so websites can report visits. " +
			"Use the create_website action to get a snippet; do not create sites through the UI.",
		apply: func(a *App) {
			a.Image = "ghcr.io/umami-software/umami:3.4.1"
			a.Port = 3000
			a.HealthPath = "/api/heartbeat"
			a.Access = AccessWall
			a.PublicPaths = []string{"/script.js", "/api/send"}
			a.Database = true
			setDefault(a, "DATABASE_URL", "${db.url}")
			setDefault(a, "APP_SECRET", "${secret.APP_SECRET}")
			a.Generated = []string{"APP_SECRET", "UMAMI_ADMIN_PASSWORD"}
		},
	},
}

func setDefault(a *App, k, v string) {
	if a.Env == nil {
		a.Env = map[string]string{}
	}
	if _, ok := a.Env[k]; !ok {
		a.Env[k] = v
	}
}

// ApplyTemplate fills a template's defaults into a and applies settings.
func ApplyTemplate(a *App) error {
	t, ok := Catalog[a.Template]
	if !ok {
		return fmt.Errorf("unknown template %q (see: sb catalog)", a.Template)
	}
	t.apply(a)
	if a.Settings == nil {
		a.Settings = map[string]string{}
	}
	for _, s := range t.Settings {
		v, ok := a.Settings[s.Name]
		if !ok {
			v = s.Default
			a.Settings[s.Name] = v
		}
		if !contains(s.Values, v) {
			return fmt.Errorf("setting %s must be one of %v", s.Name, s.Values)
		}
	}
	for k := range a.Settings {
		if t.Setting(k) == nil {
			return fmt.Errorf("unknown setting %q for %s", k, t.Name)
		}
	}
	if t.Name == "vaultwarden" {
		a.Env["SIGNUPS_ALLOWED"] = a.Settings["signups"]
	}
	return nil
}

// CheckUserEnv refuses env keys a template controls itself.
func CheckUserEnv(template string, keys ...string) error {
	t, ok := Catalog[template]
	if !ok {
		return nil
	}
	for _, f := range t.ForbidEnv {
		for _, k := range keys {
			if k == f {
				return fmt.Errorf("env %s cannot be set on %s apps (it is managed by the template; see: sb catalog)", k, t.Name)
			}
		}
	}
	return nil
}

// Setting looks up a setting by name.
func (t *Template) Setting(name string) *Setting {
	for i := range t.Settings {
		if t.Settings[i].Name == name {
			return &t.Settings[i]
		}
	}
	return nil
}

// Action looks up an action by name.
func (t *Template) Action(name string) *Action {
	for i := range t.Actions {
		if t.Actions[i].Name == name {
			return &t.Actions[i]
		}
	}
	return nil
}

// TemplateNames returns sorted template names.
func TemplateNames() []string {
	var out []string
	for k := range Catalog {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func contains(xs []string, s string) bool {
	for _, x := range xs {
		if x == s {
			return true
		}
	}
	return false
}
