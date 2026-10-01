package glesys

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
)

// state is the per-context sidecar y-cluster persists next to the
// talosconfig. Anything Teardown / Lookup / lifecycle subcommands
// need to act on the cluster without re-reading the YAML config
// (which the operator's CWD may not have) lives here.
//
// File layout under cacheDir:
//
//	<context>.json               -- this file
//	<context>-talosconfig        -- talosctl client config (mode 0600)
//	<context>-controlplane.yaml  -- the control plane's machine config, sent as cloudconfig (mode 0600)
//	<context>-worker-<n>.yaml    -- each worker's, likewise
//
// cacheDir defaults to ~/.cache/y-cluster-glesys; tests use a
// t.TempDir().
type state struct {
	Context    string `json:"context"`
	DataCenter string `json:"dataCenter"`
	// ServerID and IPv4 are the control plane's: the address the
	// kubeconfig and talosconfig point at, and what cluster.Lookup
	// reads. IPv4 is reserved ahead of the server so the Talos
	// cluster endpoint could name it.
	ServerID string `json:"serverID"`
	IPv4     string `json:"ipv4"`
	// Workers are the other servers, in creation order. Each has
	// its own reserved address. Teardown releases every address
	// with its server.
	Workers []nodeState `json:"workers,omitempty"`
	// Bootstrapped records that etcd bootstrap succeeded. Talos
	// refuses a second bootstrap, so a provision that is retried
	// after a failure past that point skips the call.
	Bootstrapped bool `json:"bootstrapped,omitempty"`
}

// nodeState is one worker server.
type nodeState struct {
	ServerID string `json:"serverID"`
	IPv4     string `json:"ipv4"`
}

// servers lists every server id the cluster has, control plane
// first, skipping ones not created yet.
func (s state) servers() []string {
	var ids []string
	if s.ServerID != "" {
		ids = append(ids, s.ServerID)
	}
	for _, w := range s.Workers {
		if w.ServerID != "" {
			ids = append(ids, w.ServerID)
		}
	}
	return ids
}

// addresses lists every reserved address, control plane first.
func (s state) addresses() []string {
	var out []string
	if s.IPv4 != "" {
		out = append(out, s.IPv4)
	}
	for _, w := range s.Workers {
		if w.IPv4 != "" {
			out = append(out, w.IPv4)
		}
	}
	return out
}

// workerAddresses lists the workers' addresses.
func (s state) workerAddresses() []string {
	out := make([]string, 0, len(s.Workers))
	for _, w := range s.Workers {
		out = append(out, w.IPv4)
	}
	return out
}

func statePath(cacheDir, context string) string {
	return filepath.Join(cacheDir, context+".json")
}

// TalosconfigPath is where Provision writes the talosctl client
// config for a context. Exported so the CLI can print it as the
// login hint: Talos has no shell, talosctl is how one reaches the
// node.
func TalosconfigPath(cacheDir, context string) string {
	return filepath.Join(cacheDir, context+"-talosconfig")
}

func machineConfigPath(cacheDir, context string) string {
	return filepath.Join(cacheDir, context+"-controlplane.yaml")
}

func workerConfigPath(cacheDir, context string, n int) string {
	return filepath.Join(cacheDir, fmt.Sprintf("%s-worker-%d.yaml", context, n))
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

// deleteState removes the sidecar and the files it points at.
// Missing files are not errors.
func deleteState(cacheDir, context string) error {
	paths := []string{
		statePath(cacheDir, context),
		TalosconfigPath(cacheDir, context),
		machineConfigPath(cacheDir, context),
	}
	if matches, err := filepath.Glob(filepath.Join(cacheDir, context+"-worker-*.yaml")); err == nil {
		paths = append(paths, matches...)
	}
	for _, p := range paths {
		if err := os.Remove(p); err != nil && !os.IsNotExist(err) {
			return err
		}
	}
	return nil
}

// HasState reports whether a glesys state sidecar exists for the
// given context in the default cache dir. Lifecycle subcommands
// that operate on a stopped cluster (start) cannot use
// cluster.Lookup, which only finds running clusters; sidecar
// presence is a cheap, file-system-only signal for which
// provisioner shipped the context.
func HasState(contextName string) bool {
	_, err := os.Stat(statePath(CacheDir(), contextName))
	return err == nil
}
