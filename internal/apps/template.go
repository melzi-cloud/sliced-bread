package apps

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io/fs"
	"path"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"go.yaml.in/yaml/v3"
)

// Template format v1 (docs/template-format.md). A template is data: a template.yaml
// plus the files it refers to (action scripts, config file templates).

const TemplateAPIVersion = "bakery.slicedbread.dev/v1"

// Str is a YAML scalar read as text, so `port: 8080` and `default: false` work in
// places that hold strings.
type Str string

// UnmarshalYAML accepts any scalar.
func (s *Str) UnmarshalYAML(n *yaml.Node) error {
	if n.Kind != yaml.ScalarNode {
		return fmt.Errorf("line %d: expected a single value", n.Line)
	}
	*s = Str(n.Value)
	return nil
}

// Template is one template, as loaded from template.yaml.
type Template struct {
	APIVersion  string         `yaml:"apiVersion" json:"apiVersion"`
	Name        string         `yaml:"name" json:"name"`
	Version     string         `yaml:"version" json:"version"`
	Description string         `yaml:"description" json:"description"`
	Upstream    Upstream       `yaml:"upstream" json:"upstream"`
	Access      TemplateAccess `yaml:"access" json:"access"`
	Components  []Component    `yaml:"components" json:"components"`
	Routes      []Route        `yaml:"routes,omitempty" json:"routes,omitempty"`
	Database    *DatabaseSpec  `yaml:"database,omitempty" json:"database,omitempty"`
	Secrets     SecretsSpec    `yaml:"secrets,omitempty" json:"secrets,omitempty"`
	Settings    []Setting      `yaml:"settings,omitempty" json:"settings,omitempty"`
	ForbidEnv   []string       `yaml:"forbidEnv,omitempty" json:"forbidEnv,omitempty"`
	Backup      *BackupSpec    `yaml:"backup,omitempty" json:"backup,omitempty"`
	Actions     []Action       `yaml:"actions,omitempty" json:"actions,omitempty"`
	Setup       []SetupStep    `yaml:"setup,omitempty" json:"setup,omitempty"`
	Notes       string         `yaml:"notes,omitempty" json:"notes,omitempty"`
	Requires    map[string]Str `yaml:"requires,omitempty" json:"requires,omitempty"`

	// Files holds every file of the template directory other than template.yaml, by
	// path relative to it (actions/x.sh, files/config.yml).
	Files map[string]string `yaml:"-" json:"files,omitempty"`
	// Trusted is true for templates from the built-in catalog. Templates loaded from a
	// directory (sb deploy --template-dir) are not: their access defaults and public
	// paths need approval like any agent change.
	Trusted bool `yaml:"-" json:"trusted,omitempty"`
}

// Upstream credits the software a template packages.
type Upstream struct {
	Project     string `yaml:"project" json:"project"`
	URL         string `yaml:"url" json:"url"`
	Licence     string `yaml:"licence" json:"licence"`
	AdaptedFrom string `yaml:"adaptedFrom,omitempty" json:"adaptedFrom,omitempty"`
}

// TemplateAccess is the default access of new apps.
type TemplateAccess struct {
	Mode        string   `yaml:"mode" json:"mode"`
	PublicPaths []string `yaml:"publicPaths,omitempty" json:"publicPaths,omitempty"`
	WallPaths   []string `yaml:"wallPaths,omitempty" json:"wallPaths,omitempty"`
}

// Component is one container of an app.
type Component struct {
	Name      string         `yaml:"name" json:"name"`
	Image     string         `yaml:"image" json:"image"`
	Port      int            `yaml:"port,omitempty" json:"port,omitempty"`
	Health    *Health        `yaml:"health,omitempty" json:"health,omitempty"`
	Replicas  int            `yaml:"replicas,omitempty" json:"replicas,omitempty"`
	Resources *Resources     `yaml:"resources,omitempty" json:"resources,omitempty"`
	Env       map[string]Str `yaml:"env,omitempty" json:"env,omitempty"`
	Volumes   []Volume       `yaml:"volumes,omitempty" json:"volumes,omitempty"`
	Files     []File         `yaml:"files,omitempty" json:"files,omitempty"`
	Command   []string       `yaml:"command,omitempty" json:"command,omitempty"`
	Args      []string       `yaml:"args,omitempty" json:"args,omitempty"`
	RunAsUser int64          `yaml:"runAsUser,omitempty" json:"runAsUser,omitempty"`
}

