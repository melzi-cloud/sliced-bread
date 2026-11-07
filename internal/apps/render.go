package apps

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

// Obj is a Kubernetes object as unstructured JSON.
type Obj = map[string]any

// Params are tenant-level values needed to render an app.
type Params struct {
	Domain        string // tenant domain, e.g. r1234567.test.slicedbread.dev
	AuthService   string // base URL of sb-control inside the cluster, e.g. http://sb-control.sb-system.svc.cluster.local:8080
	SecretVersion string // changes when the app secret changes, to roll pods
	// Values of the app secret and of the database secret, only needed for config files
	// (env refers to Kubernetes Secrets instead). Nil when unknown.
	Values map[string]string
	DB     map[string]string // host, port, user, password, name, url
}

const (
	AppSecret      = "app-env" // per-app secret holding generated and provided values
	DBSecret       = "db-app"  // created by CloudNativePG
	ManagedByLabel = "app.kubernetes.io/managed-by"
	AppLabel       = "slicedbread.dev/app"
	ComponentLabel = "slicedbread.dev/component"
	ActionLabel    = "slicedbread.dev/action"
	// EnterPath on an app host passes the login wall, then redirects to ?to=<path>.
	EnterPath = "/__sb/enter"
	// sbPaths are SlicedBread's own paths on app hosts; they always go through the wall.
	sbPaths = "/__sb/"
)

func meta(kind, apiVersion, ns, name string, labels map[string]string) Obj {
	m := Obj{"name": name}
	if ns != "" {
		m["namespace"] = ns
	}
	l := map[string]any{ManagedByLabel: "slicedbread"}
	for k, v := range labels {
		l[k] = v
	}
	m["labels"] = l
	return Obj{"apiVersion": apiVersion, "kind": kind, "metadata": m}
}

// Components returns the app's containers: the template's, or one "web" component for
// apps deployed from an image, a repo or a directory. The first component gets the
// app's user env, secret env and settings env.
func Components(a *App) []Component {
	if a.TemplateDef != nil {
		t := a.TemplateDef
		out := make([]Component, len(t.Components))
		for i, c := range t.Components {
			c.Env = copyEnv(c.Env)
			if i == 0 {
				for _, s := range t.Settings {
					for k, v := range s.Env {
						c.Env[k] = v
					}
				}
				for k, v := range a.Env {
					c.Env[k] = Str(v)
				}
			}
			out[i] = c
		}
		return out
	}
	c := Component{Name: "web", Image: a.CurrentImage(), Port: a.ContainerPort(), Replicas: 1, Volumes: a.Volumes, RunAsUser: a.RunAsUser, Env: map[string]Str{}}
	if a.HealthPath != "" {
		c.Health = &Health{Path: a.HealthPath}
	}
	for k, v := range a.Env {
		c.Env[k] = Str(v)
	}
	return []Component{c}
}

func copyEnv(in map[string]Str) map[string]Str {
	out := map[string]Str{}
	for k, v := range in {
		out[k] = v
	}
	return out
}

// Primary is the component that serves the app's host by default.
func Primary(a *App) Component { return Components(a)[0] }

var refInText = regexp.MustCompile(`\$\{([^}]*)\}`)

// resolver turns references into values.
type resolver struct {
	a      *App
	p      Params
	inFile bool
}

