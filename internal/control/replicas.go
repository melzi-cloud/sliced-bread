package control

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/melzi-cloud/sliced-bread/internal/apps"
	"github.com/melzi-cloud/sliced-bread/internal/kube"
)

// sb-control runs as two replicas on 3-node installs, so a server reboot or loss does not
// take the login wall down with it. Everything a request may need from another replica
// lives in the cluster: auth state in the sb-auth Secret (read-modify-write with
// resourceVersion), one-time exchange tokens as small ConfigMaps (create fails if used),
// CLI login grants as Secrets. Background work runs only on the replica holding the
// sb-control Lease.

// --- auth state ---

// authFromSecret parses the sb-auth Secret.
func authFromSecret(sec kube.Obj) (authState, string, error) {
	var st authState
	raw, err := base64.StdEncoding.DecodeString(kube.Str(sec, "data", "state.json"))
	if err != nil {
		return st, "", err
	}
	err = json.Unmarshal(raw, &st)
	return st, kube.Str(sec, "metadata", "resourceVersion"), err
}

func authSecretObj(ns string, st authState, rv string) (kube.Obj, error) {
	b, err := json.Marshal(st)
	if err != nil {
		return nil, err
	}
	md := kube.Obj{"name": authSecret, "namespace": ns}
	if rv != "" {
		md["resourceVersion"] = rv
	}
	return kube.Obj{"apiVersion": "v1", "kind": "Secret", "metadata": md, "type": "Opaque",
		"data": kube.Obj{"state.json": base64.StdEncoding.EncodeToString(b)}}, nil
}

// refresh reloads the auth state if the Secret changed. Cheap when it did not.
func (a *Auth) refresh(ctx context.Context) error {
	if a.kube == nil {
		return nil
	}
	var sec kube.Obj
	if err := a.kube.Get(ctx, "v1", "Secret", a.ns, authSecret, &sec); err != nil {
		return err
	}
	a.mu.RLock()
	same := kube.Str(sec, "metadata", "resourceVersion") == a.rv
	a.mu.RUnlock()
	if same {
		return nil
	}
	st, rv, err := authFromSecret(sec)
	if err != nil {
		return err
	}
	a.mu.Lock()
	a.st, a.rv, a.refreshedAt = st, rv, time.Now()
	a.mu.Unlock()
	return nil
}

// refreshSoon reloads the state at most once a second; used when a token or user is not
// found, in case another replica just added it.
func (a *Auth) refreshSoon() {
	a.mu.RLock()
	recent := time.Since(a.refreshedAt) < time.Second
	a.mu.RUnlock()
	if recent || a.kube == nil {
		return
	}
	a.mu.Lock()
	a.refreshedAt = time.Now()
	a.mu.Unlock()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	a.refresh(ctx)
}

// mutate applies a change to the latest stored state and writes it back, retrying when
// another replica wrote in between.
func (a *Auth) mutate(ctx context.Context, f func(st *authState) error) error {
	if a.kube == nil {
		a.mu.Lock()
		defer a.mu.Unlock()
		return f(&a.st)
	}
	for attempt := 0; attempt < 5; attempt++ {
		var sec kube.Obj
		if err := a.kube.Get(ctx, "v1", "Secret", a.ns, authSecret, &sec); err != nil {
			return err
		}
		st, rv, err := authFromSecret(sec)
		if err != nil {
			return err
		}
		if err := f(&st); err != nil {
			return err
		}
		obj, err := authSecretObj(a.ns, st, rv)
		if err != nil {
			return err
		}
		err = a.kube.Update(ctx, obj)
		if kube.IsConflict(err) {
			continue
		}
		if err != nil {
			return err
		}
		a.mu.Lock()
		a.st, a.rv = st, "" // the next refresh picks up the new resourceVersion
		a.mu.Unlock()
		return nil
	}
	return errors.New("auth state changed concurrently too often; try again")
}

// --- one-time exchange tokens ---

// markUsed records an exchange token's nonce; false if it was already redeemed on any
// replica.
func (a *Auth) markUsed(ctx context.Context, nonce string, exp int64) (bool, error) {
	if a.kube == nil {
		a.mu.Lock()
		defer a.mu.Unlock()
		if a.used == nil {
			a.used = map[string]int64{}
		}
		if _, seen := a.used[nonce]; seen {
			return false, nil
		}
		a.used[nonce] = exp
		return true, nil
	}
	err := a.kube.Create(ctx, kube.Obj{"apiVersion": "v1", "kind": "ConfigMap",
		"metadata": kube.Obj{"name": "sb-xchg-" + sha(nonce)[:24], "namespace": a.ns,
			"labels":      kube.Obj{"slicedbread.dev/kind": "exchange-nonce", apps.ManagedByLabel: "slicedbread"},
			"annotations": kube.Obj{"slicedbread.dev/expires": strconv.FormatInt(exp, 10)}}})
	if kube.IsConflict(err) {
		return false, nil
	}
	return err == nil, err
}

// --- leader ---

const leaseName = "sb-control"

func podName() string {
	if n := os.Getenv("HOSTNAME"); n != "" {
		return n
	}
	h, _ := os.Hostname()
	return h
}

