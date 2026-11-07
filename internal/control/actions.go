package control

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/melzi-cloud/sliced-bread/internal/apps"
	"github.com/melzi-cloud/sliced-bread/internal/kube"
)

// Template actions run as Jobs next to the app (D-029). The script and the other template
// files are mounted from a ConfigMap; the job gets only the secrets the action declares;
// it can reach its app, its database and DNS (plus HTTPS to the internet when the action
// declares egress). The last JSON line it prints is the result.

const (
	actionFilesCM = "sb-template-files"
	actionDir     = "/sb/template"
	actionVolDir  = "/volumes"
	// Extra time beyond the action's own limit for scheduling and pulling the image.
	actionStartSlack = 3 * time.Minute
)

func dashed(s string) string { return strings.ReplaceAll(s, "_", "-") }

// actionFilesObj holds the template's files for action jobs. ConfigMap keys cannot hold
// "/", so each file gets a key and an item that puts it back at its path.
func actionFilesObj(a *apps.App) (kube.Obj, []any) {
	data := kube.Obj{}
	var items []any
	paths := make([]string, 0, len(a.TemplateDef.Files))
	for p := range a.TemplateDef.Files {
		paths = append(paths, p)
	}
	sort.Strings(paths)
	for i, p := range paths {
		key := "f" + strconv.Itoa(i)
		data[key] = a.TemplateDef.Files[p]
		items = append(items, kube.Obj{"key": key, "path": p, "mode": 0o555})
	}
	cm := kube.Obj{"apiVersion": "v1", "kind": "ConfigMap",
		"metadata": kube.Obj{"name": actionFilesCM, "namespace": apps.Namespace(a.Name), "labels": kube.Obj{apps.AppLabel: a.Name, apps.ManagedByLabel: "slicedbread"}},
		"data":     data}
	return cm, items
}

// volumeNode returns the node running the component that mounts a volume (RWO volumes
// can only be mounted there).
func (s *Server) volumeNode(ctx context.Context, a *apps.App, vol string) string {
	for _, c := range apps.Components(a) {
		for _, v := range c.Volumes {
			if v.Name != vol {
				continue
			}
			pods, _ := s.kube.List(ctx, "v1", "Pod", apps.Namespace(a.Name), apps.AppLabel+"="+a.Name+","+apps.ComponentLabel+"="+c.Name)
			for _, p := range pods {
				if n := kube.Str(p, "spec", "nodeName"); n != "" && kube.Str(p, "status", "phase") == "Running" && kube.Str(p, "metadata", "deletionTimestamp") == "" {
					return n
				}
			}
		}
	}
	return ""
}

