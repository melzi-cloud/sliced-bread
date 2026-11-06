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
	Generated   []string          `json:"generated,omitempty"` // secret keys generated on first deploy
	Build       *BuildState       `json:"build,omitempty"`
	Hooks       map[string]string `json:"hooks,omitempty"` // template hook progress, e.g. umami admin password set
	Generation  int64             `json:"generation,omitempty"`
}

// Repo is a git source built with Railpack (or its Dockerfile if present).
type Repo struct {
	URL    string `json:"url"`
	Branch string `json:"branch,omitempty"`
	Path   string `json:"path,omitempty"`
}

// Upload is source code uploaded by the CLI and stored by the control plane.
type Upload struct {
	ID    string `json:"id"`
	Bytes int64  `json:"bytes,omitempty"`
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
	for _, p := range a.PublicPaths {
		if !strings.HasPrefix(p, "/") || strings.ContainsAny(p, " \t\n`\"'") {
			return fmt.Errorf("invalid public path %q", p)
		}
	}
	return nil
}

// Namespace returns the Kubernetes namespace for an app.
func Namespace(name string) string { return "app-" + name }

// Host returns the public hostname for an app in a tenant domain.
func Host(name, domain string) string { return name + "." + domain }

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
