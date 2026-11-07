package control

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/melzi-cloud/sliced-bread/internal/apps"
	"github.com/melzi-cloud/sliced-bread/internal/kube"
)

// Backups: every app with a volume or a database gets one restic snapshot per run in an
// S3-compatible bucket. After each backup a restore test restores that snapshot into a
// scratch namespace and checks it: restic verifies file contents, required files must be
// present, and a database dump must load into a fresh Postgres with the same table count.

const (
	backupSecret     = "sb-backup" // in sb-system: target, keys, restic password, schedule
	backupEnvSecret  = "sb-backup-env"
	restoreTestNS    = "sb-restore-test"
	resticImage      = "restic/restic:0.19.1"
	postgresImage    = "postgres:18.6"
	checkImage       = "alpine:3.24.4"
	historyLimit     = 20
	backupJobTimeout = 3600
)

// BackupConfig is where backups go.
type BackupConfig struct {
	Endpoint        string `json:"endpoint"` // https://...
	Bucket          string `json:"bucket"`
	Region          string `json:"region"`
	Prefix          string `json:"prefix"`
	AccessKeyID     string `json:"accessKeyId,omitempty"`
	SecretAccessKey string `json:"secretAccessKey,omitempty"`
	Password        string `json:"password,omitempty"` // restic repository password
	Hour            int    `json:"hour"`               // daily backup hour, UTC
}

// Repository returns the restic repository URL.
func (c BackupConfig) Repository() string {
	return "s3:" + strings.TrimRight(c.Endpoint, "/") + "/" + c.Bucket + "/" + strings.Trim(c.Prefix, "/")
}

func (c BackupConfig) validate() error {
	u, err := url.Parse(c.Endpoint)
	switch {
	case err != nil || u.Scheme != "https" || u.Host == "":
		return errors.New("endpoint must be an https URL, e.g. https://<account>.r2.cloudflarestorage.com")
	case c.Bucket == "" || strings.ContainsAny(c.Bucket, "/ "):
		return errors.New("bucket is required and cannot contain / or spaces")
	case c.AccessKeyID == "" || c.SecretAccessKey == "":
		return errors.New("access key id and secret access key are required")
	case c.Hour < 0 || c.Hour > 23:
		return errors.New("hour must be 0-23 (UTC)")
	case strings.Contains(c.Prefix, ".."):
		return errors.New("bad prefix")
	}
	return nil
}

// backupConfig returns the backup target, or nil when no bucket is configured
// (the secret can exist with only the encryption password sb init installed).
func (s *Server) backupConfig(ctx context.Context) (*BackupConfig, error) {
	c, err := s.backupSecret(ctx)
	if err != nil || c == nil || c.Endpoint == "" || c.Bucket == "" {
		return nil, err
	}
	return c, nil
}

// backupSecret returns whatever the backup secret holds, including just the password.
func (s *Server) backupSecret(ctx context.Context) (*BackupConfig, error) {
	var sec kube.Obj
	if err := s.kube.Get(ctx, "v1", "Secret", s.cfg.Namespace, backupSecret, &sec); err != nil {
		if kube.IsNotFound(err) {
			return nil, nil
		}
		return nil, err
	}
	d := func(k string) string {
		v := kube.Str(sec, "data", k)
		b, _ := base64.StdEncoding.DecodeString(v)
		return string(b)
	}
	h, _ := strconv.Atoi(d("hour"))
	return &BackupConfig{
		Endpoint: d("endpoint"), Bucket: d("bucket"), Region: d("region"), Prefix: d("prefix"),
		AccessKeyID: d("access_key_id"), SecretAccessKey: d("secret_access_key"), Password: d("password"), Hour: h,
	}, nil
}

func (s *Server) saveBackupConfig(ctx context.Context, c BackupConfig) error {
	return s.kube.Apply(ctx, kube.Obj{
		"apiVersion": "v1", "kind": "Secret",
		"metadata": kube.Obj{"name": backupSecret, "namespace": s.cfg.Namespace, "labels": kube.Obj{apps.ManagedByLabel: "slicedbread"}},
		"type":     "Opaque",
		"stringData": kube.Obj{
			"endpoint": c.Endpoint, "bucket": c.Bucket, "region": c.Region, "prefix": c.Prefix,
			"access_key_id": c.AccessKeyID, "secret_access_key": c.SecretAccessKey, "password": c.Password,
			"hour": strconv.Itoa(c.Hour),
		},
	})
}