func (r resolver) value(ref string) (string, error) {
	a, p := r.a, r.p
	ns := Namespace(a.Name)
	parts := strings.Split(ref, ".")
	switch {
	case ref == "app.url":
		return "https://" + Host(a.Name, p.Domain), nil
	case ref == "app.host":
		return Host(a.Name, p.Domain), nil
	case ref == "db.host":
		return "db-rw." + ns + ".svc.cluster.local", nil
	case ref == "db.port":
		return "5432", nil
	case ref == "db.user", ref == "db.name":
		return "app", nil
	case ref == "db.url" || ref == "db.password":
		if !r.inFile {
			return "", fmt.Errorf("${%s} must be the whole value", ref)
		}
		v, ok := p.DB[strings.TrimPrefix(ref, "db.")]
		if !ok {
			return "", errNotReady
		}
		return v, nil
	case parts[0] == "secret" && len(parts) == 2:
		if !r.inFile {
			return "", fmt.Errorf("${%s} must be the whole value", ref)
		}
		v, ok := p.Values[parts[1]]
		if !ok {
			return "", errNotReady
		}
		return v, nil
	case parts[0] == "setting" && len(parts) == 2:
		return a.Settings[parts[1]], nil
	case parts[0] == "component" && len(parts) == 3:
		for _, c := range Components(a) {
			if c.Name == parts[1] && c.Port > 0 {
				host := ServiceName(a, c.Name) + "." + ns + ".svc.cluster.local"
				switch parts[2] {
				case "host":
					return host, nil
				case "port":
					return strconv.Itoa(c.Port), nil
				case "internal":
					return "http://" + host + ":" + strconv.Itoa(c.Port), nil
				}
			}
		}
	case parts[0] == "apps" && len(parts) == 3 && parts[2] == "url":
		return "https://" + Host(parts[1], p.Domain), nil
	case parts[0] == "apps" && len(parts) == 3 && parts[2] == "internal":
		return "http://web." + Namespace(parts[1]) + ".svc.cluster.local", nil
	}
	return "", fmt.Errorf("unknown reference ${%s}", ref)
}

var errNotReady = fmt.Errorf("not ready")

func (r resolver) text(s string) (string, error) {
	var err error
	out := refInText.ReplaceAllStringFunc(s, func(m string) string {
		v, e := r.value(m[2 : len(m)-1])
		if e != nil && err == nil {
			err = e
		}
		return v
	})
	return out, err
}

// ServiceName is the Service of a component.
func ServiceName(a *App, comp string) string { return comp }

