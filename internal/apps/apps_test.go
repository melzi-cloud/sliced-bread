package apps

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"encoding/json"
	"strings"
	"testing"
	"time"
)

var testParams = Params{Domain: "r1.test.slicedbread.dev", AuthService: "http://sb-control.sb-system.svc.cluster.local:8080", SecretVersion: "1"}

func find(objs []Obj, kind, name string) Obj {
	for _, o := range objs {
		if o["kind"] == kind && o["metadata"].(Obj)["name"] == name {
			return o
		}
	}
	return nil
}

func js(o any) string { b, _ := json.Marshal(o); return string(b) }

func TestValidate(t *testing.T) {
	ok := App{Name: "crm", Image: "nginx", Access: AccessWall}
	if err := ok.Validate(); err != nil {
		t.Fatal(err)
	}
	bad := []App{
		{Name: "API", Image: "x", Access: AccessWall},
		{Name: "api", Image: "x", Access: AccessWall},
		{Name: "crm", Access: AccessWall},
		{Name: "crm", Image: "x", Repo: &Repo{URL: "https://github.com/a/b"}, Access: AccessWall},
		{Name: "crm", Repo: &Repo{URL: "git@github.com:a/b"}, Access: AccessWall},
		{Name: "crm", Image: "x", Access: "open"},
		{Name: "crm", Image: "x", Access: AccessWall, Env: map[string]string{"bad-key": "1"}},
		{Name: "crm", Image: "x", Access: AccessWall, PublicPaths: []string{"no-slash"}},
	}
	for i, a := range bad {
		if err := a.Validate(); err == nil {
			t.Errorf("case %d should fail: %+v", i, a)
		}
	}
}

func TestRenderWallApp(t *testing.T) {
	a := &App{Name: "crm", Image: "nginx:1", Port: 80, Access: AccessWall, Env: map[string]string{"SELF": "${app.url}", "ANALYTICS": "${apps.analytics.url}"}}
	objs, err := Render(a, testParams)
	if err != nil {
		t.Fatal(err)
	}
	ing := find(objs, "Ingress", "web")
	ann := ing["metadata"].(Obj)["annotations"].(Obj)
	if ann["traefik.ingress.kubernetes.io/router.middlewares"] != "app-crm-sb-wall@kubernetescrd" {
		t.Fatalf("wall app must use the wall middleware: %v", ann)
	}
	if !strings.Contains(js(ing), `"host":"crm.r1.test.slicedbread.dev"`) {
		t.Fatal("wrong host")
	}
	mw := find(objs, "Middleware", "sb-wall")
	s := js(mw)
	for _, want := range []string{`/auth/verify?app=crm`, `"Cookie"`, `"X-Sb-User"`} {
		if !strings.Contains(s, want) {
			t.Errorf("wall middleware missing %s: %s", want, s)
		}
	}
	dep := js(find(objs, "Deployment", "web"))
	for _, want := range []string{`"value":"https://crm.r1.test.slicedbread.dev"`, `"value":"https://analytics.r1.test.slicedbread.dev"`, `"drop":["ALL"]`, `"add":["CHOWN"`, `"automountServiceAccountToken":false`} {
		if !strings.Contains(dep, want) {
			t.Errorf("deployment missing %s", want)
		}
	}
	if find(objs, "NetworkPolicy", "sb-isolation") == nil {
		t.Fatal("missing network policy")
	}
	if find(objs, "Ingress", "web-public") != nil {
		t.Fatal("no public paths, so no public ingress")
	}
}