func newResticPassword() string {
	b := make([]byte, 32)
	rand.Read(b)
	return base64.RawURLEncoding.EncodeToString(b)
}

// envSecret places restic's environment in a namespace for its jobs. Apps cannot read it:
// app pods get no service account token.
func (s *Server) envSecret(ctx context.Context, c *BackupConfig, ns string) error {
	region := c.Region
	if region == "" {
		region = "auto"
	}
	return s.kube.Apply(ctx, kube.Obj{
		"apiVersion": "v1", "kind": "Secret",
		"metadata": kube.Obj{"name": backupEnvSecret, "namespace": ns, "labels": kube.Obj{apps.ManagedByLabel: "slicedbread"}},
		"type":     "Opaque",
		"stringData": kube.Obj{
			"RESTIC_REPOSITORY": c.Repository(), "RESTIC_PASSWORD": c.Password,
			"AWS_ACCESS_KEY_ID": c.AccessKeyID, "AWS_SECRET_ACCESS_KEY": c.SecretAccessKey,
			"SB_S3_REGION": region, "RESTIC_CACHE_DIR": "/tmp/restic-cache",
		},
	})
}

const resticFn = `r() { restic -o s3.region="$SB_S3_REGION" -o s3.bucket-lookup=path "$@"; }
`

// --- job specs ---

type jobSpec struct {
	name, ns   string
	app        *apps.App
	run        *apps.BackupRun
	init       []any
	main       kube.Obj
	volumes    []any
	nodeName   string
	envSecrets bool
}

func (j jobSpec) obj() kube.Obj {
	pod := kube.Obj{
		"restartPolicy":                "Never",
		"automountServiceAccountToken": false,
		"containers":                   []any{j.main},
		"volumes":                      j.volumes,
	}
	if len(j.init) > 0 {
		pod["initContainers"] = j.init
	}
	if j.nodeName != "" {
		pod["nodeName"] = j.nodeName
	}
	lbl := kube.Obj{apps.AppLabel: j.app.Name, "slicedbread.dev/backup-run": j.run.ID, apps.ManagedByLabel: "slicedbread"}
	return kube.Obj{
		"apiVersion": "batch/v1", "kind": "Job",
		"metadata": kube.Obj{"name": j.name, "namespace": j.ns, "labels": lbl},
		"spec": kube.Obj{
			"backoffLimit": 0, "activeDeadlineSeconds": backupJobTimeout, "ttlSecondsAfterFinished": 86400,
			"template": kube.Obj{"metadata": kube.Obj{"labels": kube.Obj{apps.AppLabel: j.app.Name, "slicedbread.dev/backup-run": j.run.ID}}, "spec": pod},
		},
	}
}

func container(name, image, script string, env []any, mounts []any, uid int64) kube.Obj {
	sc := kube.Obj{"allowPrivilegeEscalation": false}
	if uid > 0 {
		sc["runAsUser"], sc["runAsGroup"], sc["runAsNonRoot"] = uid, uid, true
	}
	c := kube.Obj{"name": name, "image": image, "command": []any{"sh", "-c", script}, "securityContext": sc,
		"env": env, "volumeMounts": mounts}
	return c
}

func envFromSecret() kube.Obj {
	return kube.Obj{"secretRef": kube.Obj{"name": backupEnvSecret}}
}

func dataVolumes(a *apps.App) (vols, mounts []any) {
	vols = []any{kube.Obj{"name": "sb", "emptyDir": kube.Obj{}}}
	mounts = []any{kube.Obj{"name": "sb", "mountPath": "/sb"}}
	for _, v := range a.Volumes {
		vols = append(vols, kube.Obj{"name": "vol-" + v.Name, "persistentVolumeClaim": kube.Obj{"claimName": "data-" + v.Name}})
		mounts = append(mounts, kube.Obj{"name": "vol-" + v.Name, "mountPath": "/sb/vol/" + v.Name})
	}
	return
}

