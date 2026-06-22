package qemu

import (
	"fmt"
	"time"

	"go.uber.org/zap"
)

// This file is the exported auto-expiry surface the cmd layer's
// `y-cluster lifetime` verb drives. The deadline math and sidecar
// persistence live in state.go (unexported); these wrappers keep the
// command package free of the sidecar's internals.

// LoadLifetime returns the persisted auto-expiry state for the named
// cluster (policy + absolute deadline). A missing sidecar surfaces
// as the underlying os error so the caller can render a clear
// "qemu-only / not provisioned" message.
func LoadLifetime(cacheDir, name string) (LifetimeState, error) {
	return loadLifetime(cacheDir, name)
}

// ExtendDeadline pushes the persisted deadline out by d and returns
// the new deadline. It errors when no deadline is armed, since
// "extend" only makes sense against an existing budget.
func ExtendDeadline(cacheDir, name string, d time.Duration) (time.Time, error) {
	ls, err := loadLifetime(cacheDir, name)
	if err != nil {
		return time.Time{}, err
	}
	if ls.ExpiresAt.IsZero() {
		return time.Time{}, fmt.Errorf("no lifetime deadline armed for %q; nothing to extend", name)
	}
	nt := ls.ExpiresAt.Add(d)
	if err := setExpiresAt(cacheDir, name, nt); err != nil {
		return time.Time{}, err
	}
	return nt, nil
}

// TeardownByName tears down a cluster identified by its cache sidecar
// rather than a freshly loaded config dir. Used by the `onExpiry:
// teardown` reap action, which only knows the cluster name + cache
// dir. keepDisk is forwarded to the provider teardown.
func TeardownByName(cacheDir, name string, keepDisk bool, logger *zap.Logger) error {
	cfg, err := loadState(cacheDir, name)
	if err != nil {
		return fmt.Errorf("load state for teardown of %q: %w", name, err)
	}
	return TeardownConfig(cfg, keepDisk, logger)
}
