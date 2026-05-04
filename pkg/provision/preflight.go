package provision

import (
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"

	"github.com/Yolean/y-cluster/pkg/kubeconfig"
)

// Preflight runs cross-provisioner checks BEFORE any state-mutating
// step in Provision. The point is to fail fast with an actionable
// message ("host port 6443 already bound; change portForwards in
// the config") rather than letting the user discover the conflict
// halfway through a partial provision.
//
// Two classes of check:
//
//   - HostPorts: every entry must currently be free. Empty values
//     skip (provider auto-assigns).
//   - KubeconfigContext: the context name must be either absent or
//     already pointing at clusterName. A second cluster that
//     reuses an existing context name would clobber the first
//     cluster's user/cert and silently break kubectl for it.
//
// Provider-specific checks (qemu's "is the named VM already
// running") layer on top in the per-provider Provision; they're
// not generalisable.
type Preflight struct {
	HostPorts         []string
	ContextName       string
	ContextCluster    string
	KubeconfigPath    string // empty -> kubectl-style env+default search
}

// Run executes every check, accumulating errors so the caller
// sees the full list of conflicts in one go (typical case: the
// developer copy-pasted the existing config and forgot to change
// any of the host-bound identifiers).
func (p Preflight) Run() error {
	var problems []string
	for _, port := range p.HostPorts {
		if err := checkHostPort(port); err != nil {
			problems = append(problems, err.Error())
		}
	}
	if p.ContextName != "" {
		if err := checkKubeconfigContext(p.KubeconfigPath, p.ContextName, p.ContextCluster); err != nil {
			problems = append(problems, err.Error())
		}
	}
	if len(problems) == 0 {
		return nil
	}
	return fmt.Errorf("preflight checks failed:\n  - %s", strings.Join(problems, "\n  - "))
}

// checkHostPort verifies port (a string for cobra-friendliness) is
// not currently bound on 127.0.0.1. Probes by binding briefly and
// closing immediately. Race window is negligible for human-driven
// provisions.
func checkHostPort(port string) error {
	if port == "" {
		return nil // provider auto-assigns
	}
	addr := net.JoinHostPort("127.0.0.1", port)
	l, err := net.Listen("tcp", addr)
	if err != nil {
		return fmt.Errorf("host port %s in use (likely another cluster); change the binding in the config", port)
	}
	_ = l.Close()
	return nil
}

// checkKubeconfigContext returns nil when the context is absent
// or already points at expectedCluster. Otherwise the context
// belongs to a different cluster and re-using it for a new
// provision would orphan that other cluster's kubectl access.
func checkKubeconfigContext(kubeconfigPath, contextName, expectedCluster string) error {
	resolved := resolveKubeconfigPath(kubeconfigPath)
	if resolved == "" {
		return nil // no kubeconfig to read; nothing to clobber
	}
	cfg, err := kubeconfig.Load(resolved)
	if err != nil {
		// File not present is fine; means there's nothing to clobber.
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return fmt.Errorf("load kubeconfig %s: %w", resolved, err)
	}
	existing := cfg.ContextCluster(contextName)
	if existing == "" || existing == expectedCluster {
		return nil
	}
	return fmt.Errorf(
		"kubeconfig context %q already points at cluster %q; "+
			"this provision wants cluster %q. Change `context:` in the config "+
			"to a unique name (convention: equal to `name:`) so kubectl access "+
			"to the existing cluster isn't clobbered",
		contextName, existing, expectedCluster)
}

// resolveKubeconfigPath mirrors the kubectl-style search used by
// pkg/cluster.readClusterName: explicit arg, then $KUBECONFIG
// (first entry of a colon-list), then ~/.kube/config.
func resolveKubeconfigPath(explicit string) string {
	if explicit != "" {
		return explicit
	}
	if env := os.Getenv("KUBECONFIG"); env != "" {
		return strings.SplitN(env, string(os.PathListSeparator), 2)[0]
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(home, ".kube", "config")
}
