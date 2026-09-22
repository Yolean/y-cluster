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
//	<context>-controlplane.yaml  -- the machine config sent as cloudconfig (mode 0600)
//
// cacheDir defaults to ~/.cache/y-cluster-glesys; tests use a
// t.TempDir().
type state struct {
	Context    string `json:"context"`
	ServerID   string `json:"serverID"`
	DataCenter string `json:"dataCenter"`
	// IPv4 is the address reserved ahead of the server so the
	// Talos cluster endpoint could name it. Teardown releases it
	// with the server.
	IPv4 string `json:"ipv4"`
	// Bootstrapped records that etcd bootstrap succeeded. Talos
	// refuses a second bootstrap, so a provision that is retried
	// after a failure past that point skips the call.
	Bootstrapped bool `json:"bootstrapped,omitempty"`
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
	for _, p := range []string{
		statePath(cacheDir, context),
		TalosconfigPath(cacheDir, context),
		machineConfigPath(cacheDir, context),
	} {
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