func dbURIEnv() []any {
	return []any{kube.Obj{"name": "SB_DB_URI", "valueFrom": kube.Obj{"secretKeyRef": kube.Obj{"name": apps.DBSecret, "key": "uri"}}}}
}

func runEnv(a *apps.App, run *apps.BackupRun) []any {
	return []any{
		kube.Obj{"name": "SB_APP", "value": a.Name},
		kube.Obj{"name": "SB_RUN", "value": run.ID},
		kube.Obj{"name": "SB_SNAPSHOT", "value": run.Snapshot},
	}
}

// waitDB: a new pod's IP reaches the network policy rules a few seconds after it starts,
// so the first connection attempts can fail. Wait until Postgres answers.
const waitDB = `i=0; until pg_isready -q -t 3 -d "$SB_DB_URI"; do i=$((i+1)); if [ $i -ge "${SB_DB_WAIT_TRIES:-40}" ]; then echo "SB_ERROR database not reachable"; exit 1; fi; sleep 3; done
`

const countTablesSQL = `select count(*) from information_schema.tables where table_schema not in ('pg_catalog','information_schema') and table_type='BASE TABLE'`
const countRowsSQL = `select coalesce(sum((xpath('/row/c/text()', query_to_xml(format('select count(*) as c from %I.%I', table_schema, table_name), false, true, '')))[1]::text::bigint),0) from information_schema.tables where table_schema not in ('pg_catalog','information_schema') and table_type='BASE TABLE'`

func backupJob(a *apps.App, run *apps.BackupRun, nodeName, uploadURL string) kube.Obj {
	vols, mounts := dataVolumes(a)
	// The app's spec and secret values (and its uploaded source) travel with every
	// snapshot, so a backup can bring back a deleted app or a whole install.
	vols = append(vols, kube.Obj{"name": "meta", "secret": kube.Obj{"secretName": backupMetaSecret}})
	resticMounts := append(append([]any{}, mounts...), kube.Obj{"name": "meta", "mountPath": "/meta-src", "readOnly": true})
	var inits []any
	if a.Database {
		dump := `set -eu
` + waitDB + `mkdir -p /sb/db
pg_dump -Fc -d "$SB_DB_URI" -f /sb/db/db.dump
psql "$SB_DB_URI" -Atc "` + countTablesSQL + `" > /sb/db/tables
echo "dumped database: $(cat /sb/db/tables) tables, $(wc -c < /sb/db/db.dump) bytes"
`
		inits = append(inits, container("dump", postgresImage, dump, dbURIEnv(), []any{kube.Obj{"name": "sb", "mountPath": "/sb"}}, 999))
	}
	script := `set -eu
` + resticFn + `r cat config >/dev/null 2>&1 || r init >/dev/null 2>&1 || r cat config >/dev/null
mkdir -p /sb/vol /sb/meta
cp /meta-src/app.json /meta-src/env.json /sb/meta/
if [ -n "$SB_UPLOAD_URL" ]; then wget -qO /sb/meta/source.tar.gz "$SB_UPLOAD_URL" || { echo "SB_ERROR could not fetch the app's source for the backup"; exit 1; }; fi
EXC=""; PRUNE=""
for p in $SB_EXCLUDE; do EXC="$EXC --exclude /sb/vol/$p"; PRUNE="$PRUNE -path /sb/vol/$p -prune -o"; done
echo "SB_FILES $(find /sb/vol $PRUNE -type f -print | wc -l)"
if [ -f /sb/db/tables ]; then echo "SB_DB_TABLES $(cat /sb/db/tables)"; fi
out=$(r backup --host slicedbread --tag "app=$SB_APP" --tag "run=$SB_RUN" $EXC --json /sb | grep '"message_type":"summary"')
echo "SB_SNAPSHOT $(echo "$out" | sed -n 's/.*"snapshot_id":"\([0-9a-f]*\)".*/\1/p')"
echo "SB_BYTES $(echo "$out" | sed -n 's/.*"total_bytes_processed":\([0-9]*\).*/\1/p')"
r forget --host slicedbread --tag "app=$SB_APP" --keep-last 10 --keep-daily 14 --keep-weekly 8 --prune >/dev/null 2>&1 || echo "note: pruning old snapshots skipped (repository busy)"
`
	main := container("restic", resticImage, script, append(runEnv(a, run), kube.Obj{"name": "SB_UPLOAD_URL", "value": uploadURL},
		kube.Obj{"name": "SB_EXCLUDE", "value": strings.Join(a.BackupExclude, " ")}), resticMounts, 0)
	main["envFrom"] = []any{envFromSecret()}
	return jobSpec{name: "backup-" + a.Name + "-" + run.ID, ns: apps.Namespace(a.Name), app: a, run: run,
		init: inits, main: main, volumes: vols, nodeName: nodeName}.obj()
}

