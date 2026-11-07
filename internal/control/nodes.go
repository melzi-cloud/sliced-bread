package control

import (
	"encoding/json"
	"net/http"
	"sort"

	"github.com/melzi-cloud/sliced-bread/internal/guard"
	"github.com/melzi-cloud/sliced-bread/internal/kube"
	"github.com/melzi-cloud/sliced-bread/internal/tenant"
)

type nodeStatus struct {
	tenant.NodeInfo
	Ready bool `json:"ready"`
}

// apiNodes lists the servers with their public address, pinned SSH host key and
// readiness, Ready ones first. None of it is secret; the CLI uses it after a server
// was replaced.
func (s *Server) apiNodes(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	var cm kube.Obj
	var list []tenant.NodeInfo
	if err := s.kube.Get(ctx, "v1", "ConfigMap", s.cfg.Namespace, tenant.NodesConfigMap, &cm); err == nil {
		json.Unmarshal([]byte(kube.Str(cm, "data", "nodes.json")), &list)
	}
	ready := map[string]bool{}
	if nodes, err := s.kube.List(ctx, "v1", "Node", "", ""); err == nil {
		for _, n := range nodes {
			st, _ := conditionStatus(n, "Ready")
			ready[kube.Str(n, "metadata", "name")] = st == "True"
		}
	}
	out := []nodeStatus{}
	for _, n := range list {
		out = append(out, nodeStatus{NodeInfo: n, Ready: ready[n.Name]})
	}
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].Ready != out[j].Ready {
			return out[i].Ready
		}
		return out[i].Name < out[j].Name
	})
	writeJSON(w, 200, map[string]any{"nodes": out, "repair": guard.LoadState(ctx, s.kube)})
}
