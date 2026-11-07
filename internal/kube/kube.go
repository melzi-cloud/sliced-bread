// Package kube is a minimal Kubernetes REST client for in-cluster use.
// It supports server-side apply, get, list, delete, merge patch and pod logs.
package kube

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"
)

// Obj is an unstructured Kubernetes object.
type Obj = map[string]any

const saDir = "/var/run/secrets/kubernetes.io/serviceaccount"

// Client talks to the API server.
type Client struct {
	Base      string
	HTTP      *http.Client
	TokenFile string
	Manager   string // field manager for server-side apply
}

// InCluster builds a client from the pod's service account.
func InCluster() (*Client, error) {
	host, port := os.Getenv("KUBERNETES_SERVICE_HOST"), os.Getenv("KUBERNETES_SERVICE_PORT")
	if host == "" {
		return nil, errors.New("not running in a cluster")
	}
	ca, err := os.ReadFile(saDir + "/ca.crt")
	if err != nil {
		return nil, err
	}
	pool := x509.NewCertPool()
	pool.AppendCertsFromPEM(ca)
	return &Client{
		Base:      "https://" + host + ":" + port,
		HTTP:      &http.Client{Timeout: 60 * time.Second, Transport: &http.Transport{TLSClientConfig: &tls.Config{RootCAs: pool}}},
		TokenFile: saDir + "/token",
		Manager:   "sb-control",
	}, nil
}

// StatusError is a non-2xx API response.
type StatusError struct {
	Code    int
	Message string
}

func (e *StatusError) Error() string { return fmt.Sprintf("kubernetes API %d: %s", e.Code, e.Message) }

// IsNotFound reports a 404.
func IsNotFound(err error) bool {
	var se *StatusError
	return errors.As(err, &se) && se.Code == http.StatusNotFound
}

// IsConflict reports a 409.
func IsConflict(err error) bool {
	var se *StatusError
	return errors.As(err, &se) && se.Code == http.StatusConflict
}

func (c *Client) do(ctx context.Context, method, path, contentType string, body []byte) ([]byte, error) {
	var b []byte
	var err error
	for attempt := 0; attempt < 4; attempt++ {
		b, err = c.once(ctx, method, path, contentType, body)
		var se *StatusError
		if err == nil || !(errors.As(err, &se) && (se.Code == http.StatusTooManyRequests || se.Code >= 500)) {
			return b, err
		}
		select {
		case <-ctx.Done():
			return nil, err
		case <-time.After(time.Duration(attempt+1) * time.Second):
		}
	}
	return b, err
}

func (c *Client) once(ctx context.Context, method, path, contentType string, body []byte) ([]byte, error) {
	var rdr io.Reader
	if body != nil {
		rdr = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.Base+path, rdr)
	if err != nil {
		return nil, err
	}
	if c.TokenFile != "" {
		tok, err := os.ReadFile(c.TokenFile) // re-read: projected tokens rotate
		if err != nil {
			return nil, err
		}
		req.Header.Set("Authorization", "Bearer "+strings.TrimSpace(string(tok)))
	}
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	req.Header.Set("Accept", "application/json")
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(io.LimitReader(resp.Body, 32<<20))
	if err != nil {
		return nil, err
	}
	if resp.StatusCode >= 300 {
		msg := strings.TrimSpace(string(b))
		var st struct {
			Message string `json:"message"`
		}
		if json.Unmarshal(b, &st) == nil && st.Message != "" {
			msg = st.Message
		}
		return nil, &StatusError{Code: resp.StatusCode, Message: msg}
	}
	return b, nil
}

type resource struct {
	group, version, plural string
	namespaced             bool
}

