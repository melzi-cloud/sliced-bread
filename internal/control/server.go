// Package control is the SlicedBread control plane that runs inside a tenant cluster.
// It serves the API used by the CLI, the login wall for apps, and reconciles apps.
package control

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/melzi-cloud/sliced-bread/internal/apps"
	"github.com/melzi-cloud/sliced-bread/internal/kube"
)

// Config comes from environment variables set by the platform manifests.
type Config struct {
	Namespace        string // where sb-control runs, e.g. sb-system
	Domain           string // tenant domain
	AdminEmail       string
	RegistryPush     string // registry address used by builds
	RegistryPull     string // registry address used by nodes
	BuildNamespace   string
	BuildkitAddr     string
	RailpackVersion  string
	InternalHostname string // Host header of in-cluster forward-auth calls
	Listen           string
	DataDir          string // persistent volume for uploads
}

// ConfigFromEnv reads Config.
func ConfigFromEnv() Config {
	get := func(k, d string) string {
		if v := os.Getenv(k); v != "" {
			return v
		}
		return d
	}
	return Config{
		Namespace:        get("SB_NAMESPACE", "sb-system"),
		Domain:           os.Getenv("SB_DOMAIN"),
		AdminEmail:       get("SB_ADMIN_EMAIL", "admin@example.com"),
		RegistryPush:     get("SB_REGISTRY_PUSH", "sb-registry.sb-system.svc.cluster.local:5000"),
		RegistryPull:     get("SB_REGISTRY_PULL", "sb-registry.local:5000"),
		BuildNamespace:   get("SB_BUILD_NAMESPACE", "sb-builds"),
		BuildkitAddr:     get("SB_BUILDKIT_ADDR", "tcp://buildkitd.sb-builds.svc.cluster.local:1234"),
		RailpackVersion:  get("SB_RAILPACK_VERSION", "v0.43.0"),
		InternalHostname: get("SB_INTERNAL_HOST", "sb-control.sb-system.svc.cluster.local"),
		Listen:           get("SB_LISTEN", ":8080"),
		DataDir:          get("SB_DATA_DIR", "/data"),
	}
}

// Server is the control plane.
type Server struct {
	cfg     Config
	kube    *kube.Client
	auth    *Auth
	devices deviceStore
	mu      sync.Mutex // serialises writes to app specs
	logf    func(string, ...any)

	registryPolicyKey string
	registryPolicyAt  time.Time
	registryGCAt      time.Time
	alertsAt          time.Time
	alertsCache       []Alert
	fallback          fallbackState
	isLeader          bool
	draining          atomic.Bool
	imageCheckAt      time.Time
	sharedGCAt        time.Time
}

// New creates a server and loads or bootstraps auth state.
func New(ctx context.Context, cfg Config, k *kube.Client) (*Server, error) {
	if cfg.Domain == "" {
		return nil, fmt.Errorf("SB_DOMAIN is required")
	}
	a, err := LoadAuth(ctx, k, cfg.Namespace, cfg.Domain)
	if err != nil {
		return nil, fmt.Errorf("auth: %w", err)
	}
	return &Server{cfg: cfg, kube: k, auth: a, logf: log.Printf, devices: deviceStore{grants: map[string]*deviceGrant{}}}, nil
}

func (s *Server) hosts() apps.Hosts { return apps.Hosts{Domain: s.cfg.Domain} }

func (s *Server) params(a *apps.App, secretVersion string) apps.Params {
	return apps.Params{Domain: s.cfg.Domain, AuthService: "http://" + s.cfg.InternalHostname + ":8080", SecretVersion: secretVersion}
}

// Handler routes by host: internal forward-auth calls, api.<domain>, auth.<domain>.
func (s *Server) Handler() http.Handler {
	api := s.apiMux()
	ui := s.uiMux()
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		host := strings.ToLower(strings.Split(r.Host, ":")[0])
		switch {
		case r.URL.Path == "/healthz":
			if s.draining.Load() {
				http.Error(w, "shutting down", http.StatusServiceUnavailable)
				return
			}
			w.Write([]byte("ok\n"))
		case host == s.cfg.InternalHostname || host == "sb-control" || host == "sb-control.sb-system" || host == "sb-control.sb-system.svc":
			switch r.URL.Path {
			case "/auth/verify":
				s.handleVerify(w, r)
			case "/auth/strip":
				s.handleStrip(w, r)
			default:
				if strings.HasPrefix(r.URL.Path, "/internal/uploads/") {
					s.handleInternalUpload(w, r)
					return
				}
				if strings.HasPrefix(r.URL.Path, "/internal/restore-source/") {
					s.handleRestoreSource(w, r)
					return
				}
				http.NotFound(w, r)
			}
		case host == s.hosts().API():
			api.ServeHTTP(w, r)
		case host == s.hosts().Auth():
			ui.ServeHTTP(w, r)
		default:
			http.NotFound(w, r)
		}
	})
}

