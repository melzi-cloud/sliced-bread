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
	if !strings.Contains(dep, `"secretKeyRef":{"key":"uri","name":"db-app"}`) || !strings.Contains(dep, `"secretKeyRef":{"key":"APP_SECRET","name":"app-env","optional":true}`) {
		t.Fatalf("umami env must come from secrets: %s", dep)
	}
	if !strings.Contains(dep, "@sha256:") {
		t.Fatal("template images are pinned by digest")
	}
	if find(objs, "Ingress", "web-wall") != nil {
		t.Fatal("a wall app needs no separate wall ingress")
	}

	v := &App{Name: "vault", Template: "vaultwarden"}
	if err := ApplyTemplate(v); err != nil {
		t.Fatal(err)
	}
	if v.Access != AccessAppAuth || !v.BackupStop || len(v.BackupRequire) != 1 {
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
	wall := js(find(objs, "Ingress", "web-wall"))
	for _, want := range []string{"app-vault-sb-wall@kubernetescrd", `"/__sb/"`, `"/identity/accounts/register"`, `"/api/accounts/register"`} {
		if !strings.Contains(wall, want) {
			t.Errorf("vaultwarden wall ingress missing %s: %s", want, wall)
		}
	}
	d := js(find(objs, "Deployment", "web"))
	if !strings.Contains(d, `"type":"Recreate"`) || !strings.Contains(d, `"name":"SIGNUPS_ALLOWED","value":"true"`) {
		t.Fatal("vaultwarden: Recreate and registration on (behind the wall)")
	}
	if err := CheckUserEnv(v, "ADMIN_TOKEN"); err == nil {
		t.Fatal("ADMIN_TOKEN must be refused")
	}
	if err := CheckUserEnv(v, "FOO"); err != nil {
		t.Fatal(err)
	}
	// The user's access choice survives a settings change.
	again := cloneForTest(v)
	again.Access = AccessWall
	again.Settings["signup_domain"] = "acme.com"
	if err := ApplyTemplate(again); err != nil || again.Access != AccessWall {
		t.Fatalf("access must be kept: %v %s", err, again.Access)
	}
	objs, _ = Render(again, testParams)
	if !strings.Contains(js(find(objs, "Deployment", "web")), `"name":"SIGNUPS_DOMAINS_WHITELIST","value":"acme.com"`) {
		t.Fatal("setting env missing")
	}
	bad := &App{Name: "vault", Template: "vaultwarden", Settings: map[string]string{"signup_domain": "not a domain"}}
	if err := ApplyTemplate(bad); err == nil {
		t.Fatal("invalid setting value must be refused")
	}
	if err := ApplyTemplate(&App{Name: "vault", Template: "vaultwarden", Settings: map[string]string{"signups": "true"}}); err == nil {
		t.Fatal("the old signups setting no longer exists")
	}
}

const multiTemplate = `apiVersion: bakery.slicedbread.dev/v1
name: tube
version: 0.1.0
description: test
upstream: {project: T, url: https://example.com, licence: MIT}
access: {mode: wall}
components:
  - name: web
    image: example.com/web:1@sha256:0000000000000000000000000000000000000000000000000000000000000001
    port: 3000
    files: [{path: /config/config.yml, template: files/config.yml}]
    env: {COMPANION: "${component.companion.internal}"}
  - name: companion
    image: example.com/comp:1@sha256:0000000000000000000000000000000000000000000000000000000000000002
    port: 8282
    replicas: 2
    env: {KEY: "${secret.COMPANION_KEY}"}
  - name: worker
    image: example.com/worker:1@sha256:0000000000000000000000000000000000000000000000000000000000000003
routes:
  - {path: /companion, to: companion}
database: {postgres: {version: "17"}}
secrets:
  generate: [{name: COMPANION_KEY, bytes: 16, format: alnum}]
`

