// Package lease is leader election with a coordination.k8s.io Lease. The holder renews
// in the background every few seconds, so long-running work does not lose it; another
// replica takes over when the lease has not been renewed for Duration.
package lease

import (
	"context"
	"os"
	"sync/atomic"
	"time"

	"github.com/melzi-cloud/sliced-bread/internal/kube"
)

const (
	Duration = 15 * time.Second
	renew    = 4 * time.Second
)

// Elector holds or waits for one Lease.
type Elector struct {
	Kube      *kube.Client
	Namespace string
	Name      string
	ID        string // this replica; defaults to the pod name
	OnChange  func(leading bool)
	leading   atomic.Bool
	// validUntil: unix nanos until which our last successful renewal still holds.
	validUntil atomic.Int64
}

// Leading reports whether this replica holds the lease.
func (e *Elector) Leading() bool { return e.leading.Load() }

// Run renews or acquires the lease until ctx ends.
func (e *Elector) Run(ctx context.Context) {
	if e.ID == "" {
		e.ID, _ = os.Hostname()
	}
	for {
		e.set(e.try(ctx))
		select {
		case <-ctx.Done():
			e.set(false)
			return
		case <-time.After(renew):
		}
	}
}

func (e *Elector) set(v bool) {
	if e.leading.Swap(v) != v && e.OnChange != nil {
		e.OnChange(v)
	}
}

func (e *Elector) try(ctx context.Context) bool {
	ctx, cancel := context.WithTimeout(ctx, renew)
	defer cancel()
	now := time.Now().UTC()
	stamp := now.Format("2006-01-02T15:04:05.000000Z")
	var l kube.Obj
	err := e.Kube.Get(ctx, "coordination.k8s.io/v1", "Lease", e.Namespace, e.Name, &l)
	if kube.IsNotFound(err) {
		return e.Kube.Create(ctx, kube.Obj{"apiVersion": "coordination.k8s.io/v1", "kind": "Lease",
			"metadata": kube.Obj{"name": e.Name, "namespace": e.Namespace},
			"spec":     kube.Obj{"holderIdentity": e.ID, "leaseDurationSeconds": int(Duration.Seconds()), "renewTime": stamp, "acquireTime": stamp}}) == nil
	}
	if err != nil {
		// Cannot reach the API: keep leading only while our last renewal is still valid.
		return e.Leading() && e.validUntil.Load() > now.UnixNano()
	}
	spec, _ := l["spec"].(map[string]any)
	holder, _ := spec["holderIdentity"].(string)
	renewed, _ := time.Parse(time.RFC3339Nano, kube.Str(l, "spec", "renewTime"))
	if holder != e.ID && now.Sub(renewed) < Duration {
		return false
	}
	spec["holderIdentity"], spec["renewTime"], spec["leaseDurationSeconds"] = e.ID, stamp, int(Duration.Seconds())
	if holder != e.ID {
		spec["acquireTime"] = stamp
	}
	if e.Kube.Update(ctx, l) != nil {
		return false
	}
	e.validUntil.Store(now.Add(Duration - renew).UnixNano())
	return true
}