func restoreTestJob(a *apps.App, run *apps.BackupRun, expectFiles int, expectTables int) kube.Obj {
	vols := []any{kube.Obj{"name": "restore", "emptyDir": kube.Obj{}}}
	mounts := []any{kube.Obj{"name": "restore", "mountPath": "/restore"}}
	restore := `set -eu
` + resticFn + `r restore "$SB_SNAPSHOT:/sb" --target /restore --verify >/tmp/out 2>&1 || { tail -5 /tmp/out; echo "SB_ERROR restic could not restore or verify snapshot $SB_SNAPSHOT"; exit 1; }
tail -2 /tmp/out
n=$(find /restore/vol -type f 2>/dev/null | wc -l)
echo "SB_FILES $n"
if [ "$n" != "$SB_EXPECT_FILES" ]; then echo "SB_ERROR restored $n files, the backup had $SB_EXPECT_FILES"; exit 1; fi
for f in $SB_REQUIRE; do
  if [ ! -s "/restore/vol/$f" ]; then echo "SB_ERROR required file $f is missing or empty"; exit 1; fi
done
chmod -R a+rX /restore
`
	env := append(runEnv(a, run),
		kube.Obj{"name": "SB_EXPECT_FILES", "value": strconv.Itoa(expectFiles)},
		kube.Obj{"name": "SB_REQUIRE", "value": strings.Join(a.BackupRequire, " ")},
		kube.Obj{"name": "SB_EXPECT_TABLES", "value": strconv.Itoa(expectTables)})
	init := container("restore", resticImage, restore, env, mounts, 0)
	init["envFrom"] = []any{envFromSecret()}
	var main kube.Obj
	if a.Database {
		check := `set -eu
export PGDATA=/tmp/pg
initdb -U postgres --auth=trust >/dev/null
pg_ctl -o "-c listen_addresses='' -k /tmp" -w start >/dev/null
createdb -h /tmp -U postgres restored
pg_restore -h /tmp -U postgres -d restored --no-owner --no-acl /restore/db/db.dump 2>/tmp/err || echo "note: pg_restore reported $(grep -c ERROR /tmp/err) errors"
t=$(psql -h /tmp -U postgres -d restored -Atc "` + countTablesSQL + `")
rows=$(psql -h /tmp -U postgres -d restored -Atc "` + countRowsSQL + `")
echo "SB_DB_TABLES $t"
echo "SB_DB_ROWS $rows"
if [ "$t" != "$SB_EXPECT_TABLES" ]; then echo "SB_ERROR restored database has $t tables, the backup had $SB_EXPECT_TABLES"; exit 1; fi
`
		main = container("check", postgresImage, check, env, mounts, 999)
	} else {
		main = container("check", checkImage, `echo "files checked"`, env, mounts, 65534)
	}
	return jobSpec{name: "restoretest-" + a.Name + "-" + run.ID, ns: restoreTestNS, app: a, run: run,
		init: []any{init}, main: main, volumes: vols}.obj()
}