// resolveEnv turns a component's env into container env entries.
func resolveEnv(a *App, c Component, primary bool, p Params) ([]any, error) {
	keys := make([]string, 0, len(c.Env))
	for k := range c.Env {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var out []any
	if primary && a.Builds() {
		if _, ok := c.Env["PORT"]; !ok {
			out = append(out, Obj{"name": "PORT", "value": fmt.Sprint(c.Port)})
		}
	}
	r := resolver{a: a, p: p}
	for _, k := range keys {
		v := strings.TrimSpace(string(c.Env[k]))
		if m := refInText.FindStringSubmatch(v); m != nil && m[0] == v {
			ref := m[1]
			switch {
			case ref == "db.url" || ref == "db.password":
				if !a.Database {
					return nil, fmt.Errorf("env %s references ${%s} but the app has no database", k, ref)
				}
				key := "uri"
				if ref == "db.password" {
					key = "password"
				}
				out = append(out, Obj{"name": k, "valueFrom": Obj{"secretKeyRef": Obj{"name": DBSecret, "key": key}}})
				continue
			case strings.HasPrefix(ref, "secret."):
				out = append(out, Obj{"name": k, "valueFrom": Obj{"secretKeyRef": Obj{"name": AppSecret, "key": strings.TrimPrefix(ref, "secret."), "optional": true}}})
				continue
			}
		}
		val, err := r.text(string(c.Env[k]))
		if err != nil {
			return nil, fmt.Errorf("env %s: %v", k, err)
		}
		out = append(out, Obj{"name": k, "value": val})
	}
	if primary {
		for _, k := range a.SecretKeys {
			if _, plain := c.Env[k]; plain {
				continue
			}
			out = append(out, Obj{"name": k, "valueFrom": Obj{"secretKeyRef": Obj{"name": AppSecret, "key": k, "optional": true}}})
		}
	}
	return out, nil
}

// Render returns the Kubernetes objects for an app. The app secret is not
// rendered here because it holds values; the control plane manages it.
func Render(a *App, p Params) ([]Obj, error) {
	if err := a.Validate(); err != nil {
		return nil, err
	}
	ns := Namespace(a.Name)
	lbl := map[string]string{AppLabel: a.Name}
	var objs []Obj

	nsObj := meta("Namespace", "v1", "", ns, lbl)
	nsObj["metadata"].(Obj)["labels"].(map[string]any)["pod-security.kubernetes.io/enforce"] = "baseline"
	nsObj["metadata"].(Obj)["labels"].(map[string]any)["pod-security.kubernetes.io/warn"] = "restricted"
	objs = append(objs, nsObj)

	// Isolation: only Traefik, the control plane, the Postgres operator and the app itself may connect.
	np := meta("NetworkPolicy", "networking.k8s.io/v1", ns, "sb-isolation", lbl)
	from := func(nsName string) Obj {
		return Obj{"from": []any{Obj{"namespaceSelector": Obj{"matchLabels": Obj{"kubernetes.io/metadata.name": nsName}}}}}
	}
	np["spec"] = Obj{
		"podSelector": Obj{},
		"policyTypes": []any{"Ingress"},
		"ingress": []any{
			Obj{"from": []any{Obj{"podSelector": Obj{}}}},
			from("kube-system"), from("sb-system"), from("cnpg-system"),
		},
	}
	objs = append(objs, np)
	objs = append(objs, actionEgressPolicy(ns, lbl))

	for _, mw := range []struct{ name, path string }{{"sb-wall", "/auth/verify"}, {"sb-strip", "/auth/strip"}} {
		m := meta("Middleware", "traefik.io/v1alpha1", ns, mw.name, lbl)
		m["spec"] = Obj{"forwardAuth": Obj{
			"address":             p.AuthService + mw.path + "?app=" + a.Name,
			"trustForwardHeader":  true,
			"authResponseHeaders": []any{"X-Sb-User", "X-Sb-Email", "Cookie"},
		}}
		objs = append(objs, m)
	}

	for _, v := range a.Volumes {
		size := v.Size
		if size == "" {
			size = "10Gi"
		}
		pvc := meta("PersistentVolumeClaim", "v1", ns, "data-"+v.Name, lbl)
		pvc["spec"] = Obj{
			"accessModes":      []any{"ReadWriteOnce"},
			"storageClassName": "hcloud-volumes",
			"resources":        Obj{"requests": Obj{"storage": size}},
		}
		objs = append(objs, pvc)
	}

	if a.Database {
		c := meta("Cluster", "postgresql.cnpg.io/v1", ns, "db", lbl)
		c["spec"] = Obj{
			"instances": 1,
			"storage":   Obj{"size": "10Gi", "storageClass": "hcloud-volumes"},
			"bootstrap": Obj{"initdb": Obj{"database": "app", "owner": "app"}},
		}
		objs = append(objs, c)
	}

	comps := Components(a)
	for i, c := range comps {
		clbl := map[string]string{AppLabel: a.Name, ComponentLabel: c.Name}
		if c.Port > 0 {
			ports := []any{Obj{"name": "http", "port": c.Port, "targetPort": c.Port}}
			if i == 0 && c.Port != 80 {
				// The first component also answers on port 80, which ${apps.X.internal} uses.
				ports = append(ports, Obj{"name": "http80", "port": 80, "targetPort": c.Port})
			}
			svc := meta("Service", "v1", ns, ServiceName(a, c.Name), clbl)
			svc["spec"] = Obj{"selector": Obj{AppLabel: a.Name, ComponentLabel: c.Name}, "ports": ports}
			objs = append(objs, svc)
			if i == 0 && c.Name != "web" {
				alias := meta("Service", "v1", ns, "web", clbl)
				alias["spec"] = Obj{"selector": Obj{AppLabel: a.Name, ComponentLabel: c.Name},
					"ports": []any{Obj{"name": "http80", "port": 80, "targetPort": c.Port}}}
				objs = append(objs, alias)
			}
		}
		image := c.Image
		if i == 0 && a.TemplateDef == nil {
			image = a.CurrentImage()
		}
		if image == "" {
			continue // a source app without a successful build yet
		}
		files, filesHash, err := renderFiles(a, c, p)
		if err == errNotReady {
			continue // waits for the database or a secret; the next reconcile renders it
		}
		if err != nil {
			return nil, err
		}
		if files != nil {
			objs = append(objs, files)
		}
		dep, err := deployment(a, c, i == 0, p, ns, clbl, image, filesHash)
		if err != nil {
			return nil, err
		}
		objs = append(objs, dep)
	}

	host := Host(a.Name, p.Domain)
	mainMW := ns + "-sb-strip@kubernetescrd"
	wallMW := ns + "-sb-wall@kubernetescrd"
	if a.Access == AccessWall {
		mainMW = wallMW
	}
	routes := Routes(a)
	objs = append(objs, ingress(ns, "web", host, routePaths(routes), routes, mainMW, lbl))
	if a.Access == AccessWall && len(a.PublicPaths) > 0 {
		objs = append(objs, ingress(ns, "web-public", host, a.PublicPaths, routes, ns+"-sb-strip@kubernetescrd", lbl))
	}
	if a.Access != AccessWall {
		// SlicedBread's own paths (/__sb/enter, /__sb/exchange) and the template's wallPaths
		// need a session even though the rest of the app does not.
		objs = append(objs, ingress(ns, "web-wall", host, append([]string{sbPaths}, a.WallPaths...), routes, wallMW, lbl))
	}
	return objs, nil
}

// Routes returns the app's path routes, longest first, ending with / to the first component.
func Routes(a *App) []Route {
	var routes []Route
	if a.TemplateDef != nil {
		routes = append(routes, a.TemplateDef.Routes...)
	}
	hasRoot := false
	for _, r := range routes {
		hasRoot = hasRoot || r.Path == "/"
	}
	if !hasRoot {
		routes = append(routes, Route{Path: "/", To: Primary(a).Name})
	}
	sort.SliceStable(routes, func(i, j int) bool { return len(routes[i].Path) > len(routes[j].Path) })
	return routes
}

func routePaths(rs []Route) []string {
	var out []string
	for _, r := range rs {
		out = append(out, r.Path)
	}
	return out
}

// routeFor returns the component serving a path (longest route prefix).
// RouteFor returns the component serving a path.
func RouteFor(routes []Route, path string) string { return routeFor(routes, path) }

func routeFor(routes []Route, path string) string {
	for _, r := range routes {
		if strings.HasPrefix(path, r.Path) {
			return r.To
		}
	}
	return routes[len(routes)-1].To
}

func componentPort(a *App, name string) int {
	for _, c := range Components(a) {
		if c.Name == name {
			return c.Port
		}
	}
	return 80
}

func ingress(ns, name, host string, paths []string, routes []Route, middleware string, lbl map[string]string) Obj {
	ing := meta("Ingress", "networking.k8s.io/v1", ns, name, lbl)
	ing["metadata"].(Obj)["annotations"] = Obj{
		"traefik.ingress.kubernetes.io/router.entrypoints": "websecure",
		"traefik.ingress.kubernetes.io/router.tls":         "true",
		"traefik.ingress.kubernetes.io/router.middlewares": middleware,
	}
	var ps []any
	for _, p := range paths {
		pt := "Prefix"
		if strings.Contains(p, ".") { // files like /script.js
			pt = "Exact"
		}
		to := routeFor(routes, p)
		ps = append(ps, Obj{"path": p, "pathType": pt, "backend": Obj{"service": Obj{"name": to, "port": Obj{"name": "http"}}}})
	}
	ing["spec"] = Obj{
		"ingressClassName": "traefik",
		"rules":            []any{Obj{"host": host, "http": Obj{"paths": ps}}},
	}
	return ing
}

// renderFiles builds the Secret holding a component's config files.
func renderFiles(a *App, c Component, p Params) (Obj, string, error) {
	if len(c.Files) == 0 || a.TemplateDef == nil {
		return nil, "", nil
	}
	r := resolver{a: a, p: p, inFile: true}
	data := Obj{}
	h := sha256.New()
	for i, f := range c.Files {
		text, err := r.text(a.TemplateDef.Files[f.Template])
		if err != nil {
			return nil, "", err
		}
		data["f"+strconv.Itoa(i)] = text
		fmt.Fprintf(h, "%s\n%s", f.Path, text)
	}
	sec := meta("Secret", "v1", Namespace(a.Name), "files-"+c.Name, map[string]string{AppLabel: a.Name, ComponentLabel: c.Name})
	sec["type"] = "Opaque"
	sec["stringData"] = data
	return sec, hex.EncodeToString(h.Sum(nil))[:12], nil
}

func deployment(a *App, c Component, primary bool, p Params, ns string, lbl map[string]string, image, filesHash string) (Obj, error) {
	env, err := resolveEnv(a, c, primary, p)
	if err != nil {
		return nil, err
	}
	container := Obj{
		"name":            "app",
		"image":           image,
		"imagePullPolicy": "IfNotPresent",
		"securityContext": Obj{
			"allowPrivilegeEscalation": false,
			"capabilities":             capabilities(c.RunAsUser),
		},
		"resources": resources(c),
	}
	if c.Port > 0 {
		container["ports"] = []any{Obj{"name": "http", "containerPort": c.Port}}
		probe := Obj{"tcpSocket": Obj{"port": c.Port}, "periodSeconds": 5, "failureThreshold": 3}
		if c.Health != nil {
			probe = Obj{"httpGet": Obj{"path": c.Health.Path, "port": c.Port}, "periodSeconds": 5, "failureThreshold": 3}
		}
		container["readinessProbe"] = probe
	}
	if len(c.Command) > 0 {
		container["command"] = toAny(c.Command)
	}
	if len(c.Args) > 0 {
		container["args"] = toAny(c.Args)
	}
	if len(env) > 0 {
		container["env"] = env
	}
	podSec := Obj{"seccompProfile": Obj{"type": "RuntimeDefault"}}
	if c.RunAsUser > 0 {
		podSec["runAsNonRoot"] = true
		podSec["runAsUser"] = c.RunAsUser
		podSec["fsGroup"] = c.RunAsUser
	}
	var mounts, vols []any
	for _, v := range c.Volumes {
		mounts = append(mounts, Obj{"name": v.Name, "mountPath": v.Path})
		vols = append(vols, Obj{"name": v.Name, "persistentVolumeClaim": Obj{"claimName": "data-" + v.Name}})
	}
	if len(c.Files) > 0 {
		vols = append(vols, Obj{"name": "sb-files", "secret": Obj{"secretName": "files-" + c.Name}})
		for i, f := range c.Files {
			mounts = append(mounts, Obj{"name": "sb-files", "mountPath": f.Path, "subPath": "f" + strconv.Itoa(i), "readOnly": true})
		}
	}
	if len(mounts) > 0 {
		container["volumeMounts"] = mounts
	}
	pod := Obj{
		"securityContext":              podSec,
		"automountServiceAccountToken": false,
		"enableServiceLinks":           false,
		"containers":                   []any{container},
	}
	if len(vols) > 0 {
		pod["volumes"] = vols
	}
	strategy := Obj{"type": "RollingUpdate"}
	if len(c.Volumes) > 0 {
		strategy = Obj{"type": "Recreate"} // RWO volumes cannot attach to two pods
	}
	replicas := c.Replicas
	if replicas < 1 {
		replicas = 1
	}
	if a.Paused {
		replicas = 0
	}
	hash := configHash(c, env, image, p.SecretVersion, filesHash)
	dep := meta("Deployment", "apps/v1", ns, c.Name, lbl)
	dep["spec"] = Obj{
		"replicas":             replicas,
		"revisionHistoryLimit": 3,
		"strategy":             strategy,
		"selector":             Obj{"matchLabels": Obj{AppLabel: a.Name, ComponentLabel: c.Name}},
		"template": Obj{
			"metadata": Obj{
				"labels":      Obj{AppLabel: a.Name, ComponentLabel: c.Name},
				"annotations": Obj{"slicedbread.dev/config-hash": hash},
			},
			"spec": pod,
		},
	}
	return dep, nil
}

func toAny(xs []string) []any {
	out := make([]any, len(xs))
	for i, x := range xs {
		out[i] = x
	}
	return out
}

func resources(c Component) Obj {
	req := Obj{"cpu": "50m", "memory": "64Mi"}
	limit := "1Gi"
	if r := c.Resources; r != nil {
		if r.CPU != "" {
			req["cpu"] = r.CPU
		}
		if r.Memory != "" {
			req["memory"] = r.Memory
		}
		if r.MemoryLimit != "" {
			limit = r.MemoryLimit
		}
	}
	return Obj{"requests": req, "limits": Obj{"memory": limit}}
}

// actionEgressPolicy limits action jobs to their own app, its database and DNS.
func actionEgressPolicy(ns string, lbl map[string]string) Obj {
	np := meta("NetworkPolicy", "networking.k8s.io/v1", ns, "sb-actions", lbl)
	np["spec"] = Obj{
		"podSelector": Obj{"matchExpressions": []any{Obj{"key": ActionLabel, "operator": "Exists"}}},
		"policyTypes": []any{"Egress"},
		"egress": []any{
			Obj{"to": []any{Obj{"podSelector": Obj{}}}},
			Obj{
				"to":    []any{Obj{"namespaceSelector": Obj{"matchLabels": Obj{"kubernetes.io/metadata.name": "kube-system"}}, "podSelector": Obj{"matchLabels": Obj{"k8s-app": "kube-dns"}}}},
				"ports": []any{Obj{"protocol": "UDP", "port": 53}, Obj{"protocol": "TCP", "port": 53}},
			},
		},
	}
	return np
}

// ActionEgressPolicy admits an action's declared external hosts: HTTPS to anywhere
// outside the cluster and private networks (NetworkPolicy cannot name hostnames).
func ActionEgressPolicy(ns, app, action string) Obj {
	np := meta("NetworkPolicy", "networking.k8s.io/v1", ns, "sb-action-egress-"+action, map[string]string{AppLabel: app})
	np["spec"] = Obj{
		"podSelector": Obj{"matchLabels": Obj{ActionLabel: action}},
		"policyTypes": []any{"Egress"},
		"egress": []any{Obj{
			"to":    []any{Obj{"ipBlock": Obj{"cidr": "0.0.0.0/0", "except": []any{"10.0.0.0/8", "172.16.0.0/12", "192.168.0.0/16", "169.254.0.0/16", "100.64.0.0/10"}}}},
			"ports": []any{Obj{"protocol": "TCP", "port": 443}},
		}},
	}
	return np
}

// rootCaps is what a container running as root keeps: enough for common images
// (nginx, Postgres, s6-based images) to switch users and fix file ownership.
// It is Docker's default set without NET_RAW, MKNOD, SYS_CHROOT, SETPCAP, SETFCAP and AUDIT_WRITE.
var rootCaps = []any{"CHOWN", "DAC_OVERRIDE", "FOWNER", "SETUID", "SETGID", "KILL", "NET_BIND_SERVICE"}

func capabilities(runAsUser int64) Obj {
	if runAsUser > 0 {
		return Obj{"drop": []any{"ALL"}}
	}
	return Obj{"drop": []any{"ALL"}, "add": rootCaps}
}

func configHash(c Component, env []any, image, secretVersion, filesHash string) string {
	b, _ := json.Marshal(struct {
		C      Component
		Env    []any
		Image  string
		Secret string
		Files  string
	}{c, env, image, secretVersion, filesHash})
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:8])
}

