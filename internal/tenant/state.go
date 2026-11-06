package tenant

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/melzi-cloud/sliced-bread/internal/sshx"
)

// Node is one tenant server.
type Node struct {
	Name      string `json:"name"`
	ID        int64  `json:"id"`
	PublicIP  string `json:"public_ip"`
	PrivateIP string `json:"private_ip"`
	HostKey   string `json:"host_key"` // pinned, authorized_keys format
}

// State describes a running development tenant. It contains no secrets
// except the path to a per-run SSH key with a short lifetime.
type State struct {
	RunID      string    `json:"run_id"`
	Owner      string    `json:"owner"`
	ExpiresAt  time.Time `json:"expires_at"`
	Location   string    `json:"location"`
	Domain     string    `json:"domain"` // <run-id>.<test domain>
	LBIP       string    `json:"lb_ip"`
	Nodes      []Node    `json:"nodes"`
	DNSRecords []string  `json:"dns_records"`
	Dir        string    `json:"-"`
}

// StateRoot is where per-run state lives, relative to the repo root. Gitignored.
const StateRoot = ".sb/runs"

func runDir(root, runID string) string { return filepath.Join(root, runID) }

// Save writes state.json.
func (s *State) Save() error {
	if err := os.MkdirAll(s.Dir, 0o700); err != nil {
		return err
	}
	b, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(s.Dir, "state.json"), b, 0o600)
}

// LoadState reads state for a run.
func LoadState(root, runID string) (*State, error) {
	dir := runDir(root, runID)
	b, err := os.ReadFile(filepath.Join(dir, "state.json"))
	if err != nil {
		return nil, err
	}
	var s State
	if err := json.Unmarshal(b, &s); err != nil {
		return nil, err
	}
	s.Dir = dir
	return &s, nil
}

// SSHKey loads the per-run SSH key.
func (s *State) SSHKey() (sshx.KeyPair, error) {
	b, err := os.ReadFile(filepath.Join(s.Dir, "id_ed25519"))
	if err != nil {
		return sshx.KeyPair{}, err
	}
	return sshx.ParseKeyPair(b)
}

// Target returns an SSH target for node i.
func (s *State) Target(i int) (sshx.Target, error) {
	if i < 0 || i >= len(s.Nodes) {
		return sshx.Target{}, fmt.Errorf("node %d out of range", i)
	}
	kp, err := s.SSHKey()
	if err != nil {
		return sshx.Target{}, err
	}
	n := s.Nodes[i]
	return sshx.Target{Addr: n.PublicIP + ":22", User: "root", Signer: kp.Signer, HostKey: n.HostKey}, nil
}