// actionJob builds the Job for one run of an action.
func (s *Server) actionJob(ctx context.Context, a *apps.App, act *apps.Action, input map[string]string, id string) (kube.Obj, error) {
	ns := apps.Namespace(a.Name)
	cm, items := actionFilesObj(a)
	if err := s.kube.Apply(ctx, cm); err != nil {
		return nil, err
	}
	primary := apps.Primary(a)
	env := []any{
		kube.Obj{"name": "SB_APP_INTERNAL", "value": fmt.Sprintf("http://%s.%s.svc.cluster.local:%d", apps.ServiceName(a, primary.Name), ns, primary.Port)},
		kube.Obj{"name": "SB_APP_URL", "value": "https://" + s.hosts().App(a.Name)},
		kube.Obj{"name": "SB_APP_NAME", "value": a.Name},
		kube.Obj{"name": "HOME", "value": "/tmp"},
	}
	names := make([]string, 0, len(input))
	for k := range input {
		names = append(names, k)
	}
	sort.Strings(names)
	for _, k := range names {
		env = append(env, kube.Obj{"name": "SB_INPUT_" + strings.ToUpper(k), "value": input[k]})
	}
	if act.Database {
		env = append(env, kube.Obj{"name": "SB_DB_URL", "valueFrom": kube.Obj{"secretKeyRef": kube.Obj{"name": apps.DBSecret, "key": "uri"}}})
	}
	for _, sec := range act.Secrets {
		env = append(env, kube.Obj{"name": sec, "valueFrom": kube.Obj{"secretKeyRef": kube.Obj{"name": apps.AppSecret, "key": sec}}})
	}
	mounts := []any{
		kube.Obj{"name": "template", "mountPath": actionDir, "readOnly": true},
		kube.Obj{"name": "tmp", "mountPath": "/tmp"},
	}
	vols := []any{
		kube.Obj{"name": "template", "configMap": kube.Obj{"name": actionFilesCM, "items": items, "defaultMode": 0o555}},
		kube.Obj{"name": "tmp", "emptyDir": kube.Obj{"sizeLimit": "512Mi"}},
	}
	node := ""
	for _, v := range act.Volumes {
		mounts = append(mounts, kube.Obj{"name": "vol-" + v, "mountPath": actionVolDir + "/" + v, "readOnly": true})
		// Read-only inside the container only: the CSI driver cannot publish a volume
		// read-only on a node where the app has it mounted read-write ("Resource busy").
		vols = append(vols, kube.Obj{"name": "vol-" + v, "persistentVolumeClaim": kube.Obj{"claimName": "data-" + v}})
		if n := s.volumeNode(ctx, a, v); n != "" {
			node = n
		} else {
			return nil, fmt.Errorf("action %s reads volume %s, but the app is not running", act.Name, v)
		}
	}
	pod := kube.Obj{
		"restartPolicy": "Never", "automountServiceAccountToken": false, "enableServiceLinks": false,
		"securityContext": kube.Obj{"runAsNonRoot": true, "runAsUser": 65534, "runAsGroup": 65534, "seccompProfile": kube.Obj{"type": "RuntimeDefault"}},
		"containers": []any{kube.Obj{
			"name": "action", "image": act.Image, "imagePullPolicy": "IfNotPresent",
			"command":      []any{actionDir + "/" + act.Run},
			"workingDir":   actionDir,
			"env":          env,
			"volumeMounts": mounts,
			"securityContext": kube.Obj{"allowPrivilegeEscalation": false, "readOnlyRootFilesystem": true,
				"capabilities": kube.Obj{"drop": []any{"ALL"}}},
			"resources": kube.Obj{"requests": kube.Obj{"cpu": "20m", "memory": "64Mi"}, "limits": kube.Obj{"memory": "512Mi"}},
		}},
		"volumes": vols,
	}
	if node != "" {
		pod["nodeName"] = node
	}
	if s.cfg.SelfImage != "" {
		// A new pod's network policy rules arrive a few seconds after it starts; until then
		// connections to the app are refused. Wait until the app answers (at most 60 s).
		pod["initContainers"] = []any{kube.Obj{
			"name": "wait", "image": s.cfg.SelfImage, "imagePullPolicy": "IfNotPresent",
			"env": []any{
				kube.Obj{"name": "SB_MODE", "value": "wait"},
				kube.Obj{"name": "SB_WAIT_ADDR", "value": fmt.Sprintf("%s.%s.svc.cluster.local:%d", apps.ServiceName(a, primary.Name), ns, primary.Port)},
			},
			"securityContext": kube.Obj{"allowPrivilegeEscalation": false, "readOnlyRootFilesystem": true, "capabilities": kube.Obj{"drop": []any{"ALL"}}},
			"resources":       kube.Obj{"requests": kube.Obj{"cpu": "10m", "memory": "16Mi"}, "limits": kube.Obj{"memory": "64Mi"}},
		}}
	}
	deadline := int((act.TimeoutDuration() + actionStartSlack).Seconds())
	return kube.Obj{
		"apiVersion": "batch/v1", "kind": "Job",
		"metadata": kube.Obj{"name": "action-" + dashed(act.Name) + "-" + id, "namespace": ns,
			"labels": kube.Obj{apps.AppLabel: a.Name, apps.ActionLabel: dashed(act.Name), apps.ManagedByLabel: "slicedbread"}},
		"spec": kube.Obj{
			"backoffLimit": 0, "activeDeadlineSeconds": deadline, "ttlSecondsAfterFinished": 900,
			"template": kube.Obj{
				// No app label on the pod: app pods are counted for status and backups.
				"metadata": kube.Obj{"labels": kube.Obj{apps.ActionLabel: dashed(act.Name)}},
				"spec":     pod,
			},
		},
	}, nil
}