func restoreJob(a *apps.App, run *apps.BackupRun) kube.Obj {
	vols, mounts := dataVolumes(a)
	vols = append(vols, kube.Obj{"name": "restore", "emptyDir": kube.Obj{}})
	mounts = append(mounts, kube.Obj{"name": "restore", "mountPath": "/restore"})
	var names []string
	for _, v := range a.Volumes {
		names = append(names, v.Name)
	}
	restore := `set -eu
` + resticFn + `r restore "$SB_SNAPSHOT:/sb" --target /restore --verify >/tmp/out 2>&1 || { tail -5 /tmp/out; echo "SB_ERROR restic could not restore snapshot $SB_SNAPSHOT"; exit 1; }
for v in $SB_VOLUMES; do
  if [ ! -d "/restore/vol/$v" ]; then echo "SB_ERROR snapshot has no volume $v"; exit 1; fi
  find "/sb/vol/$v" -mindepth 1 -delete
  cp -a "/restore/vol/$v/." "/sb/vol/$v/"
  echo "restored volume $v: $(find "/sb/vol/$v" -type f | wc -l) files"
done
echo "SB_FILES $(find /sb/vol -type f 2>/dev/null | wc -l)"
chmod -R a+rX /restore
`
	env := append(runEnv(a, run), kube.Obj{"name": "SB_VOLUMES", "value": strings.Join(names, " ")})
	init := container("restore", resticImage, restore, env, mounts, 0)
	init["envFrom"] = []any{envFromSecret()}
	var main kube.Obj
	if a.Database {
		load := `set -eu
` + waitDB + `pg_restore --clean --if-exists --no-owner --no-acl -d "$SB_DB_URI" /restore/db/db.dump 2>/tmp/err || echo "note: pg_restore reported $(grep -c ERROR /tmp/err) errors"
echo "SB_DB_TABLES $(psql "$SB_DB_URI" -Atc "` + countTablesSQL + `")"
`
		main = container("database", postgresImage, load, append(append(env, dbURIEnv()...), kube.Obj{"name": "SB_DB_WAIT_TRIES", "value": "200"}), []any{kube.Obj{"name": "restore", "mountPath": "/restore"}}, 999)
	} else {
		main = container("database", checkImage, `echo "no database"`, env, nil, 65534)
	}
	return jobSpec{name: "restore-" + a.Name + "-" + run.ID, ns: apps.Namespace(a.Name), app: a, run: run,
		init: []any{init}, main: main, volumes: vols}.obj()
}

// --- state machine ---

var errBackupBusy = errors.New("a backup, restore test or restore is already running for this app")

func now() string { return time.Now().UTC().Format(time.RFC3339) }

// startBackupRun records a new run. Runs that need the app stopped begin in "stopping".
func (s *Server) startBackupRun(ctx context.Context, a *apps.App, kind, trigger, snapshot string) (*apps.BackupRun, error) {
	if !a.HasData() && kind != "backup" {
		return nil, fmt.Errorf("app %s has no volume or database; its backups hold only its settings and source, which bring it back if it is deleted (sb restore %s --recreate)", a.Name, a.Name)
	}
	c, err := s.backupConfig(ctx)
	if err != nil {
		return nil, err
	}
	if c == nil {
		return nil, errors.New("backups are not configured; run: sb backups configure")
	}
	if err := s.ensureRepo(ctx, c); err != nil {
		return nil, err
	}
	if a.Backup == nil {
		a.Backup = &apps.BackupState{}
	}
	if a.Backup.Running != nil {
		return nil, errBackupBusy
	}
	if kind != "backup" && snapshot == "" {
		if a.Backup.LastBackup == nil || a.Backup.LastBackup.Phase != "succeeded" {
			return nil, errors.New("there is no successful backup yet; run: sb backup " + a.Name)
		}
		snapshot = a.Backup.LastBackup.Snapshot
	}
	run := &apps.BackupRun{ID: randID(6), Kind: kind, Trigger: trigger, Snapshot: snapshot, StartedAt: now(), Phase: "running"}
	if kind == "restore" || (kind == "backup" && a.BackupStop && len(a.Volumes) > 0) {
		run.Phase = "stopping"
		a.Paused = true
	}
	a.Backup.Running = run
	if run.Phase == "running" {
		if err := s.launch(ctx, c, a, run); err != nil {
			a.Backup.Running = nil
			return nil, err
		}
	}
	if err := s.saveInternal(ctx, a); err != nil {
		return nil, err
	}
	if a.Paused {
		s.reconcile(ctx, a)
	}
	return run, nil
}

