package qemu

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

// nowFunc is the wall clock used for lifetime deadline math. A
// package var so tests can pin it; production uses time.Now.
var nowFunc = time.Now

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

	// Lifetime fields are additive (added after stateVersion 1)
	// and all omitempty, so an old sidecar without them decodes
	// cleanly to "no lifetime" and an old binary ignores them.
	// They are NOT a reason to bump stateVersion.
	//
	// Lifetime/OnExpiry are the policy copied from config at
	// provision; ExpiresAt is the absolute deadline, anchored to
	// the most recent provision/start (not to provision alone --
	// an appliance disk may boot long after it was built).
	Lifetime  string `json:"lifetime,omitempty"`
	OnExpiry  string `json:"onExpiry,omitempty"`
	ExpiresAt string `json:"expiresAt,omitempty"` // RFC3339; empty = no deadline
}

// statePath returns the sidecar path for (cacheDir, name).
func statePath(cacheDir, name string) string {
	return filepath.Join(cacheDir, name+".json")
}

// saveState writes the launch-relevant subset of cfg to the
// sidecar, including the lifetime policy (Lifetime/OnExpiry). The
// ExpiresAt deadline is NOT set here -- it is armed separately by
// armLifetime so the deadline anchors to start, and so `extend` can
// move it without rewriting launch state. Atomic via .tmp+rename.
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
		Lifetime:     cfg.Lifetime,
		OnExpiry:     cfg.OnExpiry,
	}
	return writeSidecar(cfg.CacheDir, cfg.Name, s)
}

// writeSidecar atomically marshals s to <cacheDir>/<name>.json via
// a .tmp+rename so a crash mid-write doesn't leave a half-written
// file Start would later fail to parse.
func writeSidecar(cacheDir, name string, s savedState) error {
	data, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return err
	}
	path := statePath(cacheDir, name)
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

// readSidecar reads and version-checks the raw sidecar. Shared by
// loadState and the lifetime helpers.
func readSidecar(cacheDir, name string) (savedState, error) {
	path := statePath(cacheDir, name)
	data, err := os.ReadFile(path)
	if err != nil {
		return savedState{}, err
	}
	var s savedState
	if err := json.Unmarshal(data, &s); err != nil {
		return savedState{}, fmt.Errorf("parse %s: %w", path, err)
	}
	if s.Version != stateVersion {
		return savedState{}, fmt.Errorf(
			"%s: unsupported state version %d (want %d); re-provision to refresh",
			path, s.Version, stateVersion)
	}
	return s, nil
}

// loadState reads <cacheDir>/<name>.json and rehydrates a runtime
// Config. Kubeconfig is re-resolved from $KUBECONFIG at call time
// rather than persisted -- it's an environmental concern that
// shouldn't bake into the sidecar.
func loadState(cacheDir, name string) (Config, error) {
	s, err := readSidecar(cacheDir, name)
	if err != nil {
		return Config{}, err
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
		Lifetime:     s.Lifetime,
		OnExpiry:     s.OnExpiry,
	}, nil
}

// LifetimeState is the persisted auto-expiry view of a cluster.
type LifetimeState struct {
	// MaxRun is the configured budget (Go duration string), empty
	// when no lifetime is set.
	MaxRun string
	// OnExpiry is the local action at the deadline.
	OnExpiry string
	// ExpiresAt is the absolute deadline; zero when unset.
	ExpiresAt time.Time
}

// Enabled reports whether a budget is configured.
func (l LifetimeState) Enabled() bool { return l.MaxRun != "" && l.MaxRun != "0" }

// Remaining is the time left until the deadline; negative when past
// due, zero when no deadline is armed.
func (l LifetimeState) Remaining() time.Duration {
	if l.ExpiresAt.IsZero() {
		return 0
	}
	return l.ExpiresAt.Sub(nowFunc())
}

// Expired reports whether an armed deadline is at or past now.
func (l LifetimeState) Expired() bool {
	return !l.ExpiresAt.IsZero() && !nowFunc().Before(l.ExpiresAt)
}

// loadLifetime returns the persisted lifetime policy + deadline.
func loadLifetime(cacheDir, name string) (LifetimeState, error) {
	s, err := readSidecar(cacheDir, name)
	if err != nil {
		return LifetimeState{}, err
	}
	ls := LifetimeState{MaxRun: s.Lifetime, OnExpiry: s.OnExpiry}
	if s.ExpiresAt != "" {
		t, err := time.Parse(time.RFC3339, s.ExpiresAt)
		if err != nil {
			return LifetimeState{}, fmt.Errorf("parse expiresAt %q: %w", s.ExpiresAt, err)
		}
		ls.ExpiresAt = t
	}
	return ls, nil
}

// armLifetime sets ExpiresAt = now + MaxRun, anchoring the deadline
// to the current moment (provision or start). It is a no-op that
// returns a zero deadline when the cluster has no lifetime budget.
// Returns the new deadline so callers can schedule a host timer.
func armLifetime(cacheDir, name string) (time.Time, error) {
	s, err := readSidecar(cacheDir, name)
	if err != nil {
		return time.Time{}, err
	}
	if s.Lifetime == "" || s.Lifetime == "0" {
		return time.Time{}, nil
	}
	d, err := time.ParseDuration(s.Lifetime)
	if err != nil {
		return time.Time{}, fmt.Errorf("parse lifetime %q: %w", s.Lifetime, err)
	}
	deadline := nowFunc().Add(d)
	s.ExpiresAt = deadline.Format(time.RFC3339)
	if err := writeSidecar(cacheDir, name, s); err != nil {
		return time.Time{}, err
	}
	return deadline, nil
}

// setExpiresAt persists an explicit deadline, used by `extend` to
// push the deadline out without re-anchoring to now.
func setExpiresAt(cacheDir, name string, t time.Time) error {
	s, err := readSidecar(cacheDir, name)
	if err != nil {
		return err
	}
	s.ExpiresAt = t.Format(time.RFC3339)
	return writeSidecar(cacheDir, name, s)
}

// removeState deletes the sidecar and ignores "not present"
// errors so teardown is idempotent.
func removeState(cacheDir, name string) error {
	if err := os.Remove(statePath(cacheDir, name)); err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
}