// checkInput validates an action's input against its declaration.
func checkInput(act *apps.Action, input map[string]string) error {
	for k := range input {
		if _, ok := act.Input[k]; !ok {
			return fmt.Errorf("action %s has no input %q", act.Name, k)
		}
	}
	for k, f := range act.Input {
		v, ok := input[k]
		if f.Required && (!ok || strings.TrimSpace(v) == "") {
			return fmt.Errorf("action %s needs input %s (%s): --input %s=...", act.Name, k, f.Description, k)
		}
		if !ok || v == "" {
			continue
		}
		switch f.Type {
		case "number":
			if _, err := strconv.ParseFloat(v, 64); err != nil {
				return fmt.Errorf("input %s must be a number", k)
			}
		case "boolean":
			if v != "true" && v != "false" {
				return fmt.Errorf("input %s must be true or false", k)
			}
		}
		if len(v) > 2000 || strings.ContainsRune(v, 0) {
			return fmt.Errorf("input %s is too long", k)
		}
	}
	return nil
}

// startAction creates the action's Job and returns its name.
func (s *Server) startAction(ctx context.Context, a *apps.App, act *apps.Action, input map[string]string) (string, error) {
	if a.TemplateDef == nil {
		return "", errors.New("only template apps have actions")
	}
	if err := checkInput(act, input); err != nil {
		return "", err
	}
	ns := apps.Namespace(a.Name)
	if len(act.Egress) > 0 {
		if err := s.kube.Apply(ctx, apps.ActionEgressPolicy(ns, a.Name, dashed(act.Name))); err != nil {
			return "", err
		}
	}
	job, err := s.actionJob(ctx, a, act, input, randID(6))
	if err != nil {
		return "", err
	}
	if err := s.kube.Apply(ctx, job); err != nil {
		return "", err
	}
	return kube.Str(job, "metadata", "name"), nil
}

// actionOutcome reads a finished (or still running) action job. done is false while it
// runs. The result is the raw last JSON object printed; err carries the failure message.
func (s *Server) actionOutcome(ctx context.Context, a *apps.App, jobName string) (done bool, result map[string]any, err error) {
	ns := apps.Namespace(a.Name)
	var job kube.Obj
	if e := s.kube.Get(ctx, "batch/v1", "Job", ns, jobName, &job); e != nil {
		if kube.IsNotFound(e) {
			return true, nil, errors.New("action job disappeared")
		}
		return false, nil, nil
	}
	ok, bad := kube.Num(job, "status", "succeeded") > 0, kube.Num(job, "status", "failed") > 0
	if !ok && !bad {
		for _, c := range kube.Slice(job, "status", "conditions") {
			m, _ := c.(map[string]any)
			if kube.Str(m, "type") == "Failed" && kube.Str(m, "status") == "True" {
				bad = true
			}
		}
		if !bad {
			if p := s.actionPodProblem(ctx, ns, jobName); p != "" {
				return true, nil, errors.New(p)
			}
			return false, nil, nil
		}
	}
	logs := ""
	if pods, e := s.kube.List(ctx, "v1", "Pod", ns, "job-name="+jobName); e == nil && len(pods) > 0 {
		logs, _ = s.kube.Logs(ctx, ns, kube.Str(pods[0], "metadata", "name"), "action", 200)
	}
	lines := strings.Split(strings.TrimSpace(logs), "\n")
	if bad {
		msg := errorLine(lines)
		if strings.Contains(kube.Str(job, "status", "conditions"), "DeadlineExceeded") || deadlineExceeded(job) {
			msg = "action timed out"
		}
		return true, nil, errors.New(msg)
	}
	for i := len(lines) - 1; i >= 0; i-- {
		l := strings.TrimSpace(lines[i])
		if strings.HasPrefix(l, "{") {
			var out map[string]any
			if json.Unmarshal([]byte(l), &out) == nil {
				return true, out, nil
			}
		}
	}
	return true, nil, errors.New("the action printed no JSON result")
}

