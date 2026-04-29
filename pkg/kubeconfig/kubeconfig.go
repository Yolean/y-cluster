// Package kubeconfig manages the host's kubeconfig for local cluster
// provisioners. It provides consistent context naming, merge behavior,
// and cleanup across all provisioner types.
//
// Implemented on top of the typed File schema (schema.go) +
// sigs.k8s.io/yaml, NOT k8s.io/client-go/tools/clientcmd. The
// kubeconfig file format is small enough that hand-rolling lets us
// drop client-go from y-cluster's binary; the Manager's behaviour is
// otherwise identical to the previous clientcmd-backed version.
package kubeconfig

import (
	"fmt"
	"os"

	"go.uber.org/zap"
)

// Manager handles kubeconfig operations for a single cluster context.
type Manager struct {
	// Path is the kubeconfig file path (from KUBECONFIG env).
	Path string
	// Context is the kubectl context name (e.g. "local").
	Context string
	// ClusterName is the cluster entry name in kubeconfig (e.g. "ystack-qemu").
	ClusterName string

	logger *zap.Logger
}

// New creates a Manager from the KUBECONFIG environment variable.
// Returns an error if KUBECONFIG is not set.
func New(contextName, clusterName string, logger *zap.Logger) (*Manager, error) {
	path := os.Getenv("KUBECONFIG")
	if path == "" {
		return nil, fmt.Errorf("KUBECONFIG env must be set")
	}
	if logger == nil {
		logger = zap.NewNop()
	}
	return &Manager{
		Path:        path,
		Context:     contextName,
		ClusterName: clusterName,
		logger:      logger,
	}, nil
}

// CleanupStale removes any existing context, cluster, and user
// entries matching this manager's names. Safe to call before
// provision -- missing entries are a no-op.
func (m *Manager) CleanupStale() {
	cfg, err := Load(m.Path)
	if err != nil {
		// If the file is unreadable for any reason other than
		// "doesn't exist" we'd silently corrupt state by
		// over-writing it; log and bail. Match the previous
		// shell-out's "ignore errors" mood without going so far
		// as to clobber.
		m.logger.Warn("kubeconfig load for cleanup failed",
			zap.String("path", m.Path), zap.Error(err))
		return
	}
	cfg.removeContext(m.Context)
	cfg.removeCluster(m.ClusterName)
	cfg.removeUser(m.ClusterName)
	if err := cfg.Save(m.Path); err != nil {
		m.logger.Warn("kubeconfig write after cleanup failed",
			zap.String("path", m.Path), zap.Error(err))
	}
}

// Import takes a raw kubeconfig (e.g. from k3s), renames its
// `default` context/cluster/user entries to this manager's
// names, and merges into the host kubeconfig at m.Path.
//
// k3s writes a kubeconfig whose context, cluster, and user are
// all called "default". We rename them in-memory rather than
// post-processing the YAML so a future k3s release that writes
// extra fields can't surprise us.
func (m *Manager) Import(rawKubeconfig []byte) error {
	incoming, err := Parse(rawKubeconfig)
	if err != nil {
		return fmt.Errorf("parse incoming kubeconfig: %w", err)
	}
	incoming.renameDefaults(m.Context, m.ClusterName)

	existing, err := Load(m.Path)
	if err != nil {
		return fmt.Errorf("load existing %s: %w", m.Path, err)
	}
	existing.MergeFrom(incoming)
	if err := existing.Save(m.Path); err != nil {
		return fmt.Errorf("write %s: %w", m.Path, err)
	}
	return nil
}

// CleanupTeardown removes the context. The previous version
// also worked around clientcmd writing `null` for empty list
// fields (kubie chokes on that); the schema-based Save here
// always emits initialised-empty slices as `[]`, so the
// post-write fix is no longer needed.
func (m *Manager) CleanupTeardown() {
	m.CleanupStale()
}