func TestRenderTemplates(t *testing.T) {
	u := &App{Name: "analytics", Template: "umami"}
	if err := ApplyTemplate(u); err != nil {
		t.Fatal(err)
	}
	objs, err := Render(u, testParams)
	if err != nil {
		t.Fatal(err)
	}
	pub := find(objs, "Ingress", "web-public")
	if pub == nil || !strings.Contains(js(pub), `"/script.js"`) || !strings.Contains(js(pub), "app-analytics-sb-strip@kubernetescrd") {
		t.Fatalf("umami needs public tracker paths through the strip middleware: %s", js(pub))
	}
	if !strings.Contains(js(pub), `"pathType":"Exact"`) {
		t.Fatal("file paths should be Exact")
	}
	if find(objs, "Cluster", "db") == nil {
		t.Fatal("umami needs a database")
	}
	dep := js(find(objs, "Deployment", "web"))
	if !strings.Contains(dep, `"secretKeyRef":{"key":"uri","name":"db-app"}`) || !strings.Contains(dep, `"secretKeyRef":{"key":"APP_SECRET","name":"app-env"}`) {
		t.Fatalf("umami env must come from secrets: %s", dep)
	}

	v := &App{Name: "vault", Template: "vaultwarden"}
	if err := ApplyTemplate(v); err != nil {
		t.Fatal(err)
	}
	if v.Env["SIGNUPS_ALLOWED"] != "false" || v.Access != AccessAppAuth {
		t.Fatalf("vaultwarden defaults wrong: %+v", v)
	}
	objs, err = Render(v, testParams)
	if err != nil {
		t.Fatal(err)
	}
	ing := js(find(objs, "Ingress", "web"))
	if !strings.Contains(ing, "app-vault-sb-strip@kubernetescrd") {
		t.Fatal("app-auth apps still go through the cookie-stripping middleware")
	}
	if !strings.Contains(js(find(objs, "Deployment", "web")), `"type":"Recreate"`) {
		t.Fatal("apps with volumes must use Recreate")
	}
	if err := CheckUserEnv("vaultwarden", "ADMIN_TOKEN"); err == nil {
		t.Fatal("ADMIN_TOKEN must be refused")
	}
	if err := CheckUserEnv("vaultwarden", "FOO"); err != nil {
		t.Fatal(err)
	}
	again := cloneForTest(v)
	again.Settings["signups"] = "true"
	if err := ApplyTemplate(again); err != nil || again.Env["SIGNUPS_ALLOWED"] != "true" {
		t.Fatalf("re-applying a template with new settings must work: %v %v", err, again.Env)
	}
	var bad *App
	bad = &App{Name: "vault", Template: "vaultwarden", Settings: map[string]string{"signups": "maybe"}}
	if err := ApplyTemplate(bad); err == nil {
		t.Fatal("invalid setting value must be refused")
	}
}

func TestRepoAppWithoutBuildHasNoDeployment(t *testing.T) {
	a := &App{Name: "site", Repo: &Repo{URL: "https://github.com/a/b"}, Access: AccessWall}
	objs, err := Render(a, testParams)
	if err != nil {
		t.Fatal(err)
	}
	if find(objs, "Deployment", "web") != nil {
		t.Fatal("no deployment before the first successful build")
	}
	a.Build = &BuildState{LastGood: "sb-registry.local:5000/site:b1"}
	objs, _ = Render(a, testParams)
	dep := js(find(objs, "Deployment", "web"))
	if !strings.Contains(dep, "sb-registry.local:5000/site:b1") || !strings.Contains(dep, `"name":"PORT","value":"8080"`) {
		t.Fatalf("repo app deployment wrong: %s", dep)
	}
}

func TestEnvRefErrors(t *testing.T) {
	a := &App{Name: "x1", Image: "i", Access: AccessWall, Env: map[string]string{"DB": "${db.url}"}}
	if _, err := Render(a, testParams); err == nil {
		t.Fatal("db ref without database must fail")
	}
	a = &App{Name: "x1", Image: "i", Access: AccessWall, Env: map[string]string{"DB": "prefix-${secret.X}"}}
	if _, err := Render(a, testParams); err == nil {
		t.Fatal("partial secret ref must fail")
	}
}