// Health is the readiness check.
type Health struct {
	Path string `yaml:"path" json:"path"`
}

// Resources are requests; memoryLimit caps memory (default 1Gi).
type Resources struct {
	CPU         string `yaml:"cpu,omitempty" json:"cpu,omitempty"`
	Memory      string `yaml:"memory,omitempty" json:"memory,omitempty"`
	MemoryLimit string `yaml:"memoryLimit,omitempty" json:"memoryLimit,omitempty"`
}

// File is a config file mounted read-only into a component.
type File struct {
	Path     string `yaml:"path" json:"path"`
	Template string `yaml:"template" json:"template"`
}

// Route sends a path prefix to a component.
type Route struct {
	Path string `yaml:"path" json:"path"`
	To   string `yaml:"to" json:"to"`
}

// DatabaseSpec asks for one Postgres database.
type DatabaseSpec struct {
	Postgres *struct {
		Version Str `yaml:"version" json:"version"`
	} `yaml:"postgres" json:"postgres"`
}

// SecretsSpec lists generated and human-provided secrets.
type SecretsSpec struct {
	Generate []GenSecret     `yaml:"generate,omitempty" json:"generate,omitempty"`
	Provide  []ProvideSecret `yaml:"provide,omitempty" json:"provide,omitempty"`
}

// GenSecret is created once by sb-control's random number generator.
type GenSecret struct {
	Name   string `yaml:"name" json:"name"`
	Bytes  int    `yaml:"bytes,omitempty" json:"bytes,omitempty"`
	Format string `yaml:"format,omitempty" json:"format,omitempty"` // hex | base64 | alnum
}

// ProvideSecret is entered by a human in the dashboard.
type ProvideSecret struct {
	Name        string `yaml:"name" json:"name"`
	Description string `yaml:"description,omitempty" json:"description,omitempty"`
	Required    bool   `yaml:"required,omitempty" json:"required,omitempty"`
}

// Setting is a typed option an agent may change.
type Setting struct {
	Name        string         `yaml:"name" json:"name"`
	Description string         `yaml:"description" json:"description"`
	Type        string         `yaml:"type" json:"type"` // enum | string | number | boolean
	Values      []string       `yaml:"values,omitempty" json:"values,omitempty"`
	Pattern     string         `yaml:"pattern,omitempty" json:"pattern,omitempty"`
	Min         *float64       `yaml:"min,omitempty" json:"min,omitempty"`
	Max         *float64       `yaml:"max,omitempty" json:"max,omitempty"`
	Default     Str            `yaml:"default" json:"default"`
	ApprovalFor []string       `yaml:"approvalFor,omitempty" json:"approvalFor,omitempty"`
	Env         map[string]Str `yaml:"env,omitempty" json:"env,omitempty"`
}

// BackupSpec says how to back up an app's volumes.
type BackupSpec struct {
	Consistency string   `yaml:"consistency,omitempty" json:"consistency,omitempty"` // live | stop
	Require     []string `yaml:"require,omitempty" json:"require,omitempty"`
	Exclude     []string `yaml:"exclude,omitempty" json:"exclude,omitempty"`
	// SQLite files that every restore test opens: PRAGMA integrity_check must return ok
	// and the named tables must exist (their row counts are reported, not judged).
	SQLite []SQLiteCheck `yaml:"sqlite,omitempty" json:"sqlite,omitempty"`
}

// SQLiteCheck is a SQLite database inside a volume (<volume>/<path>) and tables to count.
type SQLiteCheck struct {
	Path   string   `yaml:"path" json:"path"`
	Tables []string `yaml:"tables,omitempty" json:"tables,omitempty"`
}

// Field is an action input or output.
type Field struct {
	Type        string `yaml:"type" json:"type"` // string | number | boolean | object | array
	Required    bool   `yaml:"required,omitempty" json:"required,omitempty"`
	Description string `yaml:"description,omitempty" json:"description,omitempty"`
	Secret      bool   `yaml:"secret,omitempty" json:"secret,omitempty"`
}