func deadlineExceeded(job kube.Obj) bool {
	for _, c := range kube.Slice(job, "status", "conditions") {
		m, _ := c.(map[string]any)
		if kube.Str(m, "reason") == "DeadlineExceeded" {
			return true
		}
	}
	return false
}

// actionPodProblem reports a pod that will never run (bad image, missing secret).
func (s *Server) actionPodProblem(ctx context.Context, ns, job string) string {
	pods, _ := s.kube.List(ctx, "v1", "Pod", ns, "job-name="+job)
	for _, p := range pods {
		for _, c := range kube.Slice(p, "status", "containerStatuses") {
			m, _ := c.(map[string]any)
			switch r := kube.Str(m, "state", "waiting", "reason"); r {
			case "ErrImagePull", "ImagePullBackOff", "CreateContainerConfigError", "InvalidImageName":
				return "action cannot start: " + r + ": " + kube.Str(m, "state", "waiting", "message")
			}
		}
	}
	return ""
}

// checkOutput keeps the declared output fields and checks their types.
func checkOutput(act *apps.Action, out map[string]any) (map[string]any, error) {
	if len(act.Output) == 0 {
		return out, nil
	}
	kept := map[string]any{}
	for k, f := range act.Output {
		v, ok := out[k]
		if !ok {
			continue
		}
		okType := false
		switch f.Type {
		case "string":
			_, okType = v.(string)
		case "number":
			_, okType = v.(float64)
		case "boolean":
			_, okType = v.(bool)
		case "object":
			_, okType = v.(map[string]any)
		case "array":
			_, okType = v.([]any)
		}
		if !okType && v != nil {
			return nil, fmt.Errorf("action %s returned %s with the wrong type (want %s)", act.Name, k, f.Type)
		}
		kept[k] = v
	}
	return kept, nil
}

// maskSecrets replaces any of the app's secret values in v with [secret].
func maskSecrets(v any, secrets []string) any {
	switch x := v.(type) {
	case string:
		for _, s := range secrets {
			x = strings.ReplaceAll(x, s, "[secret]")
		}
		return x
	case map[string]any:
		out := map[string]any{}
		for k, e := range x {
			out[k] = maskSecrets(e, secrets)
		}
		return out
	case []any:
		out := make([]any, len(x))
		for i, e := range x {
			out[i] = maskSecrets(e, secrets)
		}
		return out
	}
	return v
}

func (s *Server) secretValuesForMask(ctx context.Context, a *apps.App) []string {
	vals, _ := s.appSecret(ctx, a)
	var out []string
	for _, v := range vals {
		if len(v) >= 6 {
			out = append(out, v)
		}
	}
	if a.Database {
		var sec kube.Obj
		if s.kube.Get(ctx, "v1", "Secret", apps.Namespace(a.Name), apps.DBSecret, &sec) == nil {
			for _, k := range []string{"password", "uri"} {
				if v := secretField(sec, k); len(v) >= 6 {
					out = append(out, v)
				}
			}
		}
	}
	// Longest first, so a URI containing a password is replaced whole.
	sort.Slice(out, func(i, j int) bool { return len(out[i]) > len(out[j]) })
	return out
}

// ActionResult is what a run returns. Secret holds outputs marked secret: they are never
// sent to API clients, only shown to a signed-in human in the dashboard (D-030).
type ActionResult struct {
	Result map[string]any
	Secret map[string]any
}

