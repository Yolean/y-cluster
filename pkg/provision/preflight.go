package provision

import (
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/Yolean/y-cluster/pkg/inventory"
	"github.com/Yolean/y-cluster/pkg/kubeconfig"
)

// PortBinder names the process that ends up binding the host ports,
// which is what decides how to read a bind probe that comes back
// "permission denied".
type PortBinder int

const (
	// PortBinderDaemon: a privileged daemon binds the host port and
	// hands the socket to an unprivileged process -- dockerd, or
	// Docker Desktop's root helper passing the fd to
	// com.docker.backend. y-cluster's own privileges have no bearing
	// on whether that bind succeeds, so a refused probe is evidence
	// of nothing.
	PortBinderDaemon PortBinder = iota

	// PortBinderSelf: the provisioner spawns the binding process as
	// this user -- qemu's `-netdev user,hostfwd=tcp::80-:80` binds
	// the host side in-process. Here the probe is exactly the bind
	// the provision will attempt, so a refusal is a real blocker
	// worth failing fast on.
	PortBinderSelf
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
//     skip (provider auto-assigns). PortBinder says who does the
//     binding, which is what makes an unbindable privileged port
//     either a hard blocker or none of our business.
//   - KubeconfigContext: the context name must be either absent or
//     already pointing at clusterName. A second cluster that
//     reuses an existing context name would clobber the first
//     cluster's user/cert and silently break kubectl for it.
//
// Provider-specific checks (qemu's "is the named VM already
// running") layer on top in the per-provider Provision; they're
// not generalisable.
type Preflight struct {
	HostPorts      []string
	PortBinder     PortBinder
	ContextName    string
	ContextCluster string
	KubeconfigPath string // empty -> kubectl-style env+default search
}

// Run executes every check, accumulating errors so the caller
// sees the full list of conflicts in one go (typical case: the
// developer copy-pasted the existing config and forgot to change
// any of the host-bound identifiers).
func (p Preflight) Run() error {
	var problems []string
	for _, port := range p.HostPorts {
		if err := checkHostPort(port, p.PortBinder); err != nil {
			problems = append(problems, attributePort(err))
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

// hostPortDialTimeout bounds the fallback connect probe. The target
// is loopback, so anything that hasn't answered by then isn't going
// to.
const hostPortDialTimeout = 250 * time.Millisecond

// attributePort upgrades the port-in-use outcome to name the
// cluster holding the port, when the host inventory has a record
// binding it. "port 26443 in use" leaves the user hunting for
// what to tear down; the inventory knows the -c path. Every other
// checkHostPort error (probe failure, privilege guidance) passes
// through untouched, as does in-use with no matching record
// (older binary, out-of-band process).
func attributePort(err error) string {
	var inUse portInUseError
	if !errors.As(err, &inUse) {
		return err.Error()
	}
	rec := inventory.FindByHostPort(inUse.port)
	if rec == nil {
		return err.Error()
	}
	return fmt.Sprintf(
		"host port %s in use by y-cluster %q (context %q); tear it down with: y-cluster teardown -c %s",
		inUse.port, rec.Name, rec.Context, rec.ConfigDir)
}

// checkHostPort verifies port (a string for cobra-friendliness) is
// free for the provider to bind. Probes by binding briefly and
// closing immediately. Race window is negligible for human-driven
// provisions.
//
// The authoritative probe is against the IPv4 wildcard, because
// that is what both providers bind: docker sets HostIP to 0.0.0.0,
// and qemu's `hostfwd=tcp::<port>-` leaves the host address empty,
// which slirp reads as 0.0.0.0.
//
// Go sets SO_REUSEADDR on every listener, and on BSD that lets a
// wildcard and a loopback bind of one port coexist, so neither
// address alone sees every conflict. A second, loopback probe
// covers the other half: a listener on 127.0.0.1 doesn't block the
// provider's wildcard bind, but it does take the loopback traffic
// the cluster is reached on. Only EADDRINUSE counts there --
// Darwin refuses every loopback bind under port 1024 whether or
// not the port is free, because XNU skips the reserved-port check
// for INADDR_ANY only.
//
// Ports below 1024 need privilege to bind on Linux (and on Darwin
// off the wildcard), which the probe usually lacks, so "bind
// refused" and "port taken" are different answers: EACCES means
// "can't tell from here", not "in use". What's left is the weaker
// question any user may ask -- is something accepting connections
// there -- plus, under PortBinderSelf, an error that names the
// privilege as the problem instead of blaming another cluster.
func checkHostPort(port string, binder PortBinder) error {
	if port == "" {
		return nil // provider auto-assigns
	}
	l, err := net.Listen("tcp", net.JoinHostPort("0.0.0.0", port))
	switch {
	case err == nil:
		_ = l.Close()
		if lo, loErr := net.Listen("tcp", net.JoinHostPort("127.0.0.1", port)); loErr == nil {
			_ = lo.Close()
		} else if errors.Is(loErr, syscall.EADDRINUSE) {
			return errHostPortInUse(port)
		}
		return nil
	case errors.Is(err, syscall.EADDRINUSE):
		return errHostPortInUse(port)
	case !errors.Is(err, os.ErrPermission):
		return fmt.Errorf("host port %s: bind probe failed: %w", port, err)
	}
	// A privileged port can't be bind-probed, but a wildcard
	// listener on it still answers on loopback.
	if hostPortAnswers(net.JoinHostPort("127.0.0.1", port)) {
		return errHostPortInUse(port)
	}
	if binder == PortBinderSelf {
		return fmt.Errorf(
			"host port %s: nothing is listening, but this user may not bind a "+
				"privileged port and the provider binds host ports as you. Run as "+
				"root, grant the binary CAP_NET_BIND_SERVICE, or map the forward to "+
				"a host port above 1023 in the config", port)
	}
	return nil
}

// portInUseError is the typed "genuinely taken" outcome, kept
// distinct so attributePort can upgrade exactly this case with
// inventory ownership and leave the other probe verdicts alone.
type portInUseError struct{ port string }

func (e portInUseError) Error() string {
	return fmt.Sprintf("host port %s in use (likely another cluster); change the binding in the config", e.port)
}

func errHostPortInUse(port string) error {
	return portInUseError{port: port}
}

// hostPortAnswers reports whether something is accepting TCP
// connections on addr. Weaker than the bind probe -- a listener with
// a full backlog, or one bound to a single non-loopback interface,
// escapes it -- but it needs no privilege, so it is the only probe
// left once the bind is refused.
func hostPortAnswers(addr string) bool {
	c, err := net.DialTimeout("tcp", addr, hostPortDialTimeout)
	if err != nil {
		return false
	}
	_ = c.Close()
	return true
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