// Action is a script run as a Job next to the app (D-029).
type Action struct {
	Name        string           `yaml:"name" json:"name"`
	Description string           `yaml:"description" json:"description"`
	Image       string           `yaml:"image" json:"image"`
	Run         string           `yaml:"run" json:"run"`
	Secrets     []string         `yaml:"secrets,omitempty" json:"secrets,omitempty"`
	Database    bool             `yaml:"database,omitempty" json:"database,omitempty"`
	Volumes     []string         `yaml:"volumes,omitempty" json:"volumes,omitempty"`
	Input       map[string]Field `yaml:"input,omitempty" json:"input,omitempty"`
	Output      map[string]Field `yaml:"output,omitempty" json:"output,omitempty"`
	On          string           `yaml:"on,omitempty" json:"on,omitempty"` // install | upgrade | "" (manual)
	Approval    bool             `yaml:"approval,omitempty" json:"approval,omitempty"`
	Timeout     string           `yaml:"timeout,omitempty" json:"timeout,omitempty"`
	Egress      []string         `yaml:"egress,omitempty" json:"egress,omitempty"`
}

// HasSecretOutput reports whether an action returns values only humans may see.
func (a *Action) HasSecretOutput() bool {
	for _, f := range a.Output {
		if f.Secret {
			return true
		}
	}
	return false
}

// TimeoutDuration is the action's time limit (default 60 seconds).
func (a *Action) TimeoutDuration() time.Duration {
	if d, err := time.ParseDuration(a.Timeout); err == nil && d > 0 {
		return d
	}
	return 60 * time.Second
}

// SetupStep is something the human still has to do.
type SetupStep struct {
	Name        string `yaml:"name" json:"name"`
	Description string `yaml:"description" json:"description"`
	Link        string `yaml:"link,omitempty" json:"link,omitempty"`
	DoneWhen    string `yaml:"doneWhen" json:"doneWhen"`
}

// --- loading ---

// LoadTemplate reads a template directory from fsys.
func LoadTemplate(fsys fs.FS, dir string) (*Template, error) {
	raw, err := fs.ReadFile(fsys, path.Join(dir, "template.yaml"))
	if err != nil {
		return nil, err
	}
	files := map[string]string{}
	err = fs.WalkDir(fsys, dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		rel := strings.TrimPrefix(strings.TrimPrefix(p, dir), "/")
		if rel == "template.yaml" {
			return nil
		}
		b, err := fs.ReadFile(fsys, p)
		if err != nil {
			return err
		}
		files[rel] = string(b)
		return nil
	})
	if err != nil {
		return nil, err
	}
	return ParseTemplate(raw, files)
}

// ParseTemplate parses template.yaml and validates it with its files.
func ParseTemplate(raw []byte, files map[string]string) (*Template, error) {
	var t Template
	dec := yaml.NewDecoder(bytes.NewReader(raw))
	dec.KnownFields(true)
	if err := dec.Decode(&t); err != nil {
		return nil, fmt.Errorf("template.yaml: %w", err)
	}
	t.Files = files
	t.Defaults()
	if err := t.Validate(); err != nil {
		name := t.Name
		if name == "" {
			name = "template"
		}
		return nil, fmt.Errorf("%s: %w", name, err)
	}
	return &t, nil
}

// Defaults fills optional fields.
func (t *Template) Defaults() {
	for i := range t.Components {
		c := &t.Components[i]
		if c.Replicas == 0 {
			c.Replicas = 1
		}
		for j := range c.Volumes {
			if c.Volumes[j].Size == "" {
				c.Volumes[j].Size = "10Gi"
			}
		}
	}
	for i := range t.Secrets.Generate {
		g := &t.Secrets.Generate[i]
		if g.Bytes == 0 {
			g.Bytes = 32
		}
		if g.Format == "" {
			g.Format = "hex"
		}
	}
	if t.Backup != nil && t.Backup.Consistency == "" {
		t.Backup.Consistency = "live"
	}
}

