package apps

import (
	"crypto/sha256"
	"embed"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"path"
	"sort"
	"strings"
	"sync"
	"time"
)

// The template catalog is one signed bundle built from github.com/melzi-cloud/bakery
// (task #16, D-035): every template's files, its version, images and tier. sb-control
// embeds a snapshot so it works offline and replaces it with a newer signed bundle on
// `sb catalog --refresh` or the daily check. The signature is checked by internal/catalog;
// this file only builds and parses bundles.

// BundleFormat is the bundle layout version this sb-control reads.
const BundleFormat = 1

// TierVerified is the only tier listed at launch. Community templates come later.
const TierVerified = "verified"

// Bundle is the catalog as published.
type Bundle struct {
	Format    int           `json:"format"`
	Serial    int64         `json:"serial"` // increases with every publish; older bundles are refused
	Built     string        `json:"built"`
	Source    string        `json:"source,omitempty"` // repository and commit it was built from
	Templates []BundleEntry `json:"templates"`
}

// BundleEntry is one template in a bundle.
type BundleEntry struct {
	Name    string            `json:"name"`
	Version string            `json:"version"`
	Tier    string            `json:"tier"`
	Hash    string            `json:"hash"` // sha256 over all files, see filesHash
	Images  []string          `json:"images"`
	Files   map[string]string `json:"files"` // template.yaml and every other file, by relative path
}

// CatalogInfo describes the catalog in use.
type CatalogInfo struct {
	Serial  int64    `json:"serial"`
	Built   string   `json:"built"`
	Source  string   `json:"source,omitempty"`
	From    string   `json:"from"`              // "embedded" or "refreshed"
	Skipped []string `json:"skipped,omitempty"` // templates this sb-control cannot use, with why
}

func filesHash(files map[string]string) string {
	keys := make([]string, 0, len(files))
	for k := range files {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	h := sha256.New()
	for _, k := range keys {
		fmt.Fprintf(h, "%s\x00%d\x00%s", k, len(files[k]), files[k])
	}
	return hex.EncodeToString(h.Sum(nil))
}

// BuildBundle builds a bundle from a directory holding one directory per template.
// Every template must pass Validate; all problems are reported together.
func BuildBundle(fsys fs.FS, serial int64, built time.Time, source string) (*Bundle, error) {
	b := &Bundle{Format: BundleFormat, Serial: serial, Built: built.UTC().Format(time.RFC3339), Source: source}
	ents, err := fs.ReadDir(fsys, ".")
	if err != nil {
		return nil, err
	}
	var errs []error
	for _, e := range ents {
		if !e.IsDir() || strings.HasPrefix(e.Name(), ".") {
			continue
		}
		if _, err := fs.Stat(fsys, path.Join(e.Name(), "template.yaml")); err != nil {
			continue // not a template (docs, tooling)
		}
		t, err := LoadTemplate(fsys, e.Name())
		if err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", e.Name(), err))
			continue
		}
		if t.Name != e.Name() {
			errs = append(errs, fmt.Errorf("directory %s holds template %s; they must match", e.Name(), t.Name))
			continue
		}
		raw, _ := fs.ReadFile(fsys, path.Join(e.Name(), "template.yaml"))
		files := map[string]string{"template.yaml": string(raw)}
		for k, v := range t.Files {
			files[k] = v
		}
		b.Templates = append(b.Templates, BundleEntry{Name: t.Name, Version: t.Version, Tier: TierVerified,
			Hash: filesHash(files), Images: templateImages(t), Files: files})
	}
	if len(errs) > 0 {
		return nil, errors.Join(errs...)
	}
	if len(b.Templates) == 0 {
		return nil, errors.New("no templates found")
	}
	sort.Slice(b.Templates, func(i, j int) bool { return b.Templates[i].Name < b.Templates[j].Name })
	return b, nil
}

func templateImages(t *Template) []string {
	seen := map[string]bool{}
	var out []string
	add := func(s string) {
		if !seen[s] {
			seen[s] = true
			out = append(out, s)
		}
	}
	for _, c := range t.Components {
		add(c.Image)
	}
	for _, a := range t.Actions {
		add(a.Image)
	}
	return out
}