// runAction runs an action and waits for its result.
func (s *Server) runAction(ctx context.Context, a *apps.App, name string, input map[string]string) (*ActionResult, error) {
	t := a.TemplateDef
	if t == nil {
		return nil, fmt.Errorf("app %s has no actions (it was not deployed from a template)", a.Name)
	}
	act := t.Action(name)
	if act == nil {
		return nil, fmt.Errorf("app %s has no action %q", a.Name, name)
	}
	job, err := s.startAction(ctx, a, act, input)
	if err != nil {
		return nil, err
	}
	deadline := time.Now().Add(act.TimeoutDuration() + actionStartSlack)
	mask := s.secretValuesForMask(ctx, a)
	for time.Now().Before(deadline) {
		done, out, err := s.actionOutcome(ctx, a, job)
		if done {
			if err != nil {
				return nil, errors.New(maskSecrets(err.Error(), mask).(string))
			}
			out, err = checkOutput(act, out)
			if err != nil {
				return nil, err
			}
			res := &ActionResult{Result: map[string]any{}, Secret: map[string]any{}}
			for k, v := range out {
				if act.Output[k].Secret {
					res.Secret[k] = v
				} else {
					res.Result[k] = maskSecrets(v, mask)
				}
			}
			return res, nil
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(time.Second):
		}
	}
	return nil, fmt.Errorf("action %s did not finish in time", name)
}

// --- install actions and setup checks (run by the tick) ---

// hookRun is the progress of a background action run, stored in App.Hooks as JSON.
type hookRun struct {
	Phase   string `json:"phase"` // running, done, failed
	Job     string `json:"job,omitempty"`
	At      int64  `json:"at"` // when the phase began
	Attempt int    `json:"attempt,omitempty"`
	Msg     string `json:"msg,omitempty"`
}

func getRun(a *apps.App, key string) hookRun {
	var r hookRun
	json.Unmarshal([]byte(a.Hooks[key]), &r)
	return r
}

func setRun(a *apps.App, key string, r hookRun) {
	if a.Hooks == nil {
		a.Hooks = map[string]string{}
	}
	b, _ := json.Marshal(r)
	a.Hooks[key] = string(b)
}

func installKey(name string) string { return "install/" + name }
func setupKey(name string) string   { return "setup/" + name }

func installDone(a *apps.App, name string) bool { return getRun(a, installKey(name)).Phase == "done" }

// pendingInstall returns the first install action not done yet, or nil.
func pendingInstall(a *apps.App) *apps.Action {
	if a.TemplateDef == nil {
		return nil
	}
	for i := range a.TemplateDef.Actions {
		act := &a.TemplateDef.Actions[i]
		if act.On == "install" && !installDone(a, act.Name) {
			return act
		}
	}
	return nil
}

// backoff is the wait before retrying a failed background run.
func backoff(attempt int) time.Duration {
	d := 15 * time.Second << min(attempt, 6)
	if d > 10*time.Minute {
		d = 10 * time.Minute
	}
	return d
}

// progressRun advances one background run: starts it when due, collects its result.
// Returns whether the app changed, and the result when it just finished successfully.
func (s *Server) progressRun(ctx context.Context, a *apps.App, key string, act *apps.Action, every time.Duration) (bool, map[string]any) {
	r := getRun(a, key)
	now := time.Now()
	switch r.Phase {
	case "running":
		done, out, err := s.actionOutcome(ctx, a, r.Job)
		if !done {
			if now.Sub(time.Unix(r.At, 0)) > act.TimeoutDuration()+actionStartSlack {
				err, done = errors.New("action did not finish in time"), true
			} else {
				return false, nil
			}
		}
		if err == nil {
			out, err = checkOutput(act, out)
		}
		if err != nil {
			mask := s.secretValuesForMask(ctx, a)
			r = hookRun{Phase: "failed", At: now.Unix(), Attempt: r.Attempt + 1, Msg: maskSecrets(err.Error(), mask).(string)}
			s.logf("%s %s: action %s failed (attempt %d): %s", a.Name, key, act.Name, r.Attempt, r.Msg)
			setRun(a, key, r)
			return true, nil
		}
		setRun(a, key, hookRun{Phase: "done", At: now.Unix()})
		return true, out
	case "failed":
		if now.Sub(time.Unix(r.At, 0)) < backoff(r.Attempt-1) {
			return false, nil
		}
	case "done":
		if every == 0 || now.Sub(time.Unix(r.At, 0)) < every {
			return false, nil
		}
	}
	job, err := s.startAction(ctx, a, act, nil)
	if err != nil {
		setRun(a, key, hookRun{Phase: "failed", At: now.Unix(), Attempt: r.Attempt + 1, Msg: err.Error()})
		return true, nil
	}
	setRun(a, key, hookRun{Phase: "running", Job: job, At: now.Unix(), Attempt: r.Attempt})
	return true, nil
}

