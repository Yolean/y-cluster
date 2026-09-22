// Package kubeconfig manages the host's kubeconfig for local cluster
// provisioners. It provides consistent context naming, merge behavior,
// and cleanup across all provisioner types.
//
// Implemented on top of the typed File schema (schema.go) +
// sigs.k8s.io/yaml, NOT k8s.io/client-go/tools/clientcmd. The
// kubeconfig file format is small enough that hand-rolling keeps
// client-go out of y-cluster's binary.
package kubeconfig

import (
	"fmt"
	"os"

	"go.uber.org/zap"
)

// Manager handles kubeconfig operations for a single cluster context.
type Manager struct {
	// Path is the kubeconfig file path.
	Path string
	// Context is the kubectl context name (e.g. "local").
	Context string
	// ClusterName is the cluster entry name in kubeconfig (e.g. "ystack-qemu").
	ClusterName string

	logger *zap.Logger
}

// New creates a Manager for the kubeconfig file at path. An empty
// path is an error rather than a fallback to the environment, so a
// caller (or a test) that did not name a file can never modify the
// operator's real kubeconfig.
func New(path, contextName, clusterName string, logger *zap.Logger) (*Manager, error) {
	if path == "" {
		return nil, fmt.Errorf("kubeconfig path is empty; set KUBECONFIG")
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

// FromEnv is New with the path taken from $KUBECONFIG, for
// provisioners whose config carries no kubeconfig path of its own.
func FromEnv(contextName, clusterName string, logger *zap.Logger) (*Manager, error) {
	return New(os.Getenv("KUBECONFIG"), contextName, clusterName, logger)
}

// CleanupStale removes any existing context, cluster, and user
// entries matching this manager's names. Safe to call before
// provision -- missing entries are a no-op.
func (m *Manager) CleanupStale() {
	if _, err := os.Stat(m.Path); os.IsNotExist(err) {
		return // no kubeconfig, nothing of ours in it
	}
	err := withFileLock(m.Path, func() error {
		cfg, err := Load(m.Path)
		if err != nil {
			// Unreadable for a reason other than "doesn't exist":
			// writing anything back would replace the operator's
			// file with our idea of it.
			return fmt.Errorf("load: %w", err)
		}
		before := len(cfg.Contexts) + len(cfg.Clusters) + len(cfg.Users)
		cfg.removeContext(m.Context)
		cfg.removeCluster(m.ClusterName)
		cfg.removeUser(m.ClusterName)
		if len(cfg.Contexts)+len(cfg.Clusters)+len(cfg.Users) == before {
			// Nothing of ours in there. Leave the file alone, and
			// do not create one that did not exist.
			return nil
		}
		return cfg.Save(m.Path)
	})
	if err != nil {
		m.logger.Warn("kubeconfig cleanup failed",
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

	return withFileLock(m.Path, func() error {
		existing, err := Load(m.Path)
		if err != nil {
			return fmt.Errorf("load existing %s: %w", m.Path, err)
		}
		existing.MergeFrom(incoming)
		if err := existing.Save(m.Path); err != nil {
			return fmt.Errorf("write %s: %w", m.Path, err)
		}
		return nil
	})
}
