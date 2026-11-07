package control

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"strconv"
	"time"

	"github.com/melzi-cloud/sliced-bread/internal/apps"
	"github.com/melzi-cloud/sliced-bread/internal/catalog"
	"github.com/melzi-cloud/sliced-bread/internal/kube"
)

// The template catalog (D-035): sb-control starts with the signed snapshot built into it
// and replaces it with a newer signed catalog from bakery, fetched on `sb catalog
// --refresh` or once a day by the lease holder. The newest verified catalog is kept in the
// ConfigMap sb-catalog so every replica (and a restarted one) uses the same.

const (
	catalogConfigMap  = "sb-catalog"
	catalogCheckEvery = 24 * time.Hour
)

func catalogURL() string {
	if u := os.Getenv("SB_CATALOG_URL"); u != "" {
		return u
	}
	return catalog.DefaultURL
}

// CheckEmbeddedCatalog verifies the snapshot built into this binary; sb-control does not
// start without a valid one.
func CheckEmbeddedCatalog() error {
	if apps.SnapshotErr != nil {
		return apps.SnapshotErr
	}
	data, sig := apps.Snapshot()
	if err := catalog.Verify(data, sig, nil); err != nil {
		return fmt.Errorf("embedded template catalog: %w", err)
	}
	return nil
}

// CatalogResult is what a refresh reports.
type CatalogResult struct {
	Serial    int64    `json:"serial"`
	Previous  int64    `json:"previous"`
	Built     string   `json:"built"`
	Source    string   `json:"source,omitempty"`
	Changed   bool     `json:"changed"`
	Templates []string `json:"templates"` // name version
	Skipped   []string `json:"skipped,omitempty"`
}

// installCatalog checks a catalog and makes it the one in use if it is newer.
func installCatalog(data, sig []byte, from string) (*CatalogResult, error) {
	if err := catalog.Verify(data, sig, nil); err != nil {
		return nil, err
	}
	b, tmpls, skipped, err := apps.LoadBundle(data)
	if err != nil {
		return nil, err
	}
	cur := apps.CurrentCatalog()
	res := &CatalogResult{Serial: b.Serial, Previous: cur.Serial, Built: b.Built, Source: b.Source, Skipped: skipped}
	for _, e := range b.Templates {
		res.Templates = append(res.Templates, e.Name+" "+e.Version)
	}
	switch {
	case b.Serial < cur.Serial:
		return res, fmt.Errorf("refusing catalog serial %d: older than the one in use (%d)", b.Serial, cur.Serial)
	case b.Serial == cur.Serial:
		return res, nil
	}
	if len(tmpls) == 0 {
		return res, errors.New("refusing a catalog with no usable templates")
	}
	apps.SetCatalog(apps.CatalogInfo{Serial: b.Serial, Built: b.Built, Source: b.Source, From: from, Skipped: skipped}, tmpls)
	res.Changed = true
	return res, nil
}

// loadStoredCatalog picks up a newer catalog another replica (or an earlier run) stored.
func (s *Server) loadStoredCatalog(ctx context.Context) {
	var cm kube.Obj
	if err := s.kube.Get(ctx, "v1", "ConfigMap", s.cfg.Namespace, catalogConfigMap, &cm); err != nil {
		return
	}
	serial, _ := strconv.ParseInt(kube.Str(cm, "metadata", "annotations", "slicedbread.dev/serial"), 10, 64)
	if serial <= apps.CurrentCatalog().Serial {
		return
	}
	res, err := installCatalog([]byte(kube.Str(cm, "data", "catalog.json")), []byte(kube.Str(cm, "data", "catalog.json.minisig")), "refreshed")
	if err != nil {
		s.logf("stored template catalog not used: %v", err)
		return
	}
	if res.Changed {
		s.logf("template catalog %d in use (%v)", res.Serial, res.Templates)
	}
}

// refreshCatalog fetches the latest published catalog and installs it if newer.
func (s *Server) refreshCatalog(ctx context.Context) (*CatalogResult, error) {
	data, sig, err := catalog.Fetch(ctx, catalogURL())
	if err != nil {
		return nil, err
	}
	res, err := installCatalog(data, sig, "refreshed")
	if err != nil {
		return res, err
	}
	now := time.Now().UTC().Format(time.RFC3339)
	cm := kube.Obj{"apiVersion": "v1", "kind": "ConfigMap",
		"metadata": kube.Obj{"name": catalogConfigMap, "namespace": s.cfg.Namespace,
			"annotations": kube.Obj{"slicedbread.dev/serial": strconv.FormatInt(apps.CurrentCatalog().Serial, 10), "slicedbread.dev/checked-at": now}},
		"data": kube.Obj{}}
	if res.Changed {
		cm["data"] = kube.Obj{"catalog.json": string(data), "catalog.json.minisig": string(sig)}
		s.logf("template catalog refreshed: serial %d -> %d (%v)", res.Previous, res.Serial, res.Templates)
	} else {
		// Keep what is stored; only note the check.
		var old kube.Obj
		if s.kube.Get(ctx, "v1", "ConfigMap", s.cfg.Namespace, catalogConfigMap, &old) == nil {
			cm["data"] = old["data"]
		}
	}
	if err := s.kube.Apply(ctx, cm); err != nil {
		return res, fmt.Errorf("store catalog: %w", err)
	}
	s.catalogCheckAt = time.Now()
	return res, nil
}

// maybeRefreshCatalog runs the daily check on the lease holder.
func (s *Server) maybeRefreshCatalog(ctx context.Context) {
	if s.catalogCheckAt.IsZero() {
		var cm kube.Obj
		if s.kube.Get(ctx, "v1", "ConfigMap", s.cfg.Namespace, catalogConfigMap, &cm) == nil {
			s.catalogCheckAt, _ = time.Parse(time.RFC3339, kube.Str(cm, "metadata", "annotations", "slicedbread.dev/checked-at"))
		}
	}
	if time.Since(s.catalogCheckAt) < catalogCheckEvery {
		return
	}
	s.catalogCheckAt = time.Now() // also after a failure: try again tomorrow, or on request
	if _, err := s.refreshCatalog(ctx); err != nil {
		s.logf("daily template catalog check: %v", err)
	}
}

func (s *Server) apiCatalogRefresh(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 90*time.Second)
	defer cancel()
	res, err := s.refreshCatalog(ctx)
	if err != nil {
		fail(w, 502, "catalog refresh failed: "+err.Error(), "the catalog in use is unchanged (see: sb catalog)")
		return
	}
	writeJSON(w, 200, res)
}
