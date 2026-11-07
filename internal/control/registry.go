package control

import (
	"context"
	"net"
	"sort"
	"strings"
	"time"

	"github.com/melzi-cloud/sliced-bread/internal/apps"
	"github.com/melzi-cloud/sliced-bread/internal/kube"
)

// The in-cluster image registry holds images built from apps' source. App pods must not
// reach it: a compromised app could overwrite another app's image or read its code.
// Only build pods (pushes) and the nodes' container runtime (pulls) are let in.

const (
	registryNamespace = "sb-system"
	registryPolicy    = "sb-registry-access"
	registryImage     = "registry:3.1.2"
	registryGCEvery   = 24 * time.Hour
)

// registryPolicyObj admits build pods and the given node sources to the registry.
// Pulls come from containerd on the nodes, not from a pod. Depending on which node the
// registry runs on, the source address is the node's IP, its cni0 bridge address
// (pod CIDR .1) or its flannel VXLAN address (pod CIDR .0), so all three are listed.
func registryPolicyObj(nodeSources []string) kube.Obj {
	from := []any{
		kube.Obj{"namespaceSelector": kube.Obj{"matchLabels": kube.Obj{"kubernetes.io/metadata.name": "sb-builds"}}},
		// sb-control keeps uploaded source in the registry (sb-uploads).
		kube.Obj{"podSelector": kube.Obj{"matchLabels": kube.Obj{"app": "sb-control"}}},
	}
	for _, src := range nodeSources {
		from = append(from, kube.Obj{"ipBlock": kube.Obj{"cidr": src + "/32"}})
	}
	return kube.Obj{
		"apiVersion": "networking.k8s.io/v1", "kind": "NetworkPolicy",
		"metadata": kube.Obj{"name": registryPolicy, "namespace": registryNamespace,
			"labels": kube.Obj{apps.ManagedByLabel: "slicedbread"}},
		"spec": kube.Obj{
			"podSelector": kube.Obj{"matchLabels": kube.Obj{"app": "sb-registry"}},
			"policyTypes": []any{"Ingress"},
			"ingress":     []any{kube.Obj{"from": from}},
		},
	}
}