var resources = map[string]resource{
	"v1/Namespace":                                     {"", "v1", "namespaces", false},
	"v1/Secret":                                        {"", "v1", "secrets", true},
	"v1/ConfigMap":                                     {"", "v1", "configmaps", true},
	"v1/Service":                                       {"", "v1", "services", true},
	"v1/PersistentVolumeClaim":                         {"", "v1", "persistentvolumeclaims", true},
	"v1/Pod":                                           {"", "v1", "pods", true},
	"v1/Node":                                          {"", "v1", "nodes", false},
	"v1/Event":                                         {"", "v1", "events", true},
	"apps/v1/Deployment":                               {"apps", "v1", "deployments", true},
	"apps/v1/ReplicaSet":                               {"apps", "v1", "replicasets", true},
	"batch/v1/Job":                                     {"batch", "v1", "jobs", true},
	"networking.k8s.io/v1/Ingress":                     {"networking.k8s.io", "v1", "ingresses", true},
	"networking.k8s.io/v1/NetworkPolicy":               {"networking.k8s.io", "v1", "networkpolicies", true},
	"traefik.io/v1alpha1/Middleware":                   {"traefik.io", "v1alpha1", "middlewares", true},
	"postgresql.cnpg.io/v1/Cluster":                    {"postgresql.cnpg.io", "v1", "clusters", true},
	"cert-manager.io/v1/Certificate":                   {"cert-manager.io", "v1", "certificates", true},
	"cert-manager.io/v1/ClusterIssuer":                 {"cert-manager.io", "v1", "clusterissuers", false},
	"coordination.k8s.io/v1/Lease":                     {"coordination.k8s.io", "v1", "leases", true},
	"policy/v1/PodDisruptionBudget":                    {"policy", "v1", "poddisruptionbudgets", true},
	"slicedbread.dev/v1alpha1/App":                     {"slicedbread.dev", "v1alpha1", "apps", false},
	"slicedbread.dev/v1alpha1/Approval":                {"slicedbread.dev", "v1alpha1", "approvals", false},
	"apiextensions.k8s.io/v1/CustomResourceDefinition": {"apiextensions.k8s.io", "v1", "customresourcedefinitions", false},
}

// Path returns the REST path for a kind. name may be empty for collections.
func Path(apiVersion, kind, namespace, name string) (string, error) {
	r, ok := resources[apiVersion+"/"+kind]
	if !ok {
		return "", fmt.Errorf("unknown kind %s/%s", apiVersion, kind)
	}
	p := "/api/" + r.version
	if r.group != "" {
		p = "/apis/" + r.group + "/" + r.version
	}
	if r.namespaced && namespace != "" {
		p += "/namespaces/" + namespace
	}
	p += "/" + r.plural
	if name != "" {
		p += "/" + name
	}
	return p, nil
}

func objPath(o Obj) (string, error) {
	md, _ := o["metadata"].(Obj)
	if md == nil {
		if m2, ok := o["metadata"].(map[string]any); ok {
			md = m2
		}
	}
	name, _ := md["name"].(string)
	ns, _ := md["namespace"].(string)
	av, _ := o["apiVersion"].(string)
	kind, _ := o["kind"].(string)
	return Path(av, kind, ns, name)
}

// Apply server-side applies an object, taking ownership of conflicting fields.
func (c *Client) Apply(ctx context.Context, o Obj) error {
	p, err := objPath(o)
	if err != nil {
		return err
	}
	b, err := json.Marshal(o)
	if err != nil {
		return err
	}
	q := url.Values{"fieldManager": {c.Manager}, "force": {"true"}}
	_, err = c.do(ctx, http.MethodPatch, p+"?"+q.Encode(), "application/apply-patch+yaml", b)
	return err
}

// Create creates an object; it fails with a 409 (IsConflict) if the name is taken, which
// makes it usable as an atomic "first one wins" check across replicas.
func (c *Client) Create(ctx context.Context, o Obj) error {
	p, err := objPath(o)
	if err != nil {
		return err
	}
	p = p[:strings.LastIndex(p, "/")]
	b, err := json.Marshal(o)
	if err != nil {
		return err
	}
	_, err = c.do(ctx, http.MethodPost, p, "application/json", b)
	return err
}

