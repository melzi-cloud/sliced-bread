// Package apps defines the SlicedBread app model and renders it into Kubernetes objects.
package apps

import (
	"fmt"
	"regexp"
	"sort"
	"strings"
)

// Access modes for an app's ingress.
const (
	AccessWall    = "wall"     // every request needs a SlicedBread session
	AccessAppAuth = "app-auth" // no wall; the app's own login protects it
	AccessPublic  = "public"   // no wall, no app login expected
)

// App is the desired state of one app. It is stored as the spec of an App
// custom resource and is the only thing the reconciler reads.
type App struct {
	Name        string            `json:"name"`
	Template    string            `json:"template,omitempty"`
	Image       string            `json:"image,omitempty"`  // for image deploys and templates
	Repo        *Repo             `json:"repo,omitempty"`   // for git deploys
	Upload      *Upload           `json:"upload,omitempty"` // for source uploaded from a local directory
	Port        int               `json:"port,omitempty"`
	Env         map[string]string `json:"env,omitempty"`        // values may reference ${...}
	SecretKeys  []string          `json:"secretKeys,omitempty"` // env keys whose values live in the app secret
	Access      string            `json:"access"`
	PublicPaths []string          `json:"publicPaths,omitempty"`
	Volumes     []Volume          `json:"volumes,omitempty"`
	Database    bool              `json:"database,omitempty"`
	HealthPath  string            `json:"healthPath,omitempty"`
	RunAsUser   int64             `json:"runAsUser,omitempty"`
	Settings    map[string]string `json:"settings,omitempty"`
	// Generate lists secrets sb-control creates once with its own random number generator;
	// Provide lists secrets a human enters in the dashboard (D-030: agents never see or
	// write secret values).
	Generate []GenSecret     `json:"generate,omitempty"`
	Provide  []ProvideSecret `json:"provide,omitempty"`
	// Template apps: the template the app was created from (with its scripts and files),
	// its version, and paths that need the login wall even when the app is not walled.
	TemplateDef     *Template         `json:"templateDef,omitempty"`
	TemplateVersion string            `json:"templateVersion,omitempty"`
	WallPaths       []string          `json:"wallPaths,omitempty"`
	Build           *BuildState       `json:"build,omitempty"`
	Hooks           map[string]string `json:"hooks,omitempty"` // template hook progress, e.g. umami admin password set
	Generation      int64             `json:"generation,omitempty"`

	// Backups. BackupStop stops the app while its volumes are copied (for SQLite and similar
	// files that are unsafe to copy while open). BackupRequire lists files, relative to the
	// volume root (<volume>/<path>), that a restored backup must contain and be non-empty.
	BackupStop    bool         `json:"backupStop,omitempty"`
	BackupRequire []string     `json:"backupRequire,omitempty"`
	BackupExclude []string     `json:"backupExclude,omitempty"`
	Paused        bool         `json:"paused,omitempty"` // scaled to 0 for a backup or a restore
	Backup        *BackupState `json:"backup,omitempty"`
}

// HasData reports whether the app has anything to back up.
func (a *App) HasData() bool { return a.Database || len(a.Volumes) > 0 }

// BackupState tracks backups, restore tests and restores of an app.
type BackupState struct {
	Running     *BackupRun  `json:"running,omitempty"`
	LastBackup  *BackupRun  `json:"lastBackup,omitempty"`
	LastTest    *BackupRun  `json:"lastTest,omitempty"`
	LastRestore *BackupRun  `json:"lastRestore,omitempty"`
	History     []BackupRun `json:"history,omitempty"` // successful backups, newest first
}

// BackupRun is one backup, restore test or restore.
type BackupRun struct {
	ID         string `json:"id"`
	Kind       string `json:"kind"`  // backup, test, restore
	Phase      string `json:"phase"` // stopping, running, succeeded, failed
	Trigger    string `json:"trigger,omitempty"`
	Snapshot   string `json:"snapshot,omitempty"`
	Files      int    `json:"files,omitempty"`
	Bytes      int64  `json:"bytes,omitempty"`
	DBTables   int    `json:"dbTables,omitempty"`
	DBRows     int64  `json:"dbRows,omitempty"`
	StartedAt  string `json:"startedAt"`
	FinishedAt string `json:"finishedAt,omitempty"`
	Message    string `json:"message,omitempty"`
}

// Repo is a git source built with Railpack (or its Dockerfile if present).
type Repo struct {
	URL    string `json:"url"`
	Branch string `json:"branch,omitempty"`
	Path   string `json:"path,omitempty"`
}

// Upload is source code uploaded by the CLI and stored by the control plane.
type Upload struct {
	ID     string `json:"id"`
	Bytes  int64  `json:"bytes,omitempty"`
	SHA256 string `json:"sha256,omitempty"` // first 12 hex chars of the tarball hash
}

// Builds reports whether the app is built from source (repo or upload).
func (a *App) Builds() bool { return a.Repo != nil || a.Upload != nil }

