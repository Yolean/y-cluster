package hetzner

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
)

// state is the per-context sidecar y-cluster persists alongside
// the SSH key. Anything Teardown / Lookup / lifecycle subcommands
// need to act on the cluster without re-reading the YAML config
// (which the operator's CWD may not have) lives here.
//
// File layout under cacheDir:
//
//	<context>.json        -- this file
//	<context>-ssh         -- private key (mode 0600)
//	<context>-ssh.pub     -- public key  (mode 0644)
//
// cacheDir defaults to ~/.cache/y-cluster-hetzner; tests use a
// t.TempDir().
type state struct {
	Context    string `json:"context"`
	ServerID   int64  `json:"serverID"`
	ServerName string `json:"serverName"`
	IPv4       string `json:"ipv4"`
	SSHUser    string `json:"sshUser"`
	SSHKeyName string `json:"sshKeyName"` // Hetzner SSHKey resource name (uploaded by us)
	// AtJobID is the at(1) job number the auto-teardown is
	// scheduled under. Phase 2 fills it; phase 1 leaves it 0.
	AtJobID int `json:"atJobID,omitempty"`
	// LBGroup mirrors cfg.LBGroup so Teardown -- which only sees
	// the context name, not the YAML config -- knows which lb-group
	// to enumerate when deciding whether to delete the LB.
	LBGroup string `json:"lbGroup,omitempty"`
	// LBID is the Hetzner LB resource id this server attached to.
	// Persisted so Teardown can delete it without a name lookup
	// when it turns out to be the last attached server.
	LBID int64 `json:"lbID,omitempty"`
}

func statePath(cacheDir, context string) string {
	return filepath.Join(cacheDir, context+".json")
}

func saveState(cacheDir string, s state) error {
	if err := os.MkdirAll(cacheDir, 0o755); err != nil {
		return fmt.Errorf("mkdir cache: %w", err)
	}
	data, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return err
	}
	tmp := statePath(cacheDir, s.Context) + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, statePath(cacheDir, s.Context))
}

func loadState(cacheDir, context string) (state, error) {
	data, err := os.ReadFile(statePath(cacheDir, context))
	if err != nil {
		return state{}, err
	}
	var s state
	if err := json.Unmarshal(data, &s); err != nil {
		return state{}, fmt.Errorf("parse state: %w", err)
	}
	return s, nil
}

func deleteState(cacheDir, context string) error {
	err := os.Remove(statePath(cacheDir, context))
	if err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
}