// Update replaces an object. With metadata.resourceVersion set it fails with a 409 if the
// object changed since it was read (optimistic concurrency).
func (c *Client) Update(ctx context.Context, o Obj) error {
	p, err := objPath(o)
	if err != nil {
		return err
	}
	b, err := json.Marshal(o)
	if err != nil {
		return err
	}
	_, err = c.do(ctx, http.MethodPut, p, "application/json", b)
	return err
}

// Get fetches one object into out.
func (c *Client) Get(ctx context.Context, apiVersion, kind, ns, name string, out any) error {
	p, err := Path(apiVersion, kind, ns, name)
	if err != nil {
		return err
	}
	b, err := c.do(ctx, http.MethodGet, p, "", nil)
	if err != nil {
		return err
	}
	return json.Unmarshal(b, out)
}

// List lists objects, optionally filtered by a label selector.
func (c *Client) List(ctx context.Context, apiVersion, kind, ns, selector string) ([]Obj, error) {
	p, err := Path(apiVersion, kind, ns, "")
	if err != nil {
		return nil, err
	}
	if selector != "" {
		p += "?labelSelector=" + url.QueryEscape(selector)
	}
	b, err := c.do(ctx, http.MethodGet, p, "", nil)
	if err != nil {
		return nil, err
	}
	var l struct {
		Items []Obj `json:"items"`
	}
	if err := json.Unmarshal(b, &l); err != nil {
		return nil, err
	}
	return l.Items, nil
}

// Delete deletes an object. Missing objects are not an error.
func (c *Client) Delete(ctx context.Context, apiVersion, kind, ns, name string) error {
	p, err := Path(apiVersion, kind, ns, name)
	if err != nil {
		return err
	}
	_, err = c.do(ctx, http.MethodDelete, p+"?propagationPolicy=Background", "application/json", []byte(`{}`))
	if IsNotFound(err) {
		return nil
	}
	return err
}

// MergePatch applies a JSON merge patch.
func (c *Client) MergePatch(ctx context.Context, apiVersion, kind, ns, name string, patch any) error {
	p, err := Path(apiVersion, kind, ns, name)
	if err != nil {
		return err
	}
	b, err := json.Marshal(patch)
	if err != nil {
		return err
	}
	_, err = c.do(ctx, http.MethodPatch, p, "application/merge-patch+json", b)
	return err
}

// Logs returns the tail of a pod container's logs.
func (c *Client) Logs(ctx context.Context, ns, pod, container string, tail int) (string, error) {
	p, _ := Path("v1", "Pod", ns, pod)
	q := url.Values{"tailLines": {fmt.Sprint(tail)}}
	if container != "" {
		q.Set("container", container)
	}
	b, err := c.do(ctx, http.MethodGet, p+"/log?"+q.Encode(), "", nil)
	return string(b), err
}

// Str digs a string out of an unstructured object, e.g. Str(o, "status", "phase").
func Str(o Obj, path ...string) string {
	var cur any = o
	for _, k := range path {
		m, ok := cur.(map[string]any)
		if !ok {
			return ""
		}
		cur = m[k]
	}
	s, _ := cur.(string)
	return s
}

// Num digs a number out of an unstructured object.
func Num(o Obj, path ...string) int64 {
	var cur any = o
	for _, k := range path {
		m, ok := cur.(map[string]any)
		if !ok {
			return 0
		}
		cur = m[k]
	}
	switch v := cur.(type) {
	case float64:
		return int64(v)
	case int64:
		return v
	case int:
		return int64(v)
	}
	return 0
}

// Slice digs a list out of an unstructured object.
func Slice(o Obj, path ...string) []any {
	var cur any = o
	for _, k := range path {
		m, ok := cur.(map[string]any)
		if !ok {
			return nil
		}
		cur = m[k]
	}
	s, _ := cur.([]any)
	return s
}