func (s *Server) launch(ctx context.Context, c *BackupConfig, a *apps.App, run *apps.BackupRun) error {
	switch run.Kind {
	case "backup":
		if err := s.envSecret(ctx, c, apps.Namespace(a.Name)); err != nil {
			return err
		}
		node := ""
		if !a.Paused && len(a.Volumes) > 0 {
			// A live volume is attached to the app's node; the job must run there too.
			// (in v1 all of an app's volumes belong to one component)
			node = s.volumeNode(ctx, a, a.Volumes[0].Name)
		}
		if err := s.writeBackupMeta(ctx, a); err != nil {
			return err
		}
		return s.kube.Apply(ctx, backupJob(a, run, node, s.uploadURL(a)))
	case "test":
		if err := s.kube.Apply(ctx, kube.Obj{"apiVersion": "v1", "kind": "Namespace", "metadata": kube.Obj{"name": restoreTestNS,
			"labels": kube.Obj{"pod-security.kubernetes.io/enforce": "baseline", apps.ManagedByLabel: "slicedbread"}}}); err != nil {
			return err
		}
		if err := s.envSecret(ctx, c, restoreTestNS); err != nil {
			return err
		}
		files, tables := -1, 0
		for _, h := range a.Backup.History {
			if h.Snapshot == run.Snapshot {
				files, tables = h.Files, h.DBTables
			}
		}
		if files < 0 {
			return fmt.Errorf("snapshot %s is not in this app's backup history", run.Snapshot)
		}
		return s.kube.Apply(ctx, restoreTestJob(a, run, files, tables))
	case "restore":
		if err := s.envSecret(ctx, c, apps.Namespace(a.Name)); err != nil {
			return err
		}
		return s.kube.Apply(ctx, restoreJob(a, run))
	}
	return fmt.Errorf("unknown run kind %s", run.Kind)
}

func jobName(a *apps.App, run *apps.BackupRun) (ns, name string) {
	switch run.Kind {
	case "test":
		return restoreTestNS, "restoretest-" + a.Name + "-" + run.ID
	case "restore":
		return apps.Namespace(a.Name), "restore-" + a.Name + "-" + run.ID
	}
	return apps.Namespace(a.Name), "backup-" + a.Name + "-" + run.ID
}

// runOutput collects SB_* lines from every container of the run's pod.
func (s *Server) runOutput(ctx context.Context, ns, job string) map[string]string {
	out := map[string]string{}
	pods, err := s.kube.List(ctx, "v1", "Pod", ns, "job-name="+job)
	if err != nil || len(pods) == 0 {
		return out
	}
	name := kube.Str(pods[0], "metadata", "name")
	for _, c := range []string{"dump", "restore", "restic", "check", "database", "build"} {
		l, err := s.kube.Logs(ctx, ns, name, c, 200)
		if err != nil {
			continue
		}
		for _, line := range strings.Split(l, "\n") {
			if k, v, ok := strings.Cut(strings.TrimSpace(line), " "); ok && strings.HasPrefix(k, "SB_") {
				out[k] = v
			}
		}
	}
	return out
}