// Hash is a short content hash of the template and its files.
func (t *Template) Hash() string {
	h := sha256.New()
	keys := make([]string, 0, len(t.Files))
	for k := range t.Files {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	fmt.Fprintf(h, "%s@%s\n", t.Name, t.Version)
	for _, k := range keys {
		fmt.Fprintf(h, "%s\n%d\n%s", k, len(t.Files[k]), t.Files[k])
	}
	return hex.EncodeToString(h.Sum(nil))[:12]
}

// --- validation ---

var (
	tmplNameRE   = regexp.MustCompile(`^[a-z][a-z0-9-]{0,28}[a-z0-9]$`)
	versionRE    = regexp.MustCompile(`^\d+\.\d+\.\d+$`)
	compNameRE   = regexp.MustCompile(`^[a-z][a-z0-9-]{0,18}[a-z0-9]$`)
	fieldNameRE  = regexp.MustCompile(`^[a-z][a-z0-9_]{0,40}$`)
	digestRefRE  = regexp.MustCompile(`^[a-z0-9./_:-]+(:[A-Za-z0-9._-]+)?@sha256:[a-f0-9]{64}$`)
	secretNameRE = regexp.MustCompile(`^[A-Z][A-Z0-9_]{0,62}$`)
	refRE        = regexp.MustCompile(`\$\{([^}]*)\}`)
	quantityRE   = regexp.MustCompile(`^[0-9]+(\.[0-9]+)?(m|Mi|Gi|Ki)?$`)
	sqlitePathRE = regexp.MustCompile(`^[A-Za-z0-9._-]+(/[A-Za-z0-9._-]+)+$`)
	sqlTableRE   = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]{0,62}$`)
)

// DigestPinned reports whether an image reference names a digest.
func DigestPinned(image string) bool { return digestRefRE.MatchString(image) }

// Validate checks a template for errors an author can fix.
func (t *Template) Validate() error {
	var errs []string
	bad := func(format string, a ...any) { errs = append(errs, fmt.Sprintf(format, a...)) }
	if t.APIVersion != TemplateAPIVersion {
		bad("apiVersion must be %s", TemplateAPIVersion)
	}
	if !tmplNameRE.MatchString(t.Name) || Reserved[t.Name] {
		bad("name %q: 2-30 lowercase letters, digits or dashes", t.Name)
	}
	if !versionRE.MatchString(t.Version) {
		bad("version %q: use semver like 1.0.0", t.Version)
	}
	if strings.TrimSpace(t.Description) == "" {
		bad("description is required")
	}
	if t.Upstream.Project == "" || !strings.HasPrefix(t.Upstream.URL, "https://") || t.Upstream.Licence == "" {
		bad("upstream needs project, https url and licence")
	}
	switch t.Access.Mode {
	case AccessWall, AccessAppAuth, AccessPublic:
	default:
		bad("access.mode must be wall, app-auth or public")
	}
	for _, p := range append(append([]string{}, t.Access.PublicPaths...), t.Access.WallPaths...) {
		if !validPath(p) {
			bad("access path %q must start with / and contain no spaces or quotes", p)
		}
	}
	if len(t.Components) == 0 {
		bad("at least one component is required")
	}
	comps := map[string]*Component{}
	volumes := map[string]string{}
	volComponents := map[string]bool{}
	for i := range t.Components {
		c := &t.Components[i]
		if !compNameRE.MatchString(c.Name) {
			bad("component name %q: 2-20 lowercase letters, digits or dashes", c.Name)
		}
		if comps[c.Name] != nil {
			bad("component %s is defined twice", c.Name)
		}
		comps[c.Name] = c
		if !DigestPinned(c.Image) {
			bad("component %s: image must be pinned by digest (name:tag@sha256:...)", c.Name)
		}
		if c.Port < 0 || c.Port > 65535 {
			bad("component %s: invalid port", c.Name)
		}
		if i == 0 && c.Port == 0 {
			bad("the first component (%s) serves the app and needs a port", c.Name)
		}
		if c.Health != nil && (c.Port == 0 || !validPath(c.Health.Path)) {
			bad("component %s: health needs a port and a path starting with /", c.Name)
		}
		if c.Replicas < 1 || c.Replicas > 10 {
			bad("component %s: replicas must be 1 to 10", c.Name)
		}
		if len(c.Volumes) > 0 && c.Replicas != 1 {
			bad("component %s: a component with a volume runs exactly 1 replica", c.Name)
		}
		if r := c.Resources; r != nil {
			for _, q := range []string{r.CPU, r.Memory, r.MemoryLimit} {
				if q != "" && !quantityRE.MatchString(q) {
					bad("component %s: invalid resource quantity %q", c.Name, q)
				}
			}
		}
		for k := range c.Env {
			if !envKeyRE.MatchString(k) {
				bad("component %s: invalid env key %q", c.Name, k)
			}
		}
		for _, v := range c.Volumes {
			if !compNameRE.MatchString(v.Name) || !strings.HasPrefix(v.Path, "/") {
				bad("component %s: volume %q needs a short lowercase name and an absolute path", c.Name, v.Name)
			}
			if other, dup := volumes[v.Name]; dup {
				bad("volume %s is defined in %s and %s", v.Name, other, c.Name)
			}
			volumes[v.Name] = c.Name
			volComponents[c.Name] = true
		}
		for _, f := range c.Files {
			if !strings.HasPrefix(f.Path, "/") || strings.Contains(f.Path, "..") {
				bad("component %s: file path %q must be absolute", c.Name, f.Path)
			}
			if _, ok := t.Files[f.Template]; !ok {
				bad("component %s: file template %s is missing from the template directory", c.Name, f.Template)
			}
		}
		if c.RunAsUser < 0 {
			bad("component %s: runAsUser must be positive", c.Name)
		}
	}
	if len(volComponents) > 1 {
		bad("in v1, volumes must all belong to one component (backups copy them together)")
	}
	for _, r := range t.Routes {
		c := comps[r.To]
		if c == nil || c.Port == 0 {
			bad("route %s goes to %q, which is not a component with a port", r.Path, r.To)
		}
		if !validPath(r.Path) {
			bad("route path %q must start with /", r.Path)
		}
	}
	if t.Database != nil && t.Database.Postgres == nil {
		bad("database: only postgres is supported")
	}
	secrets := map[string]bool{}
	for _, g := range t.Secrets.Generate {
		if !secretNameRE.MatchString(g.Name) || secrets[g.Name] {
			bad("generated secret %q: uppercase name, defined once", g.Name)
		}
		secrets[g.Name] = true
		if g.Bytes < 8 || g.Bytes > 128 {
			bad("generated secret %s: bytes must be 8 to 128", g.Name)
		}
		switch g.Format {
		case "hex", "base64", "alnum":
		default:
			bad("generated secret %s: format must be hex, base64 or alnum", g.Name)
		}
	}
	for _, p := range t.Secrets.Provide {
		if !secretNameRE.MatchString(p.Name) || secrets[p.Name] {
			bad("provided secret %q: uppercase name, defined once", p.Name)
		}
		secrets[p.Name] = true
	}
	settings := map[string]*Setting{}
	for i := range t.Settings {
		s := &t.Settings[i]
		if !fieldNameRE.MatchString(s.Name) || settings[s.Name] != nil {
			bad("setting %q: lowercase name, defined once", s.Name)
		}
		settings[s.Name] = s
		if s.Description == "" {
			bad("setting %s needs a description", s.Name)
		}
		switch s.Type {
		case "enum":
			if len(s.Values) == 0 {
				bad("setting %s: enum needs values", s.Name)
			}
		case "string", "number", "boolean":
		default:
			bad("setting %s: type must be enum, string, number or boolean", s.Name)
		}
		if s.Pattern != "" {
			if _, err := regexp.Compile(s.Pattern); err != nil {
				bad("setting %s: invalid pattern: %v", s.Name, err)
			}
		}
		if err := s.Check(string(s.Default)); err != nil && !(s.Type == "string" && s.Default == "") {
			bad("setting %s: default: %v", s.Name, err)
		}
		for _, v := range s.ApprovalFor {
			if err := s.Check(v); err != nil {
				bad("setting %s: approvalFor %q: %v", s.Name, v, err)
			}
		}
		for k := range s.Env {
			if !envKeyRE.MatchString(k) {
				bad("setting %s: invalid env key %q", s.Name, k)
			}
		}
	}
	for _, k := range t.ForbidEnv {
		if !envKeyRE.MatchString(k) {
			bad("forbidEnv: invalid key %q", k)
		}
	}
	if t.Backup != nil {
		switch t.Backup.Consistency {
		case "live", "stop":
		default:
			bad("backup.consistency must be live or stop")
		}
		for _, p := range append(append([]string{}, t.Backup.Require...), t.Backup.Exclude...) {
			vol, _, _ := strings.Cut(p, "/")
			if _, ok := volumes[vol]; !ok || strings.Contains(p, "..") {
				bad("backup path %q must start with a volume name", p)
			}
		}
		seen := map[string]bool{}
		for _, c := range t.Backup.SQLite {
			vol, _, _ := strings.Cut(c.Path, "/")
			if _, ok := volumes[vol]; !ok || !sqlitePathRE.MatchString(c.Path) || strings.Contains(c.Path, "..") {
				bad("backup.sqlite path %q must be a file in a volume: <volume>/<path>, letters, digits, . _ - and /", c.Path)
			} else if seen[c.Path] {
				bad("backup.sqlite path %q is listed twice", c.Path)
			}
			seen[c.Path] = true
			for _, tb := range c.Tables {
				if !sqlTableRE.MatchString(tb) {
					bad("backup.sqlite %s: table %q must be a plain name (letters, digits, _)", c.Path, tb)
				}
			}
		}
	}
	actions := map[string]*Action{}
	for i := range t.Actions {
		a := &t.Actions[i]
		if !fieldNameRE.MatchString(a.Name) || actions[a.Name] != nil {
			bad("action %q: lowercase name, defined once", a.Name)
		}
		actions[a.Name] = a
		if !DigestPinned(a.Image) {
			bad("action %s: image must be pinned by digest", a.Name)
		}
		if _, ok := t.Files[a.Run]; !ok {
			bad("action %s: script %s is missing from the template directory", a.Name, a.Run)
		} else if !strings.HasPrefix(t.Files[a.Run], "#!") {
			bad("action %s: script %s must start with #! (for example #!/bin/sh)", a.Name, a.Run)
		}
		for _, sec := range a.Secrets {
			if !secrets[sec] {
				bad("action %s: secret %s is not declared in secrets", a.Name, sec)
			}
		}
		for _, v := range a.Volumes {
			if _, ok := volumes[v]; !ok {
				bad("action %s: volume %s does not exist", a.Name, v)
			}
		}
		if a.Database && t.Database == nil {
			bad("action %s: database: true but the template has no database", a.Name)
		}
		switch a.On {
		case "", "install", "upgrade":
		default:
			bad("action %s: on must be install or upgrade", a.Name)
		}
		if a.On != "" && len(a.Input) > 0 {
			for name, f := range a.Input {
				if f.Required {
					bad("action %s runs on %s and cannot have the required input %s", a.Name, a.On, name)
				}
			}
		}
		if a.Timeout != "" {
			if d, err := time.ParseDuration(a.Timeout); err != nil || d <= 0 || d > 15*time.Minute {
				bad("action %s: timeout must be a duration up to 15m", a.Name)
			}
		}
		for name, f := range a.Input {
			if !fieldNameRE.MatchString(name) || (f.Type != "string" && f.Type != "number" && f.Type != "boolean") {
				bad("action %s: input %q needs a lowercase name and type string, number or boolean", a.Name, name)
			}
		}
		for name, f := range a.Output {
			switch f.Type {
			case "string", "number", "boolean", "object", "array":
			default:
				bad("action %s: output %s has an invalid type", a.Name, name)
			}
		}
	}
	for _, s := range t.Setup {
		if !fieldNameRE.MatchString(s.Name) || s.Description == "" {
			bad("setup step %q needs a lowercase name and a description", s.Name)
		}
		a := actions[s.DoneWhen]
		if a == nil {
			bad("setup step %s: doneWhen must name an action", s.Name)
		} else if a.Output["done"].Type != "boolean" || len(a.Input) > 0 && hasRequired(a.Input) {
			bad("setup step %s: action %s must output done (boolean) and take no required input", s.Name, a.Name)
		}
		if s.Link != "" && !strings.HasPrefix(s.Link, "/") {
			bad("setup step %s: link must be a path on the app, starting with /", s.Name)
		}
	}
	// References in env, settings env and files.
	check := func(where, text string, inFile bool) {
		for _, m := range refRE.FindAllStringSubmatch(text, -1) {
			if err := t.checkRef(m[1], settings, secrets, comps); err != nil {
				bad("%s: ${%s}: %v", where, m[1], err)
			}
			if !inFile && (strings.HasPrefix(m[1], "secret.") || m[1] == "db.url" || m[1] == "db.password") && strings.TrimSpace(text) != "${"+m[1]+"}" {
				bad("%s: ${%s} must be the whole value in env (it comes from a Kubernetes Secret)", where, m[1])
			}
		}
	}
	for _, c := range t.Components {
		for k, v := range c.Env {
			check("component "+c.Name+" env "+k, string(v), false)
		}
		for _, f := range c.Files {
			check("file "+f.Template, t.Files[f.Template], true)
		}
		for _, a := range append(append([]string{}, c.Command...), c.Args...) {
			if strings.Contains(a, "${") {
				bad("component %s: command and args cannot use references; use env", c.Name)
			}
		}
	}
	for _, s := range t.Settings {
		for k, v := range s.Env {
			check("setting "+s.Name+" env "+k, string(v), false)
		}
	}
	if len(errs) > 0 {
		return fmt.Errorf("%s", strings.Join(errs, "; "))
	}
	return nil
}

func hasRequired(in map[string]Field) bool {
	for _, f := range in {
		if f.Required {
			return true
		}
	}
	return false
}

func validPath(p string) bool {
	return strings.HasPrefix(p, "/") && !strings.ContainsAny(p, " \t\n`\"'")
}

