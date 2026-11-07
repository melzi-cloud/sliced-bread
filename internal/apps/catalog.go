package apps

import (
	"encoding/json"
	"fmt"
	"strings"
)

func cloneTemplate(t *Template) *Template {
	b, _ := json.Marshal(t)
	var c Template
	json.Unmarshal(b, &c)
	return &c
}

// ApplyTemplate (re)derives an app's template-controlled fields from its template and
// settings. On the first call it copies the template into the app (a.TemplateDef) and
// sets the defaults the user may change later (access mode, public paths); later calls
// keep those, so a settings change never resets what the user chose.
func ApplyTemplate(a *App) error {
	if a.TemplateDef == nil {
		cat := CatalogTemplate(a.Template)
		if cat == nil {
			return fmt.Errorf("unknown template %q (see: sb catalog)", a.Template)
		}
		a.TemplateDef = cloneTemplate(cat)
	}
	t := a.TemplateDef
	first := a.TemplateVersion == ""
	a.Template, a.TemplateVersion = t.Name, t.Version
	if a.Settings == nil {
		a.Settings = map[string]string{}
	}
	for k := range a.Settings {
		if t.Setting(k) == nil {
			return fmt.Errorf("unknown setting %q for template %s (see: sb catalog)", k, t.Name)
		}
	}
	for _, s := range t.Settings {
		v, set := a.Settings[s.Name]
		if !set {
			v = string(s.Default)
			a.Settings[s.Name] = v
		}
		if v == "" && s.Type == "string" {
			continue
		}
		if err := s.Check(v); err != nil {
			return fmt.Errorf("setting %s: %v", s.Name, err)
		}
	}
	if first {
		if a.Access == "" {
			a.Access = t.Access.Mode
		}
		if len(a.PublicPaths) == 0 {
			a.PublicPaths = append([]string{}, t.Access.PublicPaths...)
		}
	}
	a.WallPaths = append([]string{}, t.Access.WallPaths...)
	a.Image, a.Port, a.HealthPath, a.RunAsUser = "", 0, "", 0
	a.Database = t.Database != nil
	a.Volumes = nil
	for _, c := range t.Components {
		a.Volumes = append(a.Volumes, c.Volumes...)
	}
	a.Generate = mergeGen(t.Secrets.Generate, a.Generate)
	a.Provide = mergeProvide(t.Secrets.Provide, a.Provide)
	a.BackupStop, a.BackupRequire, a.BackupExclude, a.BackupSQLite = false, nil, nil, nil
	if b := t.Backup; b != nil {
		a.BackupStop = b.Consistency == "stop"
		a.BackupRequire = append([]string{}, b.Require...)
		a.BackupExclude = append([]string{}, b.Exclude...)
		for _, c := range b.SQLite {
			a.BackupSQLite = append(a.BackupSQLite, SQLiteCheck{Path: c.Path, Tables: append([]string{}, c.Tables...)})
		}
	}
	return nil
}

// mergeGen keeps the template's generated secrets and any the user added.
func mergeGen(tmpl, cur []GenSecret) []GenSecret {
	out := append([]GenSecret{}, tmpl...)
	seen := map[string]bool{}
	for _, g := range tmpl {
		seen[g.Name] = true
	}
	for _, g := range cur {
		if !seen[g.Name] {
			out = append(out, g)
		}
	}
	return out
}

func mergeProvide(tmpl, cur []ProvideSecret) []ProvideSecret {
	out := append([]ProvideSecret{}, tmpl...)
	seen := map[string]bool{}
	for _, p := range tmpl {
		seen[p.Name] = true
	}
	for _, p := range cur {
		if !seen[p.Name] {
			out = append(out, p)
		}
	}
	return out
}

// TemplateOf returns the template an app was created from, or nil.
func TemplateOf(a *App) *Template {
	if a.TemplateDef != nil {
		return a.TemplateDef
	}
	return CatalogTemplate(a.Template)
}

// CheckUserEnv refuses env keys a template controls itself.
func CheckUserEnv(a *App, keys ...string) error {
	t := TemplateOf(a)
	if t == nil {
		return nil
	}
	for _, k := range keys {
		for _, f := range t.ForbidEnv {
			if strings.EqualFold(f, k) {
				return fmt.Errorf("env %s is controlled by the %s template (use its settings; see: sb catalog)", k, t.Name)
			}
		}
	}
	return nil
}

func contains(xs []string, s string) bool {
	for _, x := range xs {
		if x == s {
			return true
		}
	}
	return false
}
