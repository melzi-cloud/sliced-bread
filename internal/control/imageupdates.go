package control

import (
	"context"
	"encoding/json"
	"strconv"
	"strings"
	"time"

	"github.com/google/go-containerregistry/pkg/name"
	"github.com/google/go-containerregistry/pkg/v1/remote"

	"github.com/melzi-cloud/sliced-bread/internal/apps"
	"github.com/melzi-cloud/sliced-bread/internal/kube"
)

// Once a day the leader looks up newer upstream versions of every template app's image
// (same tag style, higher version) and records them. They are reported, not applied:
// applying with automatic rollback is `sb update` (later).

const (
	imageUpdatesConfigMap = "sb-image-updates"
	imageCheckEvery       = 24 * time.Hour
)

// ImageUpdate is one app whose image has a newer upstream version.
type ImageUpdate struct {
	App       string `json:"app"`
	Image     string `json:"image"`
	Current   string `json:"current"`
	Latest    string `json:"latest"`
	CheckedAt string `json:"checkedAt"`
	Error     string `json:"error,omitempty"`
}

// parseVersion reads "1.37.5" or "v3.4.1" into numbers; ok is false for tags such as
// "latest", "1.37.5-alpine" or dates.
func parseVersion(tag string) ([]int, bool) {
	t := strings.TrimPrefix(tag, "v")
	parts := strings.Split(t, ".")
	if len(parts) < 2 || len(parts) > 4 {
		return nil, false
	}
	out := make([]int, len(parts))
	for i, p := range parts {
		n, err := strconv.Atoi(p)
		if err != nil || n < 0 || (len(p) > 1 && p[0] == '0') {
			return nil, false
		}
		out[i] = n
	}
	return out, true
}

func newer(a, b []int) bool {
	for i := 0; i < len(a) && i < len(b); i++ {
		if a[i] != b[i] {
			return a[i] > b[i]
		}
	}
	return len(a) > len(b)
}

// latestVersion picks the highest tag with the same shape (number of parts, v prefix) as
// current that is newer than it. "" if none.
func latestVersion(current string, tags []string) string {
	cv, ok := parseVersion(current)
	if !ok {
		return ""
	}
	best, bestV := "", cv
	for _, t := range tags {
		if strings.HasPrefix(t, "v") != strings.HasPrefix(current, "v") {
			continue
		}
		v, ok := parseVersion(t)
		if !ok || len(v) != len(cv) || v[0] > cv[0]+1 { // skip implausible jumps (date-like tags)
			continue
		}
		if newer(v, bestV) {
			best, bestV = t, v
		}
	}
	return best
}

func (s *Server) maybeCheckImages(ctx context.Context, list []*apps.App) {
	if s.imageCheckAt.IsZero() {
		var cm kube.Obj
		if s.kube.Get(ctx, "v1", "ConfigMap", s.cfg.Namespace, imageUpdatesConfigMap, &cm) == nil {
			s.imageCheckAt, _ = time.Parse(time.RFC3339, kube.Str(cm, "metadata", "annotations", "slicedbread.dev/checked-at"))
		}
	}
	if time.Since(s.imageCheckAt) < imageCheckEvery {
		return
	}
	s.imageCheckAt = time.Now()
	var out []ImageUpdate
	seen := map[string][]string{}
	for _, a := range list {
		if a.Template == "" || a.Image == "" || strings.Contains(a.Image, "@") {
			continue
		}
		repo, tag, ok := strings.Cut(a.Image, ":")
		if !ok || strings.Contains(tag, "/") {
			continue
		}
		u := ImageUpdate{App: a.Name, Image: repo, Current: tag, CheckedAt: now()}
		tags, cached := seen[repo]
		if !cached {
			r, err := name.NewRepository(repo)
			if err == nil {
				cctx, cancel := context.WithTimeout(ctx, 30*time.Second)
				tags, err = remote.List(r, remote.WithContext(cctx))
				cancel()
			}
			if err != nil {
				u.Error = err.Error()
				out = append(out, u)
				continue
			}
			seen[repo] = tags
		}
		if l := latestVersion(tag, tags); l != "" {
			u.Latest = l
			out = append(out, u)
		}
	}
	b, _ := json.Marshal(out)
	if err := s.kube.Apply(ctx, kube.Obj{"apiVersion": "v1", "kind": "ConfigMap",
		"metadata": kube.Obj{"name": imageUpdatesConfigMap, "namespace": s.cfg.Namespace, "labels": kube.Obj{apps.ManagedByLabel: "slicedbread"},
			"annotations": kube.Obj{"slicedbread.dev/checked-at": now()}},
		"data": kube.Obj{"updates.json": string(b)}}); err != nil {
		s.logf("image updates: %v", err)
	}
}

// imageUpdates returns the last check's results by app.
func (s *Server) imageUpdates(ctx context.Context) map[string]ImageUpdate {
	out := map[string]ImageUpdate{}
	var cm kube.Obj
	if s.kube == nil || s.kube.Get(ctx, "v1", "ConfigMap", s.cfg.Namespace, imageUpdatesConfigMap, &cm) != nil {
		return out
	}
	var list []ImageUpdate
	json.Unmarshal([]byte(kube.Str(cm, "data", "updates.json")), &list)
	for _, u := range list {
		out[u.App] = u
	}
	return out
}
