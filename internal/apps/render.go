package apps

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"regexp"
	"sort"
	"strings"
)

// Obj is a Kubernetes object as unstructured JSON.
type Obj = map[string]any

// Params are tenant-level values needed to render an app.
type Params struct {
	Domain        string // tenant domain, e.g. r1234567.test.slicedbread.dev
	AuthService   string // base URL of sb-control inside the cluster, e.g. http://sb-control.sb-system.svc.cluster.local:8080
	SecretVersion string // changes when the app secret changes, to roll pods
}

const (
	AppSecret      = "app-env" // per-app secret holding generated and user-set values
	DBSecret       = "db-app"  // created by CloudNativePG
	ManagedByLabel = "app.kubernetes.io/managed-by"
	AppLabel       = "slicedbread.dev/app"
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

var (
	wholeRef  = regexp.MustCompile(`^\$\{(db\.url|secret\.[A-Za-z_][A-Za-z0-9_]*)\}$`)
	inlineRef = regexp.MustCompile(`\$\{(app\.url|app\.host|apps\.[a-z0-9-]+\.(url|internal))\}`)
)

// resolveEnv turns env values into container env entries.
func resolveEnv(a *App, p Params) ([]any, error) {
	keys := make([]string, 0, len(a.Env))
	for k := range a.Env {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var out []any
	if a.Builds() {
		if _, ok := a.Env["PORT"]; !ok {
			out = append(out, Obj{"name": "PORT", "value": fmt.Sprint(a.ContainerPort())})
		}
	}
	for _, k := range keys {
		v := a.Env[k]
		if m := wholeRef.FindStringSubmatch(v); m != nil {
			ref := m[1]
			secret, key := DBSecret, "uri"
			if strings.HasPrefix(ref, "secret.") {
				secret, key = AppSecret, strings.TrimPrefix(ref, "secret.")
			} else if !a.Database {
				return nil, fmt.Errorf("env %s references ${db.url} but the app has no database", k)
			}
			out = append(out, Obj{"name": k, "valueFrom": Obj{"secretKeyRef": Obj{"name": secret, "key": key}}})
			continue
		}
		if strings.Contains(v, "${db.url}") || strings.Contains(v, "${secret.") {
			return nil, fmt.Errorf("env %s: ${db.url} and ${secret.*} must be the whole value", k)
		}
		v = inlineRef.ReplaceAllStringFunc(v, func(s string) string {
			ref := strings.TrimSuffix(strings.TrimPrefix(s, "${"), "}")
			switch {
			case ref == "app.url":
				return "https://" + Host(a.Name, p.Domain)
			case ref == "app.host":
				return Host(a.Name, p.Domain)
			case strings.HasSuffix(ref, ".url"):
				other := strings.TrimSuffix(strings.TrimPrefix(ref, "apps."), ".url")
				return "https://" + Host(other, p.Domain)
			default:
				other := strings.TrimSuffix(strings.TrimPrefix(ref, "apps."), ".internal")
				return "http://web." + Namespace(other) + ".svc.cluster.local"
			}
		})
		out = append(out, Obj{"name": k, "value": v})
	}
	for _, k := range a.SecretKeys {
		if _, plain := a.Env[k]; plain {
			continue
		}
		out = append(out, Obj{"name": k, "valueFrom": Obj{"secretKeyRef": Obj{"name": AppSecret, "key": k}}})
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

	port := a.ContainerPort()
	svc := meta("Service", "v1", ns, "web", lbl)
	svc["spec"] = Obj{
		"selector": Obj{AppLabel: a.Name},
		"ports":    []any{Obj{"name": "http", "port": 80, "targetPort": port}},
	}
	objs = append(objs, svc)

	if img := a.CurrentImage(); img != "" {
		dep, err := deployment(a, p, ns, lbl, img, port)
		if err != nil {
			return nil, err
		}
		objs = append(objs, dep)
	}

	host := Host(a.Name, p.Domain)
	mainMW := ns + "-sb-strip@kubernetescrd"
	if a.Access == AccessWall {
		mainMW = ns + "-sb-wall@kubernetescrd"
	}
	objs = append(objs, ingress(ns, "web", host, []string{"/"}, mainMW, lbl))
	if a.Access == AccessWall && len(a.PublicPaths) > 0 {
		objs = append(objs, ingress(ns, "web-public", host, a.PublicPaths, ns+"-sb-strip@kubernetescrd", lbl))
	}
	return objs, nil
}

func ingress(ns, name, host string, paths []string, middleware string, lbl map[string]string) Obj {
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
		ps = append(ps, Obj{"path": p, "pathType": pt, "backend": Obj{"service": Obj{"name": "web", "port": Obj{"number": 80}}}})
	}
	ing["spec"] = Obj{
		"ingressClassName": "traefik",
		"rules":            []any{Obj{"host": host, "http": Obj{"paths": ps}}},
	}
	return ing
}

func deployment(a *App, p Params, ns string, lbl map[string]string, image string, port int) (Obj, error) {
	env, err := resolveEnv(a, p)
	if err != nil {
		return nil, err
	}
	probe := Obj{"tcpSocket": Obj{"port": port}, "periodSeconds": 5, "failureThreshold": 3}
	if a.HealthPath != "" {
		probe = Obj{"httpGet": Obj{"path": a.HealthPath, "port": port}, "periodSeconds": 5, "failureThreshold": 3}
	}
	podSec := Obj{"seccompProfile": Obj{"type": "RuntimeDefault"}}
	if a.RunAsUser > 0 {
		podSec["runAsNonRoot"] = true
		podSec["runAsUser"] = a.RunAsUser
		podSec["fsGroup"] = a.RunAsUser
	}
	container := Obj{
		"name":            "app",
		"image":           image,
		"imagePullPolicy": "IfNotPresent",
		"ports":           []any{Obj{"name": "http", "containerPort": port}},
		"readinessProbe":  probe,
		"securityContext": Obj{
			"allowPrivilegeEscalation": false,
			"capabilities":             Obj{"drop": []any{"ALL"}},
		},
		"resources": Obj{
			"requests": Obj{"cpu": "50m", "memory": "64Mi"},
			"limits":   Obj{"memory": "1Gi"},
		},
	}
	if len(env) > 0 {
		container["env"] = env
	}
	var mounts, vols []any
	for _, v := range a.Volumes {
		mounts = append(mounts, Obj{"name": v.Name, "mountPath": v.Path})
		vols = append(vols, Obj{"name": v.Name, "persistentVolumeClaim": Obj{"claimName": "data-" + v.Name}})
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
	if len(a.Volumes) > 0 {
		strategy = Obj{"type": "Recreate"} // RWO volumes cannot attach to two pods
	}
	hash := configHash(a, image, p.SecretVersion)
	dep := meta("Deployment", "apps/v1", ns, "web", lbl)
	dep["spec"] = Obj{
		"replicas":             1,
		"revisionHistoryLimit": 3,
		"strategy":             strategy,
		"selector":             Obj{"matchLabels": Obj{AppLabel: a.Name}},
		"template": Obj{
			"metadata": Obj{
				"labels":      Obj{AppLabel: a.Name},
				"annotations": Obj{"slicedbread.dev/config-hash": hash},
			},
			"spec": pod,
		},
	}
	return dep, nil
}

func configHash(a *App, image, secretVersion string) string {
	b, _ := json.Marshal(struct {
		Env    map[string]string
		Keys   []string
		Image  string
		Secret string
	}{a.Env, a.SecretKeys, image, secretVersion})
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:8])
}