func TestMultiComponentTemplate(t *testing.T) {
	tm, err := ParseTemplate([]byte(multiTemplate), map[string]string{
		"files/config.yml": "companion: ${component.companion.internal}/companion\nkey: ${secret.COMPANION_KEY}\ndb: ${db.url}\n",
	})
	if err != nil {
		t.Fatal(err)
	}
	a := &App{Name: "tube", TemplateDef: tm}
	if err := ApplyTemplate(a); err != nil {
		t.Fatal(err)
	}
	// Without secret values the config file cannot be rendered yet: web waits.
	objs, err := Render(a, testParams)
	if err != nil {
		t.Fatal(err)
	}
	if find(objs, "Deployment", "web") != nil || find(objs, "Deployment", "companion") == nil {
		t.Fatal("web waits for its config file values; companion does not")
	}
	p := testParams
	p.Values = map[string]string{"COMPANION_KEY": "k123"}
	p.DB = map[string]string{"url": "postgresql://app:pw@db-rw/app"}
	objs, err = Render(a, p)
	if err != nil {
		t.Fatal(err)
	}
	files := js(find(objs, "Secret", "files-web"))
	if !strings.Contains(files, "companion: http://companion.app-tube.svc.cluster.local:8282/companion") || !strings.Contains(files, "key: k123") {
		t.Fatalf("config file: %s", files)
	}
	web := js(find(objs, "Deployment", "web"))
	if !strings.Contains(web, `"subPath":"f0"`) || !strings.Contains(web, `"value":"http://companion.app-tube.svc.cluster.local:8282"`) {
		t.Fatalf("web: %s", web)
	}
	comp := js(find(objs, "Deployment", "companion"))
	if !strings.Contains(comp, `"replicas":2`) || !strings.Contains(comp, `"slicedbread.dev/component":"companion"`) {
		t.Fatalf("companion: %s", comp)
	}
	worker := js(find(objs, "Deployment", "worker"))
	if strings.Contains(worker, "readinessProbe") || find(objs, "Service", "worker") != nil {
		t.Fatal("workers have no port, probe or service")
	}
	ing := js(find(objs, "Ingress", "web"))
	if !strings.Contains(ing, `{"backend":{"service":{"name":"companion","port":{"name":"http"}}},"path":"/companion"`) {
		t.Fatalf("route to companion missing: %s", ing)
	}
	svc := js(find(objs, "Service", "web"))
	if !strings.Contains(svc, `"port":80`) || !strings.Contains(svc, `"port":3000`) {
		t.Fatalf("first component answers on its port and 80: %s", svc)
	}
}

func TestTemplateValidation(t *testing.T) {
	cases := map[string]string{
		"tag only":       strings.Replace(multiTemplate, "@sha256:0000000000000000000000000000000000000000000000000000000000000001", "", 1),
		"bad route":      strings.Replace(multiTemplate, "to: companion}", "to: nothing}", 1),
		"undeclared ref": strings.Replace(multiTemplate, "${secret.COMPANION_KEY}", "${secret.OTHER}", 1),
		"partial secret": strings.Replace(multiTemplate, `KEY: "${secret.COMPANION_KEY}"`, `KEY: "x${secret.COMPANION_KEY}"`, 1),
		"unknown field":  multiTemplate + "colour: blue\n",
		"worker first":   strings.Replace(multiTemplate, "    port: 3000\n", "", 1),
	}
	for name, src := range cases {
		if _, err := ParseTemplate([]byte(src), map[string]string{"files/config.yml": "x"}); err == nil {
			t.Errorf("%s: should fail", name)
		}
	}
	if _, err := ParseTemplate([]byte(multiTemplate), nil); err == nil {
		t.Error("missing file template should fail")
	}
}