func cloneForTest(a *App) *App {
	b, _ := json.Marshal(a)
	var c App
	json.Unmarshal(b, &c)
	return &c
}

func TestVaultwardenSignupDomain(t *testing.T) {
	v := &App{Name: "vault", Template: "vaultwarden", Settings: map[string]string{"signups": "true", "signup_domain": "acme.com"}}
	if err := ApplyTemplate(v); err != nil {
		t.Fatal(err)
	}
	if v.Env["SIGNUPS_DOMAINS_WHITELIST"] != "acme.com" || v.Env["SIGNUPS_ALLOWED"] != "true" {
		t.Fatalf("env: %v", v.Env)
	}
	v.Settings["signup_domain"] = ""
	ApplyTemplate(v)
	if _, ok := v.Env["SIGNUPS_DOMAINS_WHITELIST"]; ok {
		t.Fatal("clearing the domain must remove the restriction")
	}
	bad := &App{Name: "vault", Template: "vaultwarden", Settings: map[string]string{"signup_domain": "not a domain"}}
	if err := ApplyTemplate(bad); err == nil {
		t.Fatal("invalid domain accepted")
	}
	if err := CheckUserEnv("vaultwarden", "SIGNUPS_DOMAINS_WHITELIST"); err == nil {
		t.Fatal("template-managed env must be refused")
	}
}

func TestInternalRefsAndAllowPolicy(t *testing.T) {
	a := &App{Name: "web1", Image: "x", Access: AccessWall, Env: map[string]string{
		"API": "${apps.api-svc.internal}/v1", "SELF": "${apps.web1.internal}", "OTHER": "${apps.db2.internal}", "PUB": "${apps.site.url}",
	}}
	got := InternalRefs(a)
	if len(got) != 2 || got[0] != "api-svc" || got[1] != "db2" {
		t.Fatalf("refs = %v", got)
	}
	np := js(AllowFromPolicy("web1", "api-svc"))
	for _, want := range []string{`"namespace":"app-api-svc"`, `"kubernetes.io/metadata.name":"app-web1"`, `"slicedbread.dev/allow-from":"web1"`} {
		if !strings.Contains(np, want) {
			t.Errorf("policy missing %s: %s", want, np)
		}
	}
}

func TestContentHashIgnoresTimestamps(t *testing.T) {
	mk := func(mtime time.Time, body string) []byte {
		var b bytes.Buffer
		gz := gzip.NewWriter(&b)
		tw := tar.NewWriter(gz)
		tw.WriteHeader(&tar.Header{Name: "index.html", Mode: 0o644, Size: int64(len(body)), ModTime: mtime})
		tw.Write([]byte(body))
		tw.Close()
		gz.Close()
		return b.Bytes()
	}
	h1, _ := ContentHash(bytes.NewReader(mk(time.Unix(1, 0), "a")))
	h2, _ := ContentHash(bytes.NewReader(mk(time.Unix(999, 0), "a")))
	h3, _ := ContentHash(bytes.NewReader(mk(time.Unix(1, 0), "b")))
	if h1 != h2 || h1 == h3 || len(h1) != 12 {
		t.Fatalf("hashes %s %s %s", h1, h2, h3)
	}
}

func TestNonRootAppsKeepNoCapabilities(t *testing.T) {
	a := &App{Name: "x1", Image: "i", Access: AccessWall, RunAsUser: 1000}
	objs, _ := Render(a, testParams)
	dep := js(find(objs, "Deployment", "web"))
	if strings.Contains(dep, `"add"`) || !strings.Contains(dep, `"runAsNonRoot":true`) {
		t.Fatalf("non-root app must drop all capabilities: %s", dep)
	}
	for _, bad := range []string{"NET_RAW", "SYS_ADMIN", "MKNOD"} {
		for _, c := range rootCaps {
			if c == bad {
				t.Fatalf("%s must not be in the root capability set", bad)
			}
		}
	}
}
