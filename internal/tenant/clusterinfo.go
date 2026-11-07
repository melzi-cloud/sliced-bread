package tenant

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// ClusterInfo is what the in-cluster node guard needs to replace a server: the same
// settings sb init used. It is not secret; the join token lives in the sb-join Secret.
type ClusterInfo struct {
	RunID      string `json:"runId"`
	Size       int    `json:"size"`
	Location   string `json:"location"`
	ServerType string `json:"serverType"`
	Image      string `json:"image"`
	K3sVersion string `json:"k3sVersion"`
	Domain     string `json:"domain"`
}

// NodeInfo is the public part of a node: how to reach it over SSH and its pinned host key.
type NodeInfo struct {
	Name      string `json:"name"`
	PublicIP  string `json:"publicIp"`
	PrivateIP string `json:"privateIp"`
	HostKey   string `json:"hostKey"`
	ServerID  int64  `json:"serverId"`
}

const (
	ClusterInfoConfigMap = "sb-install"
	NodesConfigMap       = "sb-nodes"
	JoinSecret           = "sb-join"
)

// NodesFromState converts state nodes to NodeInfo.
func NodesFromState(st *State) []NodeInfo {
	out := make([]NodeInfo, 0, len(st.Nodes))
	for _, n := range st.Nodes {
		out = append(out, NodeInfo{Name: n.Name, PublicIP: n.PublicIP, PrivateIP: n.PrivateIP, HostKey: n.HostKey, ServerID: n.ID})
	}
	return out
}

// writeClusterInfo stores the install settings, the current join token and the node list
// in sb-system, over stdin.
func (p *Provisioner) writeClusterInfo(ctx context.Context, st *State, token string) error {
	info := ClusterInfo{RunID: st.RunID, Size: st.Size, Location: p.Cfg.Location, ServerType: p.Cfg.ServerType,
		Image: p.Cfg.Image, K3sVersion: p.Cfg.K3sVersion, Domain: st.Domain}
	ib, _ := json.Marshal(info)
	nb, _ := json.Marshal(NodesFromState(st))
	doc := fmt.Sprintf(`apiVersion: v1
kind: Namespace
metadata: {name: sb-system}
---
apiVersion: v1
kind: Secret
metadata: {name: %s, namespace: sb-system}
type: Opaque
stringData:
  token: %q
---
apiVersion: v1
kind: ConfigMap
metadata: {name: %s, namespace: sb-system}
data:
  info.json: %q
---
apiVersion: v1
kind: ConfigMap
metadata: {name: %s, namespace: sb-system}
data:
  nodes.json: %q
`, JoinSecret, token, ClusterInfoConfigMap, string(ib), NodesConfigMap, string(nb))
	if _, err := st.Kubectl(ctx, "apply -f -", []byte(doc)); err != nil {
		return fmt.Errorf("store cluster info: %w", err)
	}
	return nil
}

// RefreshNodes updates the local node list from the install's API (nodes can be replaced
// by the in-cluster guard). It uses the agent token saved next to the state and is best
// effort: on any error the local list is kept.
func (st *State) RefreshNodes(ctx context.Context) error {
	b, err := os.ReadFile(filepath.Join(st.Dir, "cli-agent.json"))
	if err != nil {
		return err
	}
	var cfg CLIConfig
	if err := json.Unmarshal(b, &cfg); err != nil || cfg.API == "" {
		return fmt.Errorf("no API config")
	}
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimSuffix(cfg.API, "/")+"/v1/nodes", nil)
	req.Header.Set("Authorization", "Bearer "+cfg.Token)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return fmt.Errorf("GET /v1/nodes: HTTP %d", resp.StatusCode)
	}
	var v struct {
		Nodes []NodeInfo `json:"nodes"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&v); err != nil || len(v.Nodes) == 0 {
		return fmt.Errorf("no nodes in API answer")
	}
	nodes := make([]Node, 0, len(v.Nodes))
	for _, n := range v.Nodes {
		if n.PublicIP == "" || n.HostKey == "" {
			continue
		}
		nodes = append(nodes, Node{Name: n.Name, ID: n.ServerID, PublicIP: n.PublicIP, PrivateIP: n.PrivateIP, HostKey: n.HostKey})
	}
	if len(nodes) == 0 {
		return fmt.Errorf("no usable nodes")
	}
	st.Nodes = nodes
	return st.Save()
}