// checkBackups advances an app's backup state. Returns true if the app changed.
func (s *Server) checkBackups(ctx context.Context, a *apps.App, c *BackupConfig) bool {
	b := a.Backup
	if b == nil || b.Running == nil {
		if a.Paused {
			a.Paused = false // left over from an interrupted run
			return true
		}
		// Apps without data are backed up too: their snapshot holds the spec, secret values
		// and uploaded source, which is what a whole-install restore needs.
		if c != nil && s.backupDue(ctx, a, c, time.Now().UTC()) {
			trigger := "schedule"
			if a.Backup == nil || a.Backup.LastBackup == nil {
				trigger = "initial"
			}
			if _, err := s.startBackupRun(ctx, a, "backup", trigger, ""); err != nil {
				s.logf("scheduled backup %s: %v", a.Name, err)
			}
			return false // startBackupRun saved
		}
		return false
	}
	run := b.Running
	if run.Phase == "stopping" {
		if c == nil {
			return s.finishRun(a, run, "failed", "backups are no longer configured")
		}
		if n := s.countPods(ctx, apps.Namespace(a.Name), a.Name); n > 0 {
			return false
		}
		run.Phase = "running"
		if err := s.launch(ctx, c, a, run); err != nil {
			return s.finishRun(a, run, "failed", "could not start: "+err.Error())
		}
		return true
	}
	ns, name := jobName(a, run)
	var job kube.Obj
	if err := s.kube.Get(ctx, "batch/v1", "Job", ns, name, &job); err != nil {
		if kube.IsNotFound(err) {
			return s.finishRun(a, run, "failed", "job disappeared")
		}
		return false
	}
	succeeded, failed := kube.Num(job, "status", "succeeded") > 0, kube.Num(job, "status", "failed") > 0
	if !succeeded && !failed {
		return false
	}
	out := s.runOutput(ctx, ns, name)
	run.Files, _ = strconv.Atoi(out["SB_FILES"])
	run.DBTables, _ = strconv.Atoi(out["SB_DB_TABLES"])
	run.DBRows, _ = strconv.ParseInt(out["SB_DB_ROWS"], 10, 64)
	run.Bytes, _ = strconv.ParseInt(out["SB_BYTES"], 10, 64)
	if run.Kind == "backup" {
		run.Snapshot = out["SB_SNAPSHOT"]
	}
	if failed {
		msg := out["SB_ERROR"]
		if msg == "" {
			msg = "job failed; inspect with: kubectl -n " + ns + " logs job/" + name + " --all-containers"
		}
		return s.finishRun(a, run, "failed", msg)
	}
	if run.Kind == "backup" && run.Snapshot == "" {
		return s.finishRun(a, run, "failed", "restic did not report a snapshot id")
	}
	s.finishRun(a, run, "succeeded", "")
	if run.Kind == "backup" && a.HasData() {
		// Every backup is followed by a restore test of that snapshot.
		test := &apps.BackupRun{ID: randID(6), Kind: "test", Trigger: "after backup " + run.ID, Snapshot: run.Snapshot, StartedAt: now(), Phase: "running"}
		a.Backup.Running = test
		if c == nil {
			s.finishRun(a, test, "failed", "backups are no longer configured")
		} else if err := s.launch(ctx, c, a, test); err != nil {
			s.finishRun(a, test, "failed", "could not start: "+err.Error())
		}
	}
	return true
}

func (s *Server) finishRun(a *apps.App, run *apps.BackupRun, phase, msg string) bool {
	run.Phase, run.Message, run.FinishedAt = phase, msg, now()
	b := a.Backup
	b.Running = nil
	switch run.Kind {
	case "backup":
		b.LastBackup = run
		if phase == "succeeded" {
			b.History = append([]apps.BackupRun{*run}, b.History...)
			if len(b.History) > historyLimit {
				b.History = b.History[:historyLimit]
			}
		}
	case "test":
		b.LastTest = run
	case "restore":
		b.LastRestore = run
		s.obsoleteRestoreApprovals(a.Name)
	}
	if run.Kind != "test" {
		a.Paused = false
	}
	if phase == "failed" {
		s.logf("%s %s for %s failed: %s", run.Kind, run.ID, a.Name, msg)
	}
	return true
}

func (s *Server) countPods(ctx context.Context, ns, app string) int {
	pods, err := s.kube.List(ctx, "v1", "Pod", ns, apps.AppLabel+"="+app)
	if err != nil {
		return 1
	}
	n := 0
	for _, p := range pods {
		// Backup and restore job pods carry the app label too; only count the app itself.
		if kube.Str(p, "metadata", "labels", "slicedbread.dev/backup-run") == "" {
			n++
		}
	}
	return n
}

// backupDue reports whether today's scheduled backup has not run yet and the app is up.
func (s *Server) backupDue(ctx context.Context, a *apps.App, c *BackupConfig, t time.Time) bool {
	sched := time.Date(t.Year(), t.Month(), t.Day(), c.Hour, 0, 0, 0, time.UTC)
	if t.Before(sched) {
		sched = sched.Add(-24 * time.Hour)
	}
	if lb := a.Backup; lb != nil && lb.LastBackup != nil {
		if last, err := time.Parse(time.RFC3339, lb.LastBackup.StartedAt); err == nil && !last.Before(sched) {
			// Done for today, unless it failed: then retry hourly.
			if lb.LastBackup.Phase != "failed" || t.Sub(last) < time.Hour {
				return false
			}
		}
	}
	// Only back up a settled app: running, latest version serving, template setup finished.
	if st := s.status(ctx, a); st.State != "running" {
		return false
	}
	if a.Database {
		var cl kube.Obj
		if err := s.kube.Get(ctx, "postgresql.cnpg.io/v1", "Cluster", apps.Namespace(a.Name), "db", &cl); err != nil || kube.Str(cl, "status", "phase") != "Cluster in healthy state" {
			return false
		}
	}
	return true
}

