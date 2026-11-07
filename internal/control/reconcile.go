package control

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/melzi-cloud/sliced-bread/internal/apps"
	"github.com/melzi-cloud/sliced-bread/internal/kube"
)

// randID returns n lowercase alphanumeric characters.
func randID(n int) string {
	const alphabet = "abcdefghijklmnopqrstuvwxyz0123456789"
	b := make([]byte, n)
	rand.Read(b)
	for i := range b {
		b[i] = alphabet[int(b[i])%len(alphabet)]
	}
	return string(b)
}

// appSecret reads the per-app secret values.
func (s *Server) appSecret(ctx context.Context, a *apps.App) (map[string]string, error) {
	var sec kube.Obj
	err := s.kube.Get(ctx, "v1", "Secret", apps.Namespace(a.Name), apps.AppSecret, &sec)
	if kube.IsNotFound(err) {
		return map[string]string{}, nil
	}
	if err != nil {
		return nil, err
	}
	out := map[string]string{}
	data, _ := sec["data"].(map[string]any)
	for k, v := range data {
		s, _ := v.(string)
		b, _ := base64.StdEncoding.DecodeString(s)
		out[k] = string(b)
	}
	return out, nil
}

func (s *Server) writeAppSecret(ctx context.Context, a *apps.App, vals map[string]string) error {
	data := kube.Obj{}
	for k, v := range vals {
		data[k] = base64.StdEncoding.EncodeToString([]byte(v))
	}
	return s.kube.Apply(ctx, kube.Obj{
		"apiVersion": "v1", "kind": "Secret",
		"metadata": kube.Obj{"name": apps.AppSecret, "namespace": apps.Namespace(a.Name), "labels": kube.Obj{apps.ManagedByLabel: "slicedbread"}},
		"type":     "Opaque",
		"data":     data,
	})
}