var internalRefRE = regexp.MustCompile(`\$\{apps\.([a-z0-9-]+)\.internal\}`)

// InternalRefs returns the apps this app calls through ${apps.<name>.internal}.
func InternalRefs(a *App) []string {
	seen := map[string]bool{}
	scan := func(v string) {
		for _, m := range internalRefRE.FindAllStringSubmatch(v, -1) {
			if m[1] != a.Name {
				seen[m[1]] = true
			}
		}
	}
	for _, c := range Components(a) {
		for _, v := range c.Env {
			scan(string(v))
		}
	}
	if a.TemplateDef != nil {
		for _, f := range a.TemplateDef.Files {
			scan(f)
		}
	}
	out := make([]string, 0, len(seen))
	for k := range seen {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// AllowFromLabel marks NetworkPolicies that admit traffic from another app.
const AllowFromLabel = "slicedbread.dev/allow-from"

// AllowFromPolicy admits traffic from app `from` into app `to`'s namespace.
func AllowFromPolicy(from, to string) Obj {
	np := meta("NetworkPolicy", "networking.k8s.io/v1", Namespace(to), "allow-from-app-"+from, map[string]string{AppLabel: to, AllowFromLabel: from})
	np["spec"] = Obj{
		"podSelector": Obj{},
		"policyTypes": []any{"Ingress"},
		"ingress":     []any{Obj{"from": []any{Obj{"namespaceSelector": Obj{"matchLabels": Obj{"kubernetes.io/metadata.name": Namespace(from)}}}}}},
	}
	return np
}