// BackupSummary is one line of `sb backups`.
func backupSummary(a *apps.App) string {
	if !a.HasData() {
		if b := a.Backup; b != nil && b.LastBackup != nil {
			if b.LastBackup.Phase == "failed" {
				return "settings backup FAILED " + b.LastBackup.StartedAt + ": " + b.LastBackup.Message
			}
			return "no data; settings and source backed up " + b.LastBackup.StartedAt
		}
		return ""
	}
	b := a.Backup
	if b == nil || (b.LastBackup == nil && b.Running == nil) {
		return "no backup yet"
	}
	var parts []string
	if b.Running != nil {
		parts = append(parts, fmt.Sprintf("%s %s (%s)", b.Running.Kind, b.Running.Phase, b.Running.ID))
	}
	if l := b.LastBackup; l != nil {
		if l.Phase == "succeeded" {
			parts = append(parts, fmt.Sprintf("last backup %s snapshot %s", l.StartedAt, l.Snapshot))
		} else {
			parts = append(parts, fmt.Sprintf("last backup FAILED %s: %s", l.StartedAt, l.Message))
		}
	}
	if t := b.LastTest; t != nil {
		if t.Phase == "succeeded" {
			parts = append(parts, fmt.Sprintf("restore test passed %s", t.FinishedAt))
		} else {
			parts = append(parts, fmt.Sprintf("restore test FAILED %s: %s", t.FinishedAt, t.Message))
		}
	}
	if r := b.LastRestore; r != nil {
		if r.Phase == "succeeded" {
			parts = append(parts, fmt.Sprintf("restored from %s at %s", short(r.Snapshot), r.FinishedAt))
		} else {
			parts = append(parts, fmt.Sprintf("restore FAILED %s: %s", r.FinishedAt, r.Message))
		}
	}
	return strings.Join(parts, "; ")
}

func short(id string) string {
	if len(id) > 12 {
		return id[:12]
	}
	return id
}

// obsoleteRestoreApprovals retires pending restore requests once a restore has run,
// so a late approval cannot replace the data a second time.
func (s *Server) obsoleteRestoreApprovals(app string) {
	if s.kube == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	list, err := s.listApprovals(ctx)
	if err != nil {
		return
	}
	for _, ap := range list {
		if ap.App == app && ap.Kind == "restore" && ap.State == "pending" {
			ap.State, ap.Result = "obsolete", "a restore already ran; ask again if another is needed"
			s.saveApproval(ctx, ap)
		}
	}
}

// backupShort is the dashboard's one-word backup state.
func backupShort(a *apps.App, now time.Time) string {
	if !a.HasData() {
		if b := a.Backup; b != nil && b.LastBackup != nil && b.LastBackup.Phase == "failed" {
			return "settings backup FAILED"
		}
		return "no data (settings saved)"
	}
	b := a.Backup
	switch {
	case b == nil || (b.LastBackup == nil && b.Running == nil):
		return "none yet"
	case b.Running != nil:
		return b.Running.Kind + " running"
	case b.LastBackup != nil && b.LastBackup.Phase == "failed":
		return "FAILED"
	case b.LastTest != nil && b.LastTest.Phase == "failed":
		return "restore test FAILED"
	}
	if len(b.History) > 0 {
		tested := "test pending"
		if b.LastTest != nil && b.LastTest.Phase == "succeeded" && b.LastTest.Snapshot == b.History[0].Snapshot {
			tested = "tested"
		}
		if t, err := time.Parse(time.RFC3339, b.History[0].StartedAt); err == nil {
			ago := now.Sub(t)
			if ago < time.Hour {
				return "ok, " + tested + ", under 1h ago"
			}
			return fmt.Sprintf("ok, %s, %dh ago", tested, int(ago.Hours()))
		}
	}
	return "ok"
}