// BuildState tracks the latest build of a repo app.
type BuildState struct {
	ID         string `json:"id"`
	Phase      string `json:"phase"` // pending, running, succeeded, failed
	Image      string `json:"image,omitempty"`
	Message    string `json:"message,omitempty"`
	LastGood   string `json:"lastGood,omitempty"` // image currently deployed
	StartedAt  string `json:"startedAt,omitempty"`
	FinishedAt string `json:"finishedAt,omitempty"`
}

// Volume is a persistent volume mounted into the app.
type Volume struct {
	Name string `json:"name"`
	Path string `json:"path"`
	Size string `json:"size,omitempty"` // Hetzner minimum is 10Gi
}

var nameRE = regexp.MustCompile(`^[a-z][a-z0-9-]{0,28}[a-z0-9]$`)

// Reserved names cannot be used for apps because they are platform hostnames.
var Reserved = map[string]bool{"api": true, "auth": true, "www": true, "registry": true, "sb": true}

var uploadIDRE = regexp.MustCompile(`^u[a-z0-9]{10}$`)

var envKeyRE = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

// Validate checks an app spec.
func (a *App) Validate() error {
	if !nameRE.MatchString(a.Name) {
		return fmt.Errorf("invalid app name %q: use 2-30 lowercase letters, digits or dashes, starting with a letter", a.Name)
	}
	if Reserved[a.Name] {
		return fmt.Errorf("app name %q is reserved", a.Name)
	}
	sources := 0
	if a.TemplateDef != nil {
		sources++
	}
	if a.Image != "" {
		sources++
	}
	if a.Upload != nil {
		sources++
		if !uploadIDRE.MatchString(a.Upload.ID) {
			return fmt.Errorf("invalid upload id")
		}
	}
	if a.Repo != nil {
		sources++
		if !strings.HasPrefix(a.Repo.URL, "https://") {
			return fmt.Errorf("repo URL must start with https://")
		}
		if strings.Contains(a.Repo.Path, "..") {
			return fmt.Errorf("repo path must not contain ..")
		}
	}
	if sources != 1 {
		return fmt.Errorf("an app needs exactly one source: a template, an image, a repo, or a directory")
	}
	switch a.Access {
	case AccessWall, AccessAppAuth, AccessPublic:
	default:
		return fmt.Errorf("access must be one of wall, app-auth, public")
	}
	if a.Port < 0 || a.Port > 65535 {
		return fmt.Errorf("invalid port %d", a.Port)
	}
	for k := range a.Env {
		if !envKeyRE.MatchString(k) {
			return fmt.Errorf("invalid env key %q", k)
		}
	}
	for _, p := range append(append([]string{}, a.PublicPaths...), a.WallPaths...) {
		if !validPath(p) {
			return fmt.Errorf("invalid path %q", p)
		}
	}
	for _, g := range a.Generate {
		if !envKeyRE.MatchString(g.Name) {
			return fmt.Errorf("invalid secret name %q", g.Name)
		}
	}
	for _, p := range a.Provide {
		if !envKeyRE.MatchString(p.Name) {
			return fmt.Errorf("invalid secret name %q", p.Name)
		}
	}
	return nil
}

// Namespace returns the Kubernetes namespace for an app.
func Namespace(name string) string { return "app-" + name }

// Host returns the public hostname for an app of a self-hosted install.
func Host(name, domain string) string { return Hosts{Domain: domain}.App(name) }

// EnvKeys returns sorted env keys, including secret ones.
func (a *App) EnvKeys() []string {
	seen := map[string]bool{}
	for k := range a.Env {
		seen[k] = true
	}
	for _, k := range a.SecretKeys {
		seen[k] = true
	}
	keys := make([]string, 0, len(seen))
	for k := range seen {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// HasSecretKey reports whether key is a secret env key.
func (a *App) HasSecretKey(key string) bool {
	for _, k := range a.SecretKeys {
		if k == key {
			return true
		}
	}
	return false
}

// AddSecretKey records key as a secret env key.
func (a *App) AddSecretKey(key string) {
	if !a.HasSecretKey(key) {
		a.SecretKeys = append(a.SecretKeys, key)
		sort.Strings(a.SecretKeys)
	}
}

// RemoveSecretKey forgets a secret env key.
func (a *App) RemoveSecretKey(key string) {
	out := a.SecretKeys[:0]
	for _, k := range a.SecretKeys {
		if k != key {
			out = append(out, k)
		}
	}
	a.SecretKeys = out
}

// CurrentImage is the image the deployment should run, or "" if a repo app has no successful build yet.
func (a *App) CurrentImage() string {
	if a.TemplateDef != nil && len(a.TemplateDef.Components) > 0 {
		return a.TemplateDef.Components[0].Image
	}
	if a.Builds() {
		if a.Build != nil {
			return a.Build.LastGood
		}
		return ""
	}
	return a.Image
}

// ContainerPort is the port the app listens on.
func (a *App) ContainerPort() int {
	if a.Port > 0 {
		return a.Port
	}
	return 8080
}