func (t *Template) checkRef(ref string, settings map[string]*Setting, secrets map[string]bool, comps map[string]*Component) error {
	parts := strings.Split(ref, ".")
	switch parts[0] {
	case "app":
		if ref == "app.url" || ref == "app.host" {
			return nil
		}
	case "db":
		if t.Database == nil {
			return fmt.Errorf("the template has no database")
		}
		switch ref {
		case "db.url", "db.host", "db.user", "db.password", "db.name", "db.port":
			return nil
		}
	case "secret":
		if len(parts) == 2 && secrets[parts[1]] {
			return nil
		}
		return fmt.Errorf("secret is not declared")
	case "setting":
		if len(parts) == 2 && settings[parts[1]] != nil {
			return nil
		}
		return fmt.Errorf("setting is not declared")
	case "component":
		if len(parts) == 3 && comps[parts[1]] != nil && comps[parts[1]].Port > 0 {
			switch parts[2] {
			case "internal", "host", "port":
				return nil
			}
		}
		return fmt.Errorf("needs a component with a port and .internal, .host or .port")
	case "apps":
		if len(parts) == 3 && (parts[2] == "url" || parts[2] == "internal") {
			return nil
		}
	}
	return fmt.Errorf("unknown reference")
}

// Check validates a setting value.
func (s *Setting) Check(v string) error {
	switch s.Type {
	case "enum":
		for _, x := range s.Values {
			if x == v {
				return nil
			}
		}
		return fmt.Errorf("must be one of %s", strings.Join(s.Values, ", "))
	case "boolean":
		if v != "true" && v != "false" {
			return fmt.Errorf("must be true or false")
		}
	case "number":
		f, err := strconv.ParseFloat(v, 64)
		if err != nil {
			return fmt.Errorf("must be a number")
		}
		if s.Min != nil && f < *s.Min || s.Max != nil && f > *s.Max {
			return fmt.Errorf("out of range")
		}
	case "string":
		if s.Pattern != "" && v != "" {
			if ok, _ := regexp.MatchString(s.Pattern, v); !ok {
				return fmt.Errorf("must match %s", s.Pattern)
			}
		}
		if strings.ContainsAny(v, "\n\r") || len(v) > 500 {
			return fmt.Errorf("must be one line, up to 500 characters")
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

// Component looks up a component by name.
func (t *Template) Component(name string) *Component {
	for i := range t.Components {
		if t.Components[i].Name == name {
			return &t.Components[i]
		}
	}
	return nil
}