func secretVersion(vals map[string]string) string {
	keys := make([]string, 0, len(vals))
	for k := range vals {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	h := sha256.New()
	for _, k := range keys {
		fmt.Fprintf(h, "%s=%s\n", k, vals[k])
	}
	return hex.EncodeToString(h.Sum(nil)[:8])
}

// setSecrets merges values into the app secret.
func (s *Server) setSecrets(ctx context.Context, a *apps.App, set map[string]string, unset []string) error {
	vals, err := s.appSecret(ctx, a)
	if err != nil {
		return err
	}
	for k, v := range set {
		vals[k] = v
	}
	for _, k := range unset {
		delete(vals, k)
	}
	return s.writeAppSecret(ctx, a, vals)
}

// reconcile applies an app's rendered objects. Callers hold s.mu or own the app.
func (s *Server) reconcile(ctx context.Context, a *apps.App) error {
	ns := apps.Namespace(a.Name)
	objs, err := apps.Render(a, s.params(a, ""))
	if err != nil {
		return err
	}
	// Namespace first, so the secret can be written before the deployment.
	if err := s.kube.Apply(ctx, objs[0]); err != nil {
		return fmt.Errorf("namespace: %w", err)
	}
	vals, err := s.appSecret(ctx, a)
	if err != nil {
		return err
	}
	changed := false
	for _, k := range a.Generated {
		if vals[k] == "" {
			vals[k] = randString(24)
			changed = true
		}
	}
	if changed || len(vals) == 0 {
		if err := s.writeAppSecret(ctx, a, vals); err != nil {
			return err
		}
	}
	objs, err = apps.Render(a, s.params(a, secretVersion(vals)))
	if err != nil {
		return err
	}
	for _, o := range objs[1:] {
		if err := s.kube.Apply(ctx, o); err != nil {
			return fmt.Errorf("apply %s %s: %w", o["kind"], o["metadata"].(map[string]any)["name"], err)
		}
	}
	// Prune objects that are no longer rendered.
	if !(a.Access == apps.AccessWall && len(a.PublicPaths) > 0) {
		s.kube.Delete(ctx, "networking.k8s.io/v1", "Ingress", ns, "web-public")
	}
	if !a.Database {
		s.kube.Delete(ctx, "postgresql.cnpg.io/v1", "Cluster", ns, "db")
	}
	s.syncAllowPolicies(ctx, a)
	if a.Builds() && (a.Build == nil || a.Build.Phase == "") {
		return s.startBuild(ctx, a)
	}
	return nil
}

// syncAllowPolicies keeps cross-app NetworkPolicies in step with ${apps.<x>.internal} references:
// outbound for this app (policies in the apps it calls) and inbound (apps that call it).
func (s *Server) syncAllowPolicies(ctx context.Context, a *apps.App) {
	want := map[string]bool{}
	for _, to := range apps.InternalRefs(a) {
		want[to] = true
		if err := s.kube.Apply(ctx, apps.AllowFromPolicy(a.Name, to)); err != nil && !kube.IsNotFound(err) {
			s.logf("allow %s -> %s: %v (retried when %s is reconciled)", a.Name, to, err, to)
		}
	}
	existing, err := s.kube.List(ctx, "networking.k8s.io/v1", "NetworkPolicy", "", apps.AllowFromLabel+"="+a.Name)
	if err == nil {
		for _, np := range existing {
			ns := kube.Str(np, "metadata", "namespace")
			to := strings.TrimPrefix(ns, "app-")
			if !want[to] {
				s.kube.Delete(ctx, "networking.k8s.io/v1", "NetworkPolicy", ns, kube.Str(np, "metadata", "name"))
			}
		}
	}
	list, err := s.listApps(ctx)
	if err != nil {
		return
	}
	for _, other := range list {
		for _, to := range apps.InternalRefs(other) {
			if to == a.Name && other.Name != a.Name {
				if err := s.kube.Apply(ctx, apps.AllowFromPolicy(other.Name, a.Name)); err != nil {
					s.logf("allow %s -> %s: %v", other.Name, a.Name, err)
				}
			}
		}
	}
}

// deleteApp removes an app and all its data.
func (s *Server) deleteApp(ctx context.Context, name string) error {
	if err := s.kube.Delete(ctx, "v1", "Namespace", "", apps.Namespace(name)); err != nil {
		return err
	}
	s.obsoleteApprovals(ctx, name, "app was deleted")
	if nps, err := s.kube.List(ctx, "networking.k8s.io/v1", "NetworkPolicy", "", apps.AllowFromLabel+"="+name); err == nil {
		for _, np := range nps {
			s.kube.Delete(ctx, "networking.k8s.io/v1", "NetworkPolicy", kube.Str(np, "metadata", "namespace"), kube.Str(np, "metadata", "name"))
		}
	}
	return s.kube.Delete(ctx, "slicedbread.dev/v1alpha1", "App", "", name)
}

// --- builds ---

func (s *Server) startBuild(ctx context.Context, a *apps.App) error {
	id := "b" + randID(6)
	push := s.cfg.RegistryPush + "/" + a.Name + ":" + id
	pull := s.cfg.RegistryPull + "/" + a.Name + ":" + id
	lastGood := ""
	if a.Build != nil {
		lastGood = a.Build.LastGood
	}
	a.Build = &apps.BuildState{ID: id, Phase: "running", Image: pull, LastGood: lastGood, StartedAt: time.Now().UTC().Format(time.RFC3339)}
	if err := s.kube.Apply(ctx, buildJob(s.cfg, a, id, push, s.uploadURL(a))); err != nil {
		a.Build.Phase, a.Build.Message = "failed", "could not create build job: "+err.Error()
	}
	return s.saveInternal(ctx, a)
}

func buildJob(cfg Config, a *apps.App, id, pushRef, uploadURL string) kube.Obj {
	prepare := `set -eu
apk add --no-cache git curl tar >/dev/null
mkdir -p /work/src /work/plan
if [ -n "$SB_UPLOAD_URL" ]; then
  echo "fetching uploaded source"
  curl -fsSL "$SB_UPLOAD_URL" | tar xz -C /work/src
else
  echo "cloning $SB_REPO ${SB_BRANCH:-(default branch)}"
  if [ -n "$SB_BRANCH" ]; then git clone --depth 1 --branch "$SB_BRANCH" "$SB_REPO" /work/src; else git clone --depth 1 "$SB_REPO" /work/src; fi
  git -C /work/src rev-parse --short HEAD > /work/plan/commit
fi
cd "/work/src/$SB_PATH"
if [ -f Dockerfile ]; then
  echo "Dockerfile found, building it"
  echo dockerfile > /work/plan/mode
else
  if [ -f package-lock.json ] && { [ -f bun.lockb ] || [ -f bun.lock ]; }; then
    echo "note: using npm (package-lock.json); the bun lockfile is ignored because Lovable exports often carry a stale one. No action needed."
    rm -f bun.lockb bun.lock
  fi
  mkdir -p /tmp/rp
  curl -fsSL "https://github.com/railwayapp/railpack/releases/download/$SB_RAILPACK/railpack-$SB_RAILPACK-x86_64-unknown-linux-musl.tar.gz" | tar xz -C /tmp/rp
  RP=$(find /tmp/rp -type f -name railpack | head -n1)
  "$RP" prepare . --plan-out /work/plan/railpack-plan.json --info-out /work/plan/info.json
  echo railpack > /work/plan/mode
fi
# The build container runs rootless as uid 1000.
chmod -R a+rwX /work
`
	// Each build runs its own rootless BuildKit (no shared daemon, no privileges); the
	// layer cache lives in the registry per app, so builds stay fast without sharing state.
	build := `set -eu
cd "/work/src/$SB_PATH"
CACHE="type=registry,ref=$SB_CACHE,registry.insecure=true"
if [ "$(cat /work/plan/mode)" = dockerfile ]; then
  set -- --frontend dockerfile.v0 --local context=. --local dockerfile=.
else
  set -- --frontend gateway.v0 --opt "source=ghcr.io/railwayapp/railpack-frontend:$SB_RAILPACK" --local context=. --local dockerfile=/work/plan
fi
buildctl-daemonless.sh build --progress plain "$@" \
  --import-cache "$CACHE" --export-cache "$CACHE,mode=max" \
  --output "type=image,name=$SB_IMAGE,push=true,registry.insecure=true" \
  --metadata-file /work/plan/metadata.json
digest=$(sed -n 's/.*"containerimage.digest": *"\(sha256:[0-9a-f]*\)".*/\1/p' /work/plan/metadata.json | head -n1)
if [ -z "$digest" ]; then echo "SB_ERROR build did not report an image digest"; exit 1; fi
echo "pushed $SB_IMAGE"
echo "SB_DIGEST $digest"
`
	repo, branch, path := "", "", "."
	if a.Repo != nil {
		repo, branch = a.Repo.URL, a.Repo.Branch
		if a.Repo.Path != "" {
			path = strings.Trim(a.Repo.Path, "/")
		}
	}
	env := []any{
		kube.Obj{"name": "SB_UPLOAD_URL", "value": uploadURL},
		kube.Obj{"name": "SB_REPO", "value": repo},
		kube.Obj{"name": "SB_BRANCH", "value": branch},
		kube.Obj{"name": "SB_PATH", "value": path},
		kube.Obj{"name": "SB_RAILPACK", "value": cfg.RailpackVersion},
		kube.Obj{"name": "SB_IMAGE", "value": pushRef},
		kube.Obj{"name": "SB_CACHE", "value": cfg.RegistryPush + "/" + a.Name + ":" + BuildCacheTag},
		kube.Obj{"name": "BUILDKITD_FLAGS", "value": "--oci-worker-no-process-sandbox"},
	}
	mounts := []any{kube.Obj{"name": "work", "mountPath": "/work"}}
	buildMounts := []any{kube.Obj{"name": "work", "mountPath": "/work"}, kube.Obj{"name": "buildkit", "mountPath": "/home/user/.local/share/buildkit"}}
	return kube.Obj{
		"apiVersion": "batch/v1", "kind": "Job",
		"metadata": kube.Obj{
			"name": "build-" + a.Name + "-" + id, "namespace": cfg.BuildNamespace,
			"labels": kube.Obj{apps.AppLabel: a.Name, "slicedbread.dev/build": id, apps.ManagedByLabel: "slicedbread"},
		},
		"spec": kube.Obj{
			"backoffLimit":            0,
			"activeDeadlineSeconds":   1800,
			"ttlSecondsAfterFinished": 86400,
			"template": kube.Obj{
				"metadata": kube.Obj{"labels": kube.Obj{apps.AppLabel: a.Name, "slicedbread.dev/build": id}},
				"spec": kube.Obj{
					"restartPolicy":                "Never",
					"automountServiceAccountToken": false,
					"initContainers": []any{kube.Obj{
						"name": "prepare", "image": "alpine:3.24.4", "command": []any{"sh", "-c", prepare},
						"env": env, "volumeMounts": mounts,
						"securityContext": kube.Obj{"allowPrivilegeEscalation": false},
					}},
					"containers": []any{kube.Obj{
						"name": "build", "image": BuildkitRootlessImage, "command": []any{"sh", "-c", build},
						"env": env, "volumeMounts": buildMounts,
						// Rootless BuildKit: non-root, not privileged. It needs unconfined seccomp and
						// AppArmor to create the user namespace its build steps run in.
						"securityContext": kube.Obj{
							"runAsUser": 1000, "runAsGroup": 1000, "runAsNonRoot": true,
							"seccompProfile":  kube.Obj{"type": "Unconfined"},
							"appArmorProfile": kube.Obj{"type": "Unconfined"},
						},
						"resources": kube.Obj{"requests": kube.Obj{"cpu": "250m", "memory": "512Mi"}, "limits": kube.Obj{"memory": "3Gi"}},
					}},
					"volumes": []any{kube.Obj{"name": "work", "emptyDir": kube.Obj{}}, kube.Obj{"name": "buildkit", "emptyDir": kube.Obj{}}},
				},
			},
		},
	}
}

// BuildkitRootlessImage builds every app in its own pod.
const BuildkitRootlessImage = "moby/buildkit:v0.34.2-rootless"

// BuildCacheTag holds each app's BuildKit layer cache in the registry.
const BuildCacheTag = "buildcache"

func (s *Server) buildJobName(a *apps.App) string { return "build-" + a.Name + "-" + a.Build.ID }

// checkBuild updates a running build. Returns true if the app changed.
func (s *Server) checkBuild(ctx context.Context, a *apps.App) bool {
	if !a.Builds() || a.Build == nil || a.Build.Phase != "running" {
		return false
	}
	var job kube.Obj
	if err := s.kube.Get(ctx, "batch/v1", "Job", s.cfg.BuildNamespace, s.buildJobName(a), &job); err != nil {
		if kube.IsNotFound(err) {
			a.Build.Phase, a.Build.Message = "failed", "build job disappeared"
			return true
		}
		return false
	}
	switch {
	case kube.Num(job, "status", "succeeded") > 0:
		digest := s.runOutput(ctx, s.cfg.BuildNamespace, s.buildJobName(a))["SB_DIGEST"]
		if !strings.HasPrefix(digest, "sha256:") {
			a.Build.Phase, a.Build.FinishedAt = "failed", time.Now().UTC().Format(time.RFC3339)
			a.Build.Message = "build finished without an image digest; see: sb logs " + a.Name + " --build"
			return true
		}
		// Deploy by digest: overwriting the tag in the registry cannot change what runs.
		a.Build.Image = strings.SplitN(a.Build.Image, "@", 2)[0] + "@" + digest
		a.Build.Phase = "succeeded"
		a.Build.LastGood = a.Build.Image
		a.Build.Message = ""
		a.Build.FinishedAt = time.Now().UTC().Format(time.RFC3339)
		return true
	case kube.Num(job, "status", "failed") > 0:
		a.Build.Phase = "failed"
		a.Build.FinishedAt = time.Now().UTC().Format(time.RFC3339)
		a.Build.Message = "build failed; see: sb logs " + a.Name + " --build"
		return true
	}
	return false
}

// buildLogs returns logs of the latest build's containers.
func (s *Server) buildLogs(ctx context.Context, a *apps.App, tail int) string {
	if a.Build == nil {
		return "no build yet\n"
	}
	pods, err := s.kube.List(ctx, "v1", "Pod", s.cfg.BuildNamespace, "slicedbread.dev/build="+a.Build.ID)
	if err != nil || len(pods) == 0 {
		return fmt.Sprintf("no build pod found for build %s (%v)\n", a.Build.ID, err)
	}
	name := kube.Str(pods[0], "metadata", "name")
	var b strings.Builder
	for _, c := range []string{"prepare", "build"} {
		l, err := s.kube.Logs(ctx, s.cfg.BuildNamespace, name, c, tail)
		fmt.Fprintf(&b, "--- %s ---\n%s", c, l)
		if err != nil {
			fmt.Fprintf(&b, "(%v)\n", err)
		}
	}
	return b.String()
}

// --- background loop ---

func (s *Server) tick(ctx context.Context) {
	ctx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()
	list, err := s.listApps(ctx)
	if err != nil {
		s.logf("tick: list apps: %v", err)
		return
	}
	bc, err := s.backupConfig(ctx)
	if err != nil {
		s.logf("tick: backup config: %v", err)
	}
	s.ensureRegistryPolicy(ctx)
	s.maybeRegistryGC(ctx, list)
	for _, a := range list {
		s.mu.Lock()
		cur, err := s.getApp(ctx, a.Name)
		if err != nil {
			s.mu.Unlock()
			continue
		}
		if s.checkBuild(ctx, cur) {
			if err := s.saveInternal(ctx, cur); err != nil {
				s.logf("tick: save %s: %v", cur.Name, err)
			} else if err := s.reconcile(ctx, cur); err != nil {
				s.logf("tick: reconcile %s: %v", cur.Name, err)
			}
		}
		if changed := s.runHooks(ctx, cur); changed {
			if err := s.saveInternal(ctx, cur); err != nil {
				s.logf("tick: save hooks %s: %v", cur.Name, err)
			}
		}
		if s.checkBackups(ctx, cur, bc) {
			if err := s.saveInternal(ctx, cur); err != nil {
				s.logf("tick: save backups %s: %v", cur.Name, err)
			} else if err := s.reconcile(ctx, cur); err != nil {
				s.logf("tick: reconcile %s: %v", cur.Name, err)
			}
		}
		s.mu.Unlock()
	}
	if fresh, err := s.listApps(ctx); err == nil {
		s.evaluateAlerts(ctx, fresh, bc)
	}
}

// --- status ---

// AppStatus is what the CLI shows for an app.
type AppStatus struct {
	Name         string            `json:"name"`
	URL          string            `json:"url"`
	Template     string            `json:"template,omitempty"`
	Source       string            `json:"source"`
	Access       string            `json:"access"`
	PublicPaths  []string          `json:"publicPaths,omitempty"`
	State        string            `json:"state"`
	Message      string            `json:"message,omitempty"`
	Build        *apps.BuildState  `json:"build,omitempty"`
	EnvKeys      []string          `json:"envKeys,omitempty"`
	Env          map[string]string `json:"env,omitempty"` // plain values; secrets are only listed in secretKeys
	Pending      []string          `json:"pendingApprovals,omitempty"`
	Storage      string            `json:"storage,omitempty"`
	Serving      string            `json:"serving,omitempty"`      // image currently deployed
	ServingBuild string            `json:"servingBuild,omitempty"` // build id currently deployed (source apps)
	SecretKeys   []string          `json:"secretKeys,omitempty"`
	Settings     map[string]string `json:"settings,omitempty"`
	Actions      []apps.Action     `json:"actions,omitempty"`
	Database     string            `json:"database,omitempty"`
	Notes        string            `json:"notes,omitempty"`
	Backups      string            `json:"backups,omitempty"`
	BackupShort  string            `json:"-"`
}

func (s *Server) status(ctx context.Context, a *apps.App) AppStatus {
	st := AppStatus{
		Name: a.Name, URL: "https://" + s.hosts().App(a.Name), Template: a.Template,
		Access: a.Access, PublicPaths: a.PublicPaths, Build: a.Build, Settings: a.Settings, SecretKeys: a.SecretKeys,
	}
	st.Env = map[string]string{}
	for k, v := range a.Env {
		st.EnvKeys = append(st.EnvKeys, k)
		st.Env[k] = v
	}
	sort.Strings(st.EnvKeys)
	st.Pending = s.pendingFor(ctx, a.Name)
	var vols []string
	for _, v := range a.Volumes {
		vols = append(vols, v.Path)
	}
	if a.Database {
		vols = append(vols, "postgres")
	}
	if len(vols) > 0 {
		st.Storage = "persistent volumes (" + strings.Join(vols, ", ") + ") survive restarts and redeploys; deleting the app deletes them"
		st.Backups = backupSummary(a)
	}
	st.BackupShort = backupShort(a, time.Now())
	switch {
	case a.Template != "":
		st.Source = "template " + a.Template
	case a.Upload != nil:
		st.Source = fmt.Sprintf("uploaded directory (%s, %d KB, content sha256 %s)", a.Upload.ID, a.Upload.Bytes/1024, a.Upload.SHA256)
	case a.Repo != nil:
		st.Source = "repo " + a.Repo.URL
		if a.Repo.Branch != "" {
			st.Source += "@" + a.Repo.Branch
		}
		if a.Repo.Path != "" {
			st.Source += " path " + a.Repo.Path
		}
	default:
		st.Source = "image " + a.Image
	}
	if t := apps.Catalog[a.Template]; t != nil {
		st.Actions, st.Notes = t.Actions, t.Notes
	}
	ns := apps.Namespace(a.Name)
	if a.Database {
		var c kube.Obj
		if err := s.kube.Get(ctx, "postgresql.cnpg.io/v1", "Cluster", ns, "db", &c); err == nil {
			st.Database = kube.Str(c, "status", "phase")
		} else {
			st.Database = "pending"
		}
	}
	if a.Builds() && a.Build != nil && a.Build.LastGood == "" {
		switch a.Build.Phase {
		case "running":
			st.State, st.Message = "building", "first build in progress (usually under 5 minutes, faster when cached); follow with: sb logs "+a.Name+" --build"
		case "failed":
			st.State, st.Message = "build-failed", a.Build.Message
		}
		return st
	}
	if a.Paused {
		st.State, st.Message = "paused", "stopped while a backup or restore copies its data; it starts again by itself (see: sb backups "+a.Name+")"
		return st
	}
	var dep kube.Obj
	if err := s.kube.Get(ctx, "apps/v1", "Deployment", ns, "web", &dep); err != nil {
		st.State, st.Message = "pending", "deployment not created yet"
		return st
	}
	st.Serving = servingImage(dep)
	if a.Upload != nil && a.Build != nil && a.Build.LastGood != "" {
		st.ServingBuild = buildIDFromImage(st.Serving)
	}
	want := kube.Num(dep, "spec", "replicas")
	settled := kube.Num(dep, "status", "observedGeneration") >= kube.Num(dep, "metadata", "generation") &&
		kube.Num(dep, "status", "updatedReplicas") == want &&
		kube.Num(dep, "status", "readyReplicas") == want &&
		kube.Num(dep, "status", "unavailableReplicas") == 0 &&
		kube.Num(dep, "status", "replicas") == want &&
		(a.CurrentImage() == "" || st.Serving == a.CurrentImage())
	if settled {
		// Pods of the previous version may still be shutting down and serving open connections.
		if n := s.terminatingPods(ctx, ns, a.Name); n > 0 {
			settled = false
		}
	}
	ready := kube.Num(dep, "status", "readyReplicas") > 0
	switch {
	case a.Build != nil && a.Build.Phase == "running":
		st.State, st.Message = "updating", "new build "+a.Build.ID+" running; the previous version keeps serving"
	case !settled && ready:
		st.State, st.Message = "updating", "rolling out the new version; the previous version keeps serving until it is ready"
		if p := s.podProblem(ctx, ns); p != "" {
			st.Message += " (new version: " + p + ")"
		}
	case !settled:
		st.State = "starting"
		st.Message = s.podProblem(ctx, ns)
		if st.Message == "" && a.Database && st.Database != "Cluster in healthy state" {
			st.Message = "waiting for database: " + st.Database
		}
	case a.Build != nil && a.Build.Phase == "failed" && a.Build.LastGood != "":
		st.State, st.Message = "update-failed", "latest build "+a.Build.ID+" failed; the previous version is still serving. See: sb logs "+a.Name+" --build"
	case a.Template == "umami" && a.Hooks["admin-password"] != "set":
		st.State, st.Message = "configuring", "finishing template setup (replacing the default admin password)"
	default:
		st.State = "running"
	}
	return st
}

// terminatingPods counts app pods that are shutting down.
func (s *Server) terminatingPods(ctx context.Context, ns, app string) int {
	pods, err := s.kube.List(ctx, "v1", "Pod", ns, apps.AppLabel+"="+app)
	if err != nil {
		return 0
	}
	n := 0
	for _, p := range pods {
		if kube.Str(p, "metadata", "deletionTimestamp") != "" {
			n++
		}
	}
	return n
}

func servingImage(dep kube.Obj) string {
	for _, c := range kube.Slice(dep, "spec", "template", "spec", "containers") {
		m, _ := c.(map[string]any)
		if kube.Str(m, "name") == "app" {
			return kube.Str(m, "image")
		}
	}
	return ""
}

func buildIDFromImage(img string) string {
	img = strings.SplitN(img, "@", 2)[0] // drop the digest
	if i := strings.LastIndex(img, ":"); i >= 0 && !strings.Contains(img[i:], "/") {
		return img[i+1:]
	}
	return ""
}

// podProblem summarises why pods are not ready.
func (s *Server) podProblem(ctx context.Context, ns string) string {
	pods, err := s.kube.List(ctx, "v1", "Pod", ns, apps.AppLabel)
	if err != nil {
		return ""
	}
	for _, p := range pods {
		for _, cs := range kube.Slice(p, "status", "containerStatuses") {
			m, _ := cs.(map[string]any)
			if w := kube.Str(m, "state", "waiting", "reason"); w != "" {
				msg := kube.Str(m, "state", "waiting", "message")
				if len(msg) > 200 {
					msg = msg[:200]
				}
				return strings.TrimSpace(w + " " + msg)
			}
			if r := kube.Num(m, "restartCount"); r > 2 {
				return fmt.Sprintf("container restarted %d times; see: sb logs", r)
			}
		}
		for _, c := range kube.Slice(p, "status", "conditions") {
			m, _ := c.(map[string]any)
			if kube.Str(m, "type") == "PodScheduled" && kube.Str(m, "status") == "False" {
				return "pod not scheduled: " + kube.Str(m, "message")
			}
		}
	}
	return ""
}

// appPod returns the name of a running app pod.
func (s *Server) appPod(ctx context.Context, a *apps.App) (string, error) {
	pods, err := s.kube.List(ctx, "v1", "Pod", apps.Namespace(a.Name), apps.AppLabel+"="+a.Name)
	if err != nil {
		return "", err
	}
	if len(pods) == 0 {
		return "", fmt.Errorf("no pods for %s", a.Name)
	}
	sort.Slice(pods, func(i, j int) bool {
		return kube.Str(pods[i], "metadata", "creationTimestamp") > kube.Str(pods[j], "metadata", "creationTimestamp")
	})
	return kube.Str(pods[0], "metadata", "name"), nil
}