func TestBuiltInCatalog(t *testing.T) {
	for _, name := range []string{"umami", "vaultwarden"} {
		tm := Catalog[name]
		if tm == nil || !tm.Trusted {
			t.Fatalf("%s missing or untrusted", name)
		}
	}
	u := Catalog["umami"]
	if a := u.Action("dashboard_login"); a == nil || !a.HasSecretOutput() {
		t.Fatal("dashboard_login returns a secret")
	}
	if a := u.Action("set_admin_password"); a == nil || a.On != "install" {
		t.Fatal("umami sets its admin password on install")
	}
	if !strings.HasPrefix(u.Files["actions/create-website.mjs"], "#!") {
		t.Fatal("scripts are loaded with the template")
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

func TestHosts(t *testing.T) {
	h := Hosts{Domain: "apps.example.com"}
	if h.App("crm") != "crm.apps.example.com" || h.Auth() != "auth.apps.example.com" || h.API() != "api.apps.example.com" {
		t.Fatal("self-hosted format")
	}
	hh := Hosts{Mode: ModeHosted, Domain: "slicedbread.cloud", Tenant: "acme"}
	if hh.App("crm") != "crm-acme.slicedbread.cloud" {
		t.Fatalf("hosted format: %s", hh.App("crm"))
	}
	long := Hosts{Mode: ModeHosted, Domain: "x.io", Tenant: strings.Repeat("t", 40)}.App(strings.Repeat("a", 30))
	label := strings.TrimSuffix(long, ".x.io")
	if len(label) > 63 || Label(strings.Repeat("a", 30)+"-"+strings.Repeat("t", 40)) != label {
		t.Fatalf("long label %q (%d)", label, len(label))
	}
}

const sqliteTemplate = `apiVersion: bakery.slicedbread.dev/v1
name: notes
version: 0.1.0
description: test
upstream: {project: N, url: https://example.com, licence: MIT}
access: {mode: wall}
components:
  - name: web
    image: example.com/web:1@sha256:0000000000000000000000000000000000000000000000000000000000000001
    port: 3000
    volumes: [{name: data, path: /data, size: 1Gi}]
backup:
  consistency: stop
  sqlite:
    - {path: data/app.db, tables: [users, notes]}
    - {path: data/sub/cache.sqlite3}
`

func TestBackupSQLiteValidation(t *testing.T) {
	tm, err := ParseTemplate([]byte(sqliteTemplate), nil)
	if err != nil {
		t.Fatal(err)
	}
	a := &App{Name: "notes", TemplateDef: tm}
	if err := ApplyTemplate(a); err != nil {
		t.Fatal(err)
	}
	if len(a.BackupSQLite) != 2 || a.BackupSQLite[0].Path != "data/app.db" || strings.Join(a.BackupSQLite[0].Tables, ",") != "users,notes" {
		t.Fatalf("app carries the template's SQLite checks: %+v", a.BackupSQLite)
	}
	cases := map[string]string{
		"unknown volume":    "{path: logs/app.db}",
		"volume only":       "{path: data}",
		"dot dot":           "{path: data/../etc/passwd}",
		"space":             `{path: "data/my app.db"}`,
		"colon":             `{path: "data/app.db:x"}`,
		"absolute":          "{path: /data/app.db}",
		"bad table":         `{path: data/b.db, tables: ["users; drop table users"]}`,
		"quoted table":      `{path: data/b.db, tables: ['a"b']}`,
		"min rows not here": "{path: data/b.db, minRows: {users: 1}}",
	}
	for name, entry := range cases {
		src := strings.Replace(sqliteTemplate, "{path: data/sub/cache.sqlite3}", entry, 1)
		if _, err := ParseTemplate([]byte(src), nil); err == nil {
			t.Errorf("%s: should fail", name)
		}
	}
	dup := strings.Replace(sqliteTemplate, "{path: data/sub/cache.sqlite3}", "{path: data/app.db}", 1)
	if _, err := ParseTemplate([]byte(dup), nil); err == nil || !strings.Contains(err.Error(), "twice") {
		t.Errorf("duplicate path: %v", err)
	}
}

func TestVaultwardenChecksItsSQLite(t *testing.T) {
	b := Catalog["vaultwarden"].Backup
	if b == nil || len(b.SQLite) != 1 || b.SQLite[0].Path != "data/db.sqlite3" || strings.Join(b.SQLite[0].Tables, ",") != "users,ciphers" {
		t.Fatalf("vaultwarden backup: %+v", b)
	}
}

func TestSQLiteSummary(t *testing.T) {
	got := SQLiteSummary([]SQLiteResult{
		{Path: "data/db.sqlite3", Integrity: "ok", Tables: map[string]int64{"users": 0, "ciphers": 12}},
		{Path: "data/x.db", Integrity: "ok", Missing: []string{"notes"}},
	})
	want := "sqlite data/db.sqlite3: integrity ok, rows: ciphers 12, users 0; sqlite data/x.db: integrity ok, missing tables notes"
	if got != want {
		t.Fatalf("got %q", got)
	}
}
