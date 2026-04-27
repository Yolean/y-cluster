// Package kubeconfig manages the host's kubeconfig for local cluster
// provisioners. It provides consistent context naming, merge behavior,
// and cleanup across all provisioner types.
//
// All operations go through k8s.io/client-go/tools/clientcmd —
// the same kubeconfig parser kubectl itself uses — so we get
// typed errors (clientcmdapi-shaped Config, *fs.PathError on the
// file ops) instead of "exit status 1" from a kubectl shell-out.
package kubeconfig

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"strings"

	"go.uber.org/zap"
	"k8s.io/client-go/tools/clientcmd"
	clientcmdapi "k8s.io/client-go/tools/clientcmd/api"
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
// provision — missing entries are a no-op.
func (m *Manager) CleanupStale() {
	cfg, err := loadOrEmpty(m.Path)
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
	delete(cfg.Contexts, m.Context)
	delete(cfg.Clusters, m.ClusterName)
	delete(cfg.AuthInfos, m.ClusterName)
	if cfg.CurrentContext == m.Context {
		cfg.CurrentContext = ""
	}
	if err := clientcmd.WriteToFile(*cfg, m.Path); err != nil {
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
	incoming, err := clientcmd.Load(rawKubeconfig)
	if err != nil {
		return fmt.Errorf("parse incoming kubeconfig: %w", err)
	}
	renameDefaults(incoming, m.Context, m.ClusterName)

	existing, err := loadOrEmpty(m.Path)
	if err != nil {
		return fmt.Errorf("load existing %s: %w", m.Path, err)
	}
	merge(existing, incoming)

	if err := clientcmd.WriteToFile(*existing, m.Path); err != nil {
		return fmt.Errorf("write %s: %w", m.Path, err)
	}
	return nil
}

// CleanupTeardown removes the context and (historically) fixed
// `null → []` for kubie compatibility. clientcmd's writer emits
// `null` for empty maps too, so we still apply the post-write
// fix.
func (m *Manager) CleanupTeardown() {
	m.CleanupStale()
	m.fixNullLists()
}

// renameDefaults rewrites the canonical k3s "default" entry
// names to this Manager's. We don't iterate every entry — we
// know k3s's shape and renaming foreign entries would be wrong
// in a merged kubeconfig.
func renameDefaults(cfg *clientcmdapi.Config, contextName, clusterName string) {
	if c, ok := cfg.Contexts["default"]; ok {
		c.Cluster = clusterName
		c.AuthInfo = clusterName
		cfg.Contexts[contextName] = c
		delete(cfg.Contexts, "default")
	}
	if cl, ok := cfg.Clusters["default"]; ok {
		cfg.Clusters[clusterName] = cl
		delete(cfg.Clusters, "default")
	}
	if ai, ok := cfg.AuthInfos["default"]; ok {
		cfg.AuthInfos[clusterName] = ai
		delete(cfg.AuthInfos, "default")
	}
	if cfg.CurrentContext == "default" {
		cfg.CurrentContext = contextName
	}
}

// merge folds incoming into existing. Identical names in
// existing are replaced — this is what kubectl config view
// --flatten does for an overlapping key, and what we want when
// re-provisioning.
func merge(existing, incoming *clientcmdapi.Config) {
	if existing.Contexts == nil {
		existing.Contexts = map[string]*clientcmdapi.Context{}
	}
	if existing.Clusters == nil {
		existing.Clusters = map[string]*clientcmdapi.Cluster{}
	}
	if existing.AuthInfos == nil {
		existing.AuthInfos = map[string]*clientcmdapi.AuthInfo{}
	}
	for k, v := range incoming.Contexts {
		existing.Contexts[k] = v
	}
	for k, v := range incoming.Clusters {
		existing.Clusters[k] = v
	}
	for k, v := range incoming.AuthInfos {
		existing.AuthInfos[k] = v
	}
	if incoming.CurrentContext != "" {
		existing.CurrentContext = incoming.CurrentContext
	}
}

// loadOrEmpty returns the parsed kubeconfig at path or an empty
// config when the file doesn't exist. Other read errors
// propagate so callers don't silently overwrite a corrupted but
// present file.
func loadOrEmpty(path string) (*clientcmdapi.Config, error) {
	cfg, err := clientcmd.LoadFromFile(path)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return clientcmdapi.NewConfig(), nil
		}
		return nil, err
	}
	return cfg, nil
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
		// Best-effort rewrite; if it fails the original file is still
		// valid YAML (kubectl accepts `contexts: null`), just noisy
		// for kubie. Return without raising.
		_ = os.WriteFile(m.Path, []byte(content), 0o600)
	}
}
