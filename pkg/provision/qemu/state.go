package qemu

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
)

// stateVersion guards forward-compat: a newer y-cluster reading an
// older sidecar bails out with a clear error rather than guessing.
// Bump when the schema changes incompatibly.
const stateVersion = 1

// savedState is the JSON sidecar written at <cacheDir>/<name>.json
// at provision time. Read by Start when the user runs
// `y-cluster start` without a -c <dir>: enough to re-invoke startVM
// against the existing qcow2 without re-running cloud-init (the
// disk already has the cluster-init user and SSH keys).
//
// Decoupled from Config: Config is a runtime struct, this is the
// on-disk shape. Kubeconfig (env-derived) and Registries / Gateway
// (cluster-state, not launch-state) are deliberately omitted --
// they live inside the cluster on the qcow2 disk now.
type savedState struct {
	Version      int           `json:"version"`
	Name         string        `json:"name"`
	DiskSize     string        `json:"diskSize"`
	Memory       string        `json:"memory"`
	CPUs         string        `json:"cpus"`
	SSHPort      string        `json:"sshPort"`
	PortForwards []PortForward `json:"portForwards"`
	Context      string        `json:"context"`
	CacheDir     string        `json:"cacheDir"`
	K3s          K3s           `json:"k3s"`
}

// statePath returns the sidecar path for (cacheDir, name).
func statePath(cacheDir, name string) string {
	return filepath.Join(cacheDir, name+".json")
}

// saveState writes the launch-relevant subset of cfg to the
// sidecar. Atomic via a .tmp+rename so a crash mid-write doesn't
// leave a half-written file Start would later fail to parse.
func saveState(cfg Config) error {
	s := savedState{
		Version:      stateVersion,
		Name:         cfg.Name,
		DiskSize:     cfg.DiskSize,
		Memory:       cfg.Memory,
		CPUs:         cfg.CPUs,
		SSHPort:      cfg.SSHPort,
		PortForwards: cfg.PortForwards,
		Context:      cfg.Context,
		CacheDir:     cfg.CacheDir,
		K3s:          cfg.K3s,
	}
	data, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return err
	}
	path := statePath(cfg.CacheDir, cfg.Name)
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return fmt.Errorf("write %s: %w", tmp, err)
	}
	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("rename %s -> %s: %w", tmp, path, err)
	}
	return nil
}

// loadState reads <cacheDir>/<name>.json and rehydrates a runtime
// Config. Kubeconfig is re-resolved from $KUBECONFIG at call time
// rather than persisted -- it's an environmental concern that
// shouldn't bake into the sidecar.
func loadState(cacheDir, name string) (Config, error) {
	path := statePath(cacheDir, name)
	data, err := os.ReadFile(path)
	if err != nil {
		return Config{}, err
	}
	var s savedState
	if err := json.Unmarshal(data, &s); err != nil {
		return Config{}, fmt.Errorf("parse %s: %w", path, err)
	}
	if s.Version != stateVersion {
		return Config{}, fmt.Errorf(
			"%s: unsupported state version %d (want %d); re-provision to refresh",
			path, s.Version, stateVersion)
	}
	return Config{
		Name:         s.Name,
		DiskSize:     s.DiskSize,
		Memory:       s.Memory,
		CPUs:         s.CPUs,
		SSHPort:      s.SSHPort,
		PortForwards: s.PortForwards,
		Context:      s.Context,
		CacheDir:     s.CacheDir,
		Kubeconfig:   os.Getenv("KUBECONFIG"),
		K3s:          s.K3s,
	}, nil
}

// removeState deletes the sidecar and ignores "not present"
// errors so teardown is idempotent.
func removeState(cacheDir, name string) error {
	if err := os.Remove(statePath(cacheDir, name)); err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
}