// Run serves HTTP and runs the reconcile loop until ctx is done.
func (s *Server) Run(ctx context.Context) error {
	srv := &http.Server{Addr: s.cfg.Listen, Handler: s.Handler(), ReadHeaderTimeout: 10 * time.Second}
	go s.loop(ctx)
	go func() {
		<-ctx.Done()
		// Fail readiness first and keep serving a few seconds, so Traefik and kube-proxy
		// stop sending login-wall checks here before the listener closes (a refused check
		// shows as a 500 on the app).
		s.draining.Store(true)
		time.Sleep(6 * time.Second)
		sctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		srv.Shutdown(sctx)
	}()
	s.logf("sb-control listening on %s for %s", s.cfg.Listen, s.cfg.Domain)
	err := srv.ListenAndServe()
	if err == http.ErrServerClosed {
		return nil
	}
	return err
}

func (s *Server) loop(ctx context.Context) {
	t := time.NewTicker(8 * time.Second)
	defer t.Stop()
	for {
		// Every replica keeps its copy of the auth state current; only the lease holder
		// runs background work (builds, backups, alerts, registry upkeep).
		s.auth.refresh(ctx)
		if s.leader(ctx) {
			s.tick(ctx)
			if time.Since(s.sharedGCAt) > 5*time.Minute {
				s.gcShared(ctx)
				s.sharedGCAt = time.Now()
			}
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

// --- app store (App custom resources) ---

type appCR struct {
	Metadata struct {
		Name            string `json:"name"`
		ResourceVersion string `json:"resourceVersion"`
	} `json:"metadata"`
	Spec apps.App `json:"spec"`
}

func (s *Server) getApp(ctx context.Context, name string) (*apps.App, error) {
	var cr appCR
	if err := s.kube.Get(ctx, "slicedbread.dev/v1alpha1", "App", "", name, &cr); err != nil {
		return nil, err
	}
	a := cr.Spec
	a.Name = cr.Metadata.Name
	return &a, nil
}

func (s *Server) listApps(ctx context.Context) ([]*apps.App, error) {
	items, err := s.kube.List(ctx, "slicedbread.dev/v1alpha1", "App", "", "")
	if err != nil {
		return nil, err
	}
	var out []*apps.App
	for _, it := range items {
		b, _ := json.Marshal(it)
		var cr appCR
		if json.Unmarshal(b, &cr) == nil {
			a := cr.Spec
			a.Name = cr.Metadata.Name
			out = append(out, &a)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

// saveApp records a change a person or agent asked for. It bumps the generation,
// which makes pending approvals for the app obsolete.
func (s *Server) saveApp(ctx context.Context, a *apps.App) error {
	a.Generation++
	if err := s.putApp(ctx, a); err != nil {
		return err
	}
	s.obsoleteApprovals(ctx, a.Name, "the app changed after this was requested; ask again")
	return nil
}

// saveInternal records control-plane bookkeeping (build progress, hook state) without bumping the generation.
func (s *Server) saveInternal(ctx context.Context, a *apps.App) error { return s.putApp(ctx, a) }

func (s *Server) putApp(ctx context.Context, a *apps.App) error {
	b, _ := json.Marshal(a)
	var spec map[string]any
	json.Unmarshal(b, &spec)
	return s.kube.Apply(ctx, kube.Obj{
		"apiVersion": "slicedbread.dev/v1alpha1", "kind": "App",
		"metadata": kube.Obj{"name": a.Name, "labels": kube.Obj{apps.ManagedByLabel: "slicedbread"}},
		"spec":     spec,
	})
}

// --- approvals (Approval custom resources) ---

// Approval is a pending privileged change requested with an agent token.
type Approval struct {
	ID          string    `json:"id"`
	App         string    `json:"app"`
	Kind        string    `json:"kind"`               // create, update, delete, restore
	Snapshot    string    `json:"snapshot,omitempty"` // restore only
	Summary     string    `json:"summary"`            // human readable
	Desired     *apps.App `json:"desired,omitempty"`
	BaseGen     int64     `json:"baseGeneration"` // app generation the request was based on; 0 for create
	RequestedBy string    `json:"requestedBy,omitempty"`
	RequestedAt string    `json:"requestedAt"`
	State       string    `json:"state"` // pending, approved, denied
	Result      string    `json:"result,omitempty"`
}

func (s *Server) saveApproval(ctx context.Context, ap *Approval) error {
	b, _ := json.Marshal(ap)
	var spec map[string]any
	json.Unmarshal(b, &spec)
	return s.kube.Apply(ctx, kube.Obj{
		"apiVersion": "slicedbread.dev/v1alpha1", "kind": "Approval",
		"metadata": kube.Obj{"name": ap.ID},
		"spec":     spec,
	})
}

func (s *Server) listApprovals(ctx context.Context) ([]*Approval, error) {
	items, err := s.kube.List(ctx, "slicedbread.dev/v1alpha1", "Approval", "", "")
	if err != nil {
		return nil, err
	}
	var out []*Approval
	for _, it := range items {
		b, _ := json.Marshal(it["spec"])
		var ap Approval
		if json.Unmarshal(b, &ap) == nil {
			out = append(out, &ap)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].RequestedAt > out[j].RequestedAt })
	return out, nil
}

func (s *Server) getApproval(ctx context.Context, id string) (*Approval, error) {
	var cr struct {
		Spec Approval `json:"spec"`
	}
	if err := s.kube.Get(ctx, "slicedbread.dev/v1alpha1", "Approval", "", id, &cr); err != nil {
		return nil, err
	}
	return &cr.Spec, nil
}

func (s *Server) requestApproval(ctx context.Context, kind, app, summary string, desired *apps.App, scope string) (*Approval, error) {
	ap := &Approval{
		ID: "ap-" + randID(6), App: app, Kind: kind, Summary: summary,
		Desired: desired, RequestedAt: time.Now().UTC().Format(time.RFC3339), State: "pending",
		RequestedBy: scope + " token",
	}
	if cur, err := s.getApp(ctx, app); err == nil {
		ap.BaseGen = cur.Generation
	}
	return ap, s.saveApproval(ctx, ap)
}

// obsoleteApprovals marks pending approvals for an app as obsolete (e.g. after delete).
func (s *Server) obsoleteApprovals(ctx context.Context, app, reason string) {
	list, err := s.listApprovals(ctx)
	if err != nil {
		return
	}
	for _, ap := range list {
		if ap.App == app && ap.State == "pending" {
			ap.State, ap.Result = "obsolete", reason
			s.saveApproval(ctx, ap)
		}
	}
}

// pendingFor lists pending approval summaries for an app.
func (s *Server) pendingFor(ctx context.Context, app string) []string {
	list, _ := s.listApprovals(ctx)
	var out []string
	for _, ap := range list {
		if ap.App == app && ap.State == "pending" {
			out = append(out, ap.ID+": "+ap.Summary)
		}
	}
	return out
}

// executeApproval applies an approved change.
func (s *Server) executeApproval(ctx context.Context, ap *Approval) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	var err error
	cur, gerr := s.getApp(ctx, ap.App)
	// Refuse stale approvals: the app changed, was deleted, or was recreated since the request.
	stale := ""
	switch {
	case (ap.Kind == "create" || ap.Kind == "recreate") && gerr == nil:
		stale = "an app with this name exists now"
	case ap.Kind == "recreate":
	case ap.Kind != "create" && gerr != nil:
		stale = "the app no longer exists"
	case ap.Kind != "create" && cur.Generation != ap.BaseGen:
		stale = "the app changed after this was requested; ask again"
	}
	if stale != "" {
		ap.State, ap.Result = "obsolete", stale
		s.saveApproval(ctx, ap)
		return fmt.Errorf("approval %s is obsolete: %s", ap.ID, stale)
	}
	switch ap.Kind {
	case "delete":
		err = s.deleteApp(ctx, ap.App)
	case "restore":
		_, err = s.startBackupRun(ctx, cur, "restore", "approved "+ap.ID, ap.Snapshot)
	case "recreate":
		_, err = s.recreateLocked(ctx, map[string]string{ap.App: ap.Snapshot})
	case "create", "update":
		if ap.Desired == nil {
			return fmt.Errorf("approval has no desired state")
		}
		if gerr == nil {
			// Approvals change access or settings, never the source: keep whatever is deployed now.
			ap.Desired.Build, ap.Desired.Hooks, ap.Desired.Generation = cur.Build, cur.Hooks, cur.Generation
			ap.Desired.Upload, ap.Desired.Repo = cur.Upload, cur.Repo
			ap.Desired.Backup, ap.Desired.Paused = cur.Backup, cur.Paused
			if cur.Template == "" {
				ap.Desired.Image = cur.Image
			}
		}
		if err = s.saveApp(ctx, ap.Desired); err == nil {
			err = s.reconcile(ctx, ap.Desired)
		}
	default:
		err = fmt.Errorf("unknown approval kind %q", ap.Kind)
	}
	ap.State = "approved"
	ap.Result = "applied"
	if err != nil {
		ap.Result = "error: " + err.Error()
	}
	if serr := s.saveApproval(ctx, ap); serr != nil {
		return serr
	}
	return err
}