// nodeSources lists every address a node's own processes may use to reach pods.
func nodeSources(nodes []kube.Obj) []string {
	seen := map[string]bool{}
	for _, n := range nodes {
		if addrs, ok := statusMap(n)["addresses"].([]any); ok {
			for _, x := range addrs {
				m, _ := x.(map[string]any)
				if t, _ := m["type"].(string); t == "InternalIP" || t == "ExternalIP" {
					if ip := net.ParseIP(kube.Str(m, "address")); ip != nil && ip.To4() != nil {
						seen[ip.String()] = true
					}
				}
			}
		}
		if _, cidr, err := net.ParseCIDR(kube.Str(n, "spec", "podCIDR")); err == nil && cidr.IP.To4() != nil {
			base := cidr.IP.To4()
			for _, last := range []byte{0, 1} {
				ip := net.IPv4(base[0], base[1], base[2], base[3]+last)
				seen[ip.String()] = true
			}
		}
	}
	out := make([]string, 0, len(seen))
	for k := range seen {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// ensureRegistryPolicy keeps the registry policy in step with the cluster's nodes.
func (s *Server) ensureRegistryPolicy(ctx context.Context) {
	nodes, err := s.kube.List(ctx, "v1", "Node", "", "")
	if err != nil || len(nodes) == 0 {
		if err != nil && time.Since(s.registryPolicyAt) > 10*time.Minute {
			s.logf("registry policy: list nodes: %v", err)
		}
		return
	}
	srcs := nodeSources(nodes)
	key := strings.Join(srcs, ",")
	if key == s.registryPolicyKey && time.Since(s.registryPolicyAt) < 10*time.Minute {
		return
	}
	if err := s.kube.Apply(ctx, registryPolicyObj(srcs)); err != nil {
		s.logf("registry policy: %v", err)
		return
	}
	s.registryPolicyKey, s.registryPolicyAt = key, time.Now()
}

// registryKeep returns, per app, what garbage collection must keep: the tags of the
// current and last good builds plus the build cache, and the digests apps run.
// Apps deploy by digest, so digests are what matter; a tag can be overwritten.
func registryKeep(list []*apps.App) map[string][]string {
	keep := map[string][]string{}
	for _, a := range list {
		if a.Build == nil {
			continue
		}
		set := map[string]bool{BuildCacheTag: true}
		for _, ref := range []string{a.Build.Image, a.Build.LastGood} {
			name, digest, _ := strings.Cut(ref, "@")
			if i := strings.LastIndex(name, ":"); i > 0 && !strings.Contains(name[i:], "/") {
				set[name[i+1:]] = true
			}
			if strings.HasPrefix(digest, "sha256:") {
				set[digest] = true
			}
		}
		for k := range set {
			keep[a.Name] = append(keep[a.Name], k)
		}
		sort.Strings(keep[a.Name])
	}
	return keep
}

// keepSpec renders the keep list as "app=item,item app2=item" for the GC script.
func keepSpec(keep map[string][]string) string {
	names := make([]string, 0, len(keep))
	for n := range keep {
		names = append(names, n)
	}
	sort.Strings(names)
	parts := make([]string, 0, len(names))
	for _, n := range names {
		parts = append(parts, n+"="+strings.Join(keep[n], ","))
	}
	return strings.Join(parts, " ")
}

// registryGCScript removes old tags, then every manifest that is neither kept by digest,
// pointed to by a kept tag, nor referenced from one of those (index children, cache
// layers), and finally lets the registry delete blobs nothing references any more.
const registryGCScript = `set -eu
V2=/var/lib/registry/docker/registry/v2
R=$V2/repositories
[ -d "$R" ] || { echo "registry empty"; exit 0; }
refs() { # digests mentioned inside a manifest or index
  d=$1; f="$V2/blobs/sha256/$(echo "${d#sha256:}" | cut -c1-2)/${d#sha256:}/data"
  [ -f "$f" ] && grep -o 'sha256:[0-9a-f]\{64\}' "$f" || true
}
for repo in "$R"/*; do
  [ -d "$repo" ] || continue
  app=$(basename "$repo"); items=""
  [ "$app" = sb-control ] && continue # the control plane image, pulled by servers that join later
  if [ "$app" = sb-uploads ]; then # uploaded source: keep what apps use and anything from the last 2 days
    items="keep $SB_UPLOADS"
    for t in "$repo"/_manifests/tags/*; do
      [ -d "$t" ] && [ -z "$(find "$t" -maxdepth 0 -mtime +2)" ] && items="$items $(basename "$t")"
    done
  fi
  [ "$app" = sb-uploads ] || for entry in $SB_KEEP; do [ "${entry%%=*}" = "$app" ] && items=$(echo "${entry#*=}" | tr , ' '); done
  if [ -z "$items" ]; then echo "removing images of $app (app gone or not built from source)"; rm -rf "$repo"; continue; fi
  keepd=""
  for k in $items; do case "$k" in sha256:*) keepd="$keepd $k";; esac; done
  for t in "$repo"/_manifests/tags/*; do
    [ -d "$t" ] || continue
    name=$(basename "$t"); keep=no
    for k in $items; do [ "$k" = "$name" ] && keep=yes; done
    if [ "$keep" = no ]; then echo "removing tag $app:$name"; rm -rf "$t"; continue; fi
    [ -f "$t/current/link" ] && keepd="$keepd $(cat "$t/current/link")"
  done
  all="$keepd"
  for d in $keepd; do all="$all $(refs "$d" | tr '\n' ' ')"; done
  for rev in "$repo"/_manifests/revisions/sha256/*; do
    [ -d "$rev" ] || continue
    d="sha256:$(basename "$rev")"
    case " $all " in *" $d "*) ;; *) echo "removing manifest $app@$d"; rm -rf "$rev";; esac
  done
done
registry garbage-collect /etc/distribution/config.yml 2>&1 | tail -n 2
echo "SB_GC done"
`

// registryGCJob removes images no app uses any more. It runs next to the registry pod
// (the volume is ReadWriteOnce) and only while no build is pushing.
func registryGCJob(id, node, keep, uploads string) kube.Obj {
	return kube.Obj{
		"apiVersion": "batch/v1", "kind": "Job",
		"metadata": kube.Obj{"name": "registry-gc-" + id, "namespace": registryNamespace,
			"labels": kube.Obj{apps.ManagedByLabel: "slicedbread", "slicedbread.dev/registry-gc": id}},
		"spec": kube.Obj{
			"backoffLimit": 0, "activeDeadlineSeconds": 1800, "ttlSecondsAfterFinished": 86400,
			"template": kube.Obj{"spec": kube.Obj{
				"restartPolicy": "Never", "automountServiceAccountToken": false, "nodeName": node,
				"containers": []any{kube.Obj{
					"name": "gc", "image": registryImage, "command": []any{"sh", "-c", registryGCScript},
					"env":             []any{kube.Obj{"name": "SB_KEEP", "value": keep}, kube.Obj{"name": "SB_UPLOADS", "value": uploads}},
					"volumeMounts":    []any{kube.Obj{"name": "data", "mountPath": "/var/lib/registry"}},
					"securityContext": kube.Obj{"allowPrivilegeEscalation": false},
				}},
				"volumes": []any{kube.Obj{"name": "data", "persistentVolumeClaim": kube.Obj{"claimName": "sb-registry"}}},
			}},
		},
	}
}

// maybeRegistryGC starts a garbage collection once a day when no build is running.
func (s *Server) maybeRegistryGC(ctx context.Context, list []*apps.App) {
	if s.registryGCAt.IsZero() {
		// After a restart or leader change, continue from the newest GC job.
		if jobs, err := s.kube.List(ctx, "batch/v1", "Job", registryNamespace, "slicedbread.dev/registry-gc"); err == nil {
			for _, j := range jobs {
				if t, err := time.Parse(time.RFC3339, kube.Str(j, "metadata", "creationTimestamp")); err == nil && t.After(s.registryGCAt) {
					s.registryGCAt = t
				}
			}
		}
	}
	if time.Since(s.registryGCAt) < registryGCEvery {
		return
	}
	for _, a := range list {
		if a.Build != nil && a.Build.Phase == "running" {
			return
		}
	}
	pods, err := s.kube.List(ctx, "v1", "Pod", registryNamespace, "app=sb-registry")
	if err != nil || len(pods) == 0 {
		return
	}
	node := kube.Str(pods[0], "spec", "nodeName")
	if node == "" {
		return
	}
	id := randID(6)
	var uploads []string
	for _, a := range list {
		if a.Upload != nil {
			uploads = append(uploads, a.Upload.ID)
		}
	}
	if err := s.kube.Apply(ctx, registryGCJob(id, node, keepSpec(registryKeep(list)), strings.Join(uploads, " "))); err != nil {
		s.logf("registry gc: %v", err)
		return
	}
	s.registryGCAt = time.Now()
	s.logf("registry gc %s started on %s", id, node)
}
