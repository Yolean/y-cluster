// Package inventory keeps host-local records of provisioned
// clusters: which config dir (-c) each one came from, plus the
// host ports it binds. Written by `y-cluster provision`, removed
// by `y-cluster teardown`.
//
// The point is discoverability, not truth: teardown needs a
// `-c <dir>` and users forget which path that was (a cluster
// provisioned by a repo's test.sh may live under something like
// itest/cluster/docker). A record lets `y-cluster teardown`
// without -c list candidate paths, and lets provision's preflight
// name the cluster that holds a conflicting port. Clusters
// provisioned by binaries that predate this package have no
// record and are simply not listed -- the backends' own state
// (containers, VMs, cache sidecars) remains authoritative.
//
// Records live one JSON file per kubeconfig context under
// $Y_CLUSTER_INVENTORY_DIR, defaulting to ~/.cache/y-cluster-clusters
// -- the same flat-suffixed-dir convention as the qemu and hetzner
// cache dirs. Context is the key because provision's preflight
// already enforces one cluster per context name.
package inventory

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// recordVersion guards forward-compat the same way the qemu
// sidecar's stateVersion does: List skips records written by a
// newer schema rather than guessing at their meaning.
const recordVersion = 1

// Record is one provisioned cluster, as JSON on disk.
type Record struct {
	Version  int    `json:"version"`
	Context  string `json:"context"`
	Name     string `json:"name"`
	Provider string `json:"provider"`
	// ConfigDir is the absolute path of the -c directory the
	// cluster was provisioned from. It can go stale (repo moved
	// or deleted); consumers should treat it as a hint.
	ConfigDir string `json:"configDir"`
	// HostPorts are the 127.0.0.1 ports the cluster binds
	// (port forwards, qemu ssh). Empty for remote providers.
	HostPorts     []string `json:"hostPorts,omitempty"`
	ProvisionedAt string   `json:"provisionedAt"` // RFC3339
}

// Dir resolves the records directory: $Y_CLUSTER_INVENTORY_DIR
// when set (e2e isolation), else ~/.cache/y-cluster-clusters.
func Dir() (string, error) {
	if env := os.Getenv("Y_CLUSTER_INVENTORY_DIR"); env != "" {
		return env, nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("locate home: %w", err)
	}
	return filepath.Join(home, ".cache", "y-cluster-clusters"), nil
}

// Save writes rec as <dir>/<context>.json, atomic via .tmp+rename.
// Stamps Version and ProvisionedAt. A context with a path
// separator would escape the dir, so it is rejected.
func Save(rec Record) error {
	if rec.Context == "" || strings.ContainsAny(rec.Context, "/\\") {
		return fmt.Errorf("inventory: unusable context name %q", rec.Context)
	}
	dir, err := Dir()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	rec.Version = recordVersion
	rec.ProvisionedAt = time.Now().Format(time.RFC3339)
	data, err := json.MarshalIndent(rec, "", "  ")
	if err != nil {
		return err
	}
	path := filepath.Join(dir, rec.Context+".json")
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	return nil
}

// Remove deletes the record for context. Missing is success --
// teardown of a cluster provisioned by an older binary, or a
// re-run teardown, must not error here.
func Remove(context string) error {
	dir, err := Dir()
	if err != nil {
		return err
	}
	if err := os.Remove(filepath.Join(dir, context+".json")); err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
}

// List returns every readable current-version record, sorted by
// context. A missing dir means no clusters (empty, nil error).
// Unparsable or newer-version files are skipped, not fatal: one
// corrupt record must not hide the others from a teardown listing.
func List() ([]Record, error) {
	dir, err := Dir()
	if err != nil {
		return nil, err
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	var recs []Record
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		data, err := os.ReadFile(filepath.Join(dir, e.Name()))
		if err != nil {
			continue
		}
		var rec Record
		if err := json.Unmarshal(data, &rec); err != nil || rec.Version != recordVersion {
			continue
		}
		recs = append(recs, rec)
	}
	sort.Slice(recs, func(i, j int) bool { return recs[i].Context < recs[j].Context })
	return recs, nil
}

// FindByHostPort returns the record binding the given host port,
// or nil. Best-effort (read errors yield nil) -- callers use it
// to enrich an error message, never to gate behavior.
func FindByHostPort(port string) *Record {
	if port == "" {
		return nil
	}
	recs, err := List()
	if err != nil {
		return nil
	}
	for i := range recs {
		for _, hp := range recs[i].HostPorts {
			if hp == port {
				return &recs[i]
			}
		}
	}
	return nil
}
