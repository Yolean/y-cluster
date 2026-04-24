// Package kubeconfig manages the host's kubeconfig for local cluster
// provisioners. It provides consistent context naming, merge behavior,
// and cleanup across all provisioner types.
package kubeconfig

import (
	"fmt"
	"os"
	"os/exec"
	"strings"

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

// CleanupStale removes any existing context, cluster, and user entries
// matching this manager's names. Safe to call before provision — it
// won't error if entries don't exist.
func (m *Manager) CleanupStale() {
	for _, args := range [][]string{
		{"config", "delete-context", m.Context},
		{"config", "delete-cluster", m.ClusterName},
		{"config", "delete-user", m.ClusterName},
	} {
		cmd := exec.Command("kubectl", args...)
		cmd.Env = append(os.Environ(), "KUBECONFIG="+m.Path)
		cmd.Run() // ignore errors — entries may not exist
	}
}

// Import takes a raw kubeconfig (e.g. from k3s), renames the default
// entries to this manager's context/cluster/user names, and merges
// into the host kubeconfig at m.Path.
func (m *Manager) Import(rawKubeconfig []byte) error {
	tmpFile := m.Path + ".tmp"
	if err := os.WriteFile(tmpFile, rawKubeconfig, 0o600); err != nil {
		return fmt.Errorf("write temp kubeconfig: %w", err)
	}
	defer os.Remove(tmpFile)

	// Rename default context to our context name
	cmd := exec.Command("kubectl", "config", "rename-context", "default", m.Context)
	cmd.Env = append(os.Environ(), "KUBECONFIG="+tmpFile)
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("rename context: %s: %w", out, err)
	}

	// Rename cluster and user entries
	content, err := os.ReadFile(tmpFile)
	if err != nil {
		return err
	}
	renamed := strings.ReplaceAll(string(content), "name: default", "name: "+m.ClusterName)
	renamed = strings.ReplaceAll(renamed, "cluster: default", "cluster: "+m.ClusterName)
	renamed = strings.ReplaceAll(renamed, "user: default", "user: "+m.ClusterName)
	if err := os.WriteFile(tmpFile, []byte(renamed), 0o600); err != nil {
		return err
	}

	// Merge into existing kubeconfig
	if _, err := os.Stat(m.Path); err == nil {
		m.logger.Info("merging into existing kubeconfig", zap.String("path", m.Path))
		mergedFile := tmpFile + "-merged"
		cmd := exec.Command("kubectl", "config", "view", "--flatten")
		cmd.Env = append(os.Environ(), "KUBECONFIG="+tmpFile+":"+m.Path)
		merged, err := cmd.Output()
		if err != nil {
			return fmt.Errorf("merge kubeconfig: %w", err)
		}
		if err := os.WriteFile(mergedFile, merged, 0o600); err != nil {
			return err
		}
		return os.Rename(mergedFile, m.Path)
	}

	// No existing kubeconfig — just move the temp file
	return os.Rename(tmpFile, m.Path)
}

// CleanupTeardown removes the context and fixes null→[] for kubie
// compatibility. kubectl writes `contexts: null` instead of `contexts: []`
// when the last entry is removed.
func (m *Manager) CleanupTeardown() {
	m.CleanupStale()
	m.fixNullLists()
}

func (m *Manager) fixNullLists() {
	data, err := os.ReadFile(m.Path)
	if err != nil {
		return
	}
	content := string(data)
	changed := false
	for _, field := range []string{"contexts", "clusters", "users"} {
		old := field + ": null"
		new := field + ": []"
		if strings.Contains(content, old) {
			content = strings.ReplaceAll(content, old, new)
			changed = true
		}
	}
	if changed {
		os.WriteFile(m.Path, []byte(content), 0o600)
	}
}