// runHooks runs install actions once the app's components are up, then setup checks.
// Returns true if the app changed.
func (s *Server) runHooks(ctx context.Context, a *apps.App) bool {
	if a.TemplateDef == nil || a.Paused {
		return false
	}
	st := s.status(ctx, a)
	if st.State != "running" && st.State != "configuring" {
		return false
	}
	if act := pendingInstall(a); act != nil {
		changed, _ := s.progressRun(ctx, a, installKey(act.Name), act, 0)
		return changed
	}
	changed := false
	for _, step := range a.TemplateDef.Setup {
		if a.Hooks[setupKey(step.Name)+"/done"] == "true" {
			continue
		}
		act := a.TemplateDef.Action(step.DoneWhen)
		if act == nil {
			continue
		}
		c, out := s.progressRun(ctx, a, "check/"+step.Name, act, 2*time.Minute)
		changed = changed || c
		if done, _ := out["done"].(bool); done {
			a.Hooks[setupKey(step.Name)+"/done"] = "true"
			s.logf("%s: setup step %s is done", a.Name, step.Name)
		}
	}
	return changed
}

// SetupStatus is one setup step as sb status and the dashboard show it.
type SetupStatus struct {
	Name        string `json:"name"`
	Description string `json:"description"`
	Link        string `json:"link,omitempty"` // passes the login wall first
	Done        bool   `json:"done"`
}

func (s *Server) setupStatus(a *apps.App, vals map[string]string) []SetupStatus {
	var out []SetupStatus
	var steps []apps.SetupStep
	if a.TemplateDef != nil {
		steps = a.TemplateDef.Setup
	}
	for _, step := range steps {
		st := SetupStatus{Name: step.Name, Description: step.Description, Done: a.Hooks[setupKey(step.Name)+"/done"] == "true"}
		if step.Link != "" {
			st.Link = s.enterURL(a.Name, step.Link)
		}
		out = append(out, st)
	}
	for _, p := range a.Provide {
		if p.Required {
			desc := "Enter " + p.Name + " on the app page in the dashboard"
			if p.Description != "" {
				desc += " (" + p.Description + ")"
			}
			out = append(out, SetupStatus{Name: "secret_" + strings.ToLower(p.Name), Description: desc,
				Link: "https://" + s.hosts().Auth() + "/apps/" + a.Name, Done: vals[p.Name] != ""})
		}
	}
	return out
}

// enterURL is a link that passes the app's login wall, then opens path.
func (s *Server) enterURL(app, path string) string {
	return "https://" + s.hosts().App(app) + apps.EnterPath + "?to=" + url.QueryEscape(path)
}

var noiseLine = regexp.MustCompile(`^(Node\.js v\d|at |\^+$|[{}\]),]*$|node:internal|triggerUncaughtException)`)

// errorLine picks the most telling line of a failed script's output: the last line that
// names an error, or else the last line that is not a stack frame or runtime noise.
func errorLine(lines []string) string {
	tail := lines
	if len(tail) > 40 {
		tail = tail[len(tail)-40:]
	}
	var errs []string
	last := ""
	for _, l := range tail {
		l = strings.TrimSpace(l)
		if l == "" || noiseLine.MatchString(l) {
			continue
		}
		last = l
		if strings.Contains(l, "Error") || strings.Contains(l, "error") {
			errs = append(errs, l)
		}
	}
	if len(errs) > 0 {
		// "[TypeError: fetch failed]" followed by "[cause]: Error: connect ECONNREFUSED": keep both.
		if len(errs) > 1 {
			return errs[len(errs)-2] + " " + errs[len(errs)-1]
		}
		return errs[0]
	}
	if last == "" {
		return "action failed"
	}
	return last
}