// LoadBundle parses a bundle (whose signature the caller has checked) into templates.
// Templates this sb-control cannot read (a newer format, a tier not offered yet) are
// skipped with a reason rather than failing the whole catalog.
func LoadBundle(data []byte) (*Bundle, map[string]*Template, []string, error) {
	var b Bundle
	if err := json.Unmarshal(data, &b); err != nil {
		return nil, nil, nil, fmt.Errorf("catalog is not valid JSON: %w", err)
	}
	if b.Format != BundleFormat {
		return nil, nil, nil, fmt.Errorf("catalog format %d is not supported by this sb-control (format %d); upgrade SlicedBread", b.Format, BundleFormat)
	}
	if b.Serial <= 0 {
		return nil, nil, nil, errors.New("catalog has no serial")
	}
	out := map[string]*Template{}
	var skipped []string
	for _, e := range b.Templates {
		if e.Tier != TierVerified {
			skipped = append(skipped, e.Name+": tier "+e.Tier+" is not offered")
			continue
		}
		if filesHash(e.Files) != e.Hash {
			return nil, nil, nil, fmt.Errorf("template %s: files do not match their hash", e.Name)
		}
		raw, ok := e.Files["template.yaml"]
		if !ok {
			return nil, nil, nil, fmt.Errorf("template %s has no template.yaml", e.Name)
		}
		files := map[string]string{}
		for k, v := range e.Files {
			if k != "template.yaml" {
				files[k] = v
			}
		}
		t, err := ParseTemplate([]byte(raw), files)
		if err != nil {
			skipped = append(skipped, fmt.Sprintf("%s %s: %v (it may need a newer SlicedBread)", e.Name, e.Version, err))
			continue
		}
		if t.Name != e.Name || t.Version != e.Version {
			return nil, nil, nil, fmt.Errorf("template %s: entry says %s %s", t.Name, e.Name, e.Version)
		}
		t.Trusted = true
		out[t.Name] = t
	}
	return &b, out, skipped, nil
}

// --- the catalog in use ---

// Snapshot is the catalog built into this binary: internal/apps/catalog/catalog.json and
// its minisign signature, refreshed from the latest bakery release with
// `make catalog-snapshot`.
//
//go:embed catalog/catalog.json catalog/catalog.json.minisig
var snapshotFS embed.FS

// Snapshot returns the embedded catalog and its signature.
func Snapshot() (data, sig []byte) {
	data, _ = snapshotFS.ReadFile("catalog/catalog.json")
	sig, _ = snapshotFS.ReadFile("catalog/catalog.json.minisig")
	return data, sig
}

var (
	catMu   sync.RWMutex
	catalog map[string]*Template
	catInfo CatalogInfo
)

// SnapshotErr is set when the embedded catalog cannot be read; sb-control refuses to
// start with it (the signature is checked there, in internal/catalog).
var SnapshotErr error

func init() {
	data, _ := Snapshot()
	b, tmpls, skipped, err := LoadBundle(data)
	if err != nil {
		SnapshotErr = fmt.Errorf("embedded template catalog: %w", err)
		catalog = map[string]*Template{}
		return
	}
	catalog, catInfo = tmpls, CatalogInfo{Serial: b.Serial, Built: b.Built, Source: b.Source, From: "embedded", Skipped: skipped}
}

// SetCatalog replaces the catalog in use (after its signature was checked).
func SetCatalog(info CatalogInfo, tmpls map[string]*Template) {
	catMu.Lock()
	defer catMu.Unlock()
	catalog, catInfo = tmpls, info
}

// CurrentCatalog describes the catalog in use.
func CurrentCatalog() CatalogInfo {
	catMu.RLock()
	defer catMu.RUnlock()
	return catInfo
}

// CatalogTemplate returns a catalog template by name, or nil.
func CatalogTemplate(name string) *Template {
	catMu.RLock()
	defer catMu.RUnlock()
	return catalog[name]
}

// TemplateNames returns sorted template names.
func TemplateNames() []string {
	catMu.RLock()
	defer catMu.RUnlock()
	var out []string
	for k := range catalog {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// TestFilesHash exposes the bundle file hash to tests in other packages.
func TestFilesHash(files map[string]string) string { return filesHash(files) }