// leader reports whether this replica holds the lease, taking or renewing it. A lease
// not renewed for 15 seconds can be taken over.
func (s *Server) leader(ctx context.Context) bool {
	if s.kube == nil {
		return true
	}
	me := podName()
	now := time.Now().UTC()
	stamp := now.Format("2006-01-02T15:04:05.000000Z")
	var l kube.Obj
	err := s.kube.Get(ctx, "coordination.k8s.io/v1", "Lease", s.cfg.Namespace, leaseName, &l)
	if kube.IsNotFound(err) {
		err = s.kube.Create(ctx, kube.Obj{"apiVersion": "coordination.k8s.io/v1", "kind": "Lease",
			"metadata": kube.Obj{"name": leaseName, "namespace": s.cfg.Namespace},
			"spec":     kube.Obj{"holderIdentity": me, "leaseDurationSeconds": 15, "renewTime": stamp, "acquireTime": stamp}})
		return s.becomes(err == nil)
	}
	if err != nil {
		return s.becomes(false)
	}
	spec, _ := l["spec"].(map[string]any)
	holder, _ := spec["holderIdentity"].(string)
	renew, _ := time.Parse(time.RFC3339Nano, kube.Str(l, "spec", "renewTime"))
	if holder != me && now.Sub(renew) < 15*time.Second {
		return s.becomes(false)
	}
	spec["holderIdentity"], spec["renewTime"], spec["leaseDurationSeconds"] = me, stamp, 15
	if holder != me {
		spec["acquireTime"] = stamp
	}
	return s.becomes(s.kube.Update(ctx, l) == nil)
}

func (s *Server) becomes(lead bool) bool {
	if lead != s.isLeader {
		if lead {
			s.logf("this replica (%s) now runs background work", podName())
		}
		s.isLeader = lead
	}
	return lead
}

// gcShared removes expired exchange nonces and device grants. Leader only.
func (s *Server) gcShared(ctx context.Context) {
	now := time.Now().Unix()
	if list, err := s.kube.List(ctx, "v1", "ConfigMap", s.cfg.Namespace, "slicedbread.dev/kind=exchange-nonce"); err == nil {
		for _, o := range list {
			if exp, _ := strconv.ParseInt(kube.Str(o, "metadata", "annotations", "slicedbread.dev/expires"), 10, 64); exp < now {
				s.kube.Delete(ctx, "v1", "ConfigMap", s.cfg.Namespace, kube.Str(o, "metadata", "name"))
			}
		}
	}
	if list, err := s.kube.List(ctx, "v1", "Secret", s.cfg.Namespace, "slicedbread.dev/kind=device-grant"); err == nil {
		for _, o := range list {
			if g := grantFromSecret(o); g == nil || time.Now().After(g.Expires.Add(time.Minute)) {
				s.kube.Delete(ctx, "v1", "Secret", s.cfg.Namespace, kube.Str(o, "metadata", "name"))
			}
		}
	}
}

// --- device grants ---

func grantSecretName(poll string) string { return "sb-device-" + sha(poll)[:24] }

func grantFromSecret(o kube.Obj) *deviceGrant {
	raw, err := base64.StdEncoding.DecodeString(kube.Str(o, "data", "grant.json"))
	if err != nil {
		return nil
	}
	var g deviceGrant
	if json.Unmarshal(raw, &g) != nil {
		return nil
	}
	return &g
}

func (s *Server) saveGrant(ctx context.Context, g *deviceGrant) error {
	if s.kube == nil {
		s.devices.mu.Lock()
		s.devices.grants[g.Poll] = g
		s.devices.mu.Unlock()
		return nil
	}
	b, _ := json.Marshal(g)
	return s.kube.Apply(ctx, kube.Obj{"apiVersion": "v1", "kind": "Secret",
		"metadata": kube.Obj{"name": grantSecretName(g.Poll), "namespace": s.cfg.Namespace,
			"labels": kube.Obj{"slicedbread.dev/kind": "device-grant", "slicedbread.dev/code": strings.ReplaceAll(g.Code, "-", ""), apps.ManagedByLabel: "slicedbread"}},
		"type": "Opaque", "data": kube.Obj{"grant.json": base64.StdEncoding.EncodeToString(b)}})
}

func (s *Server) grantByPoll(ctx context.Context, poll string) *deviceGrant {
	if s.kube == nil {
		s.devices.mu.Lock()
		defer s.devices.mu.Unlock()
		return s.devices.grants[poll]
	}
	var o kube.Obj
	if err := s.kube.Get(ctx, "v1", "Secret", s.cfg.Namespace, grantSecretName(poll), &o); err != nil {
		return nil
	}
	return grantFromSecret(o)
}

func (s *Server) grantByCode(ctx context.Context, code string) *deviceGrant {
	if s.kube == nil {
		s.devices.mu.Lock()
		defer s.devices.mu.Unlock()
		return s.devices.byCode(code)
	}
	clean := strings.ReplaceAll(strings.ToUpper(strings.TrimSpace(code)), "-", "")
	if clean == "" || strings.ContainsAny(clean, ",=!() ") {
		return nil
	}
	list, err := s.kube.List(ctx, "v1", "Secret", s.cfg.Namespace, "slicedbread.dev/code="+clean)
	if err != nil {
		return nil
	}
	for _, o := range list {
		if g := grantFromSecret(o); g != nil && g.Code == strings.ToUpper(strings.TrimSpace(code)) && time.Now().Before(g.Expires) {
			return g
		}
	}
	return nil
}

func (s *Server) deleteGrant(ctx context.Context, poll string) {
	if s.kube == nil {
		s.devices.mu.Lock()
		delete(s.devices.grants, poll)
		s.devices.mu.Unlock()
		return
	}
	s.kube.Delete(ctx, "v1", "Secret", s.cfg.Namespace, grantSecretName(poll))
}

func (s *Server) countGrants(ctx context.Context) int {
	if s.kube == nil {
		s.devices.mu.Lock()
		defer s.devices.mu.Unlock()
		s.devices.gc()
		return len(s.devices.grants)
	}
	list, _ := s.kube.List(ctx, "v1", "Secret", s.cfg.Namespace, "slicedbread.dev/kind=device-grant")
	return len(list)
}
