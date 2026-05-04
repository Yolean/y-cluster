// Package cluster resolves a kubectl context to a running local
// cluster runtime and exposes helpers to run commands (ctr,
// crictl, raw shell) on the cluster's node.
//
// It replaces ystack's bash trio:
//
//	y-cluster-local-detect  → Lookup
//	y-cluster-local-ctr     → RunCtr
//	y-cluster-local-crictl  → RunCrictl
//
// Discovery is probe-based rather than name-convention-based:
// instead of requiring the kubeconfig cluster to be named
// "ystack-k3d" / "ystack-qemu" / etc., Lookup reads the cluster
// name out of the kubeconfig context, then asks each supported
// backend "is something running with that name?". Docker is
// probed first (cheapest), QEMU second (pidfile lookup). The
// first hit wins.
//
// Supported backends are docker (k3s in a privileged container),
// qemu (k3s in a VM via QEMU/KVM), and multipass (k3s in a
// Multipass-managed VM); ystack's lima / k3d paths are intentionally
// not supported -- y-cluster doesn't provision those, so it can't
// reliably detect them either.
package cluster

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/Yolean/y-cluster/pkg/dockerexec"
	"github.com/Yolean/y-cluster/pkg/kubeconfig"
	"github.com/Yolean/y-cluster/pkg/multipassexec"
)

// DefaultContext is the kubeconfig context name we assume when
// the caller doesn't pass --context. Matches ystack's convention
// and y-cluster's own CommonConfig.Context default ("local").
const DefaultContext = "local"

// Backend identifies the runtime serving the cluster's
// containerd. Add a new constant when adding a new provisioner.
type Backend string

const (
	BackendDocker    Backend = "docker"
	BackendQEMU      Backend = "qemu"
	BackendMultipass Backend = "multipass"
)

// AllBackends is the canonical list of probed backends. Mirrors
// config.AllProviders so call sites that need to enumerate every
// known backend (test helpers, error messages) read from one place
// and a fourth provisioner only edits this slice plus the constants
// above. Sorted alphabetically.
var AllBackends = []Backend{BackendDocker, BackendMultipass, BackendQEMU}

// LookupResult is what Lookup returns when it finds a running
// cluster matching a kubectl context. The Backend-specific
// fields (ContainerName for docker; SSH* for qemu; MultipassName
// for multipass) are populated only when the corresponding
// backend matches.
type LookupResult struct {
	Backend     Backend
	Context     string // kubectl context name we resolved
	ClusterName string // contexts[?].context.cluster from kubeconfig

	// Docker-only.
	ContainerName string

	// QEMU-only.
	SSHKey  string
	SSHHost string
	SSHPort string
	SSHUser string

	// Multipass-only.
	MultipassName string
}

// ErrNotFound is the user-facing error for "the kubeconfig
// context exists but no docker, qemu, or multipass cluster is
// running with that cluster name". Wrapped with the cluster +
// context names so the message is actionable.
var ErrNotFound = errors.New("no running docker, qemu, or multipass cluster matches the kubeconfig context")

// Lookup resolves the kubectl `contextName` (defaults to
// DefaultContext when empty) to a running cluster runtime.
// kubeconfigPath is passed to kubectl as `--kubeconfig`; empty
// means kubectl uses its normal $KUBECONFIG / ~/.kube/config
// search. Currently only docker and qemu backends are probed.
func Lookup(ctx context.Context, kubeconfigPath, contextName string) (*LookupResult, error) {
	if contextName == "" {
		contextName = DefaultContext
	}
	clusterName, err := readClusterName(ctx, kubeconfigPath, contextName)
	if err != nil {
		return nil, err
	}
	if clusterName == "" {
		return nil, fmt.Errorf("kubeconfig context %q not found (or has no cluster set)", contextName)
	}

	running, err := dockerContainerRunning(ctx, clusterName)
	if err != nil {
		// Daemon down, permission denied, etc. Propagate rather
		// than silently falling through to qemu — that fall-
		// through hid real misconfiguration when this was a
		// shell-out.
		return nil, fmt.Errorf("probe docker for %q: %w", clusterName, err)
	}
	if running {
		return &LookupResult{
			Backend:       BackendDocker,
			Context:       contextName,
			ClusterName:   clusterName,
			ContainerName: clusterName,
		}, nil
	}

	if alive, sshKey, sshPort := qemuRunning(clusterName); alive {
		// sshPort comes from the provisioner-written state JSON
		// (<cache>/<name>.json) so a cluster that was provisioned
		// with a non-default sshPort is still reachable. Falling
		// back to "2222" -- the qemu provisioner's hardcoded
		// default in pkg/provision/config.QEMUConfig -- only
		// matters for old caches written before sshPort landed
		// in the sidecar; current provisions always include it.
		// SSHUser is hardcoded because cloud-init's user-data
		// template (pkg/provision/qemu/qemu.go renderCloudInitUserData)
		// only ever creates `ystack`. SSHHost is hardcoded
		// because qemu always binds host-side port forwards to
		// 127.0.0.1.
		if sshPort == "" {
			sshPort = "2222"
		}
		return &LookupResult{
			Backend:     BackendQEMU,
			Context:     contextName,
			ClusterName: clusterName,
			SSHKey:      sshKey,
			SSHHost:     "127.0.0.1",
			SSHPort:     sshPort,
			SSHUser:     "ystack",
		}, nil
	}

	if multipassRunning(ctx, clusterName) {
		return &LookupResult{
			Backend:       BackendMultipass,
			Context:       contextName,
			ClusterName:   clusterName,
			MultipassName: clusterName,
		}, nil
	}

	return nil, fmt.Errorf("%w (cluster=%q, context=%q)", ErrNotFound, clusterName, contextName)
}

// multipassRunning reports whether a Multipass VM with the given
// name exists and is in the Running state. Probed last in Lookup
// because it's a CLI shellout (slower than the docker daemon API
// and the qemu pidfile read), but still fast enough for an
// interactive command (~50 ms when multipass is installed; the
// short-circuit in multipassexec.Reachable keeps cost zero on
// hosts without multipass).
func multipassRunning(ctx context.Context, name string) bool {
	if !multipassexec.Reachable(2 * time.Second) {
		return false
	}
	probeCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	running, err := multipassexec.IsRunning(probeCtx, name)
	if err != nil {
		return false
	}
	return running
}

// ResolveClusterName resolves contextName to its cluster entry
// name in the kubeconfig. Empty kubeconfigPath uses the
// kubectl-style search ($KUBECONFIG, then ~/.kube/config). Returns
// "" (no error) when the context does not exist.
//
// Used by Lookup, but also exported for `y-cluster start` which
// needs the cluster name without first finding a running cluster
// (the cluster is, by definition, stopped at start time).
func ResolveClusterName(kubeconfigPath, contextName string) (string, error) {
	return readClusterName(context.TODO(), kubeconfigPath, contextName)
}

// readClusterName resolves contextName to its cluster entry name
// in the kubeconfig. Empty kubeconfigPath uses the kubectl-style
// search ($KUBECONFIG, then ~/.kube/config). Returns "" (no error)
// when the context does not exist. Implemented on the typed File
// schema in pkg/kubeconfig (no client-go).
func readClusterName(_ context.Context, kubeconfigPath, contextName string) (string, error) {
	resolved := kubeconfigPath
	if resolved == "" {
		if env := os.Getenv("KUBECONFIG"); env != "" {
			// Take the first entry for KUBECONFIG=a:b. The full
			// merge semantics clientcmd implements aren't relevant
			// here -- detect/ctr/crictl resolve a context name
			// against ONE kubeconfig, the same one provision wrote.
			resolved = strings.SplitN(env, string(os.PathListSeparator), 2)[0]
		}
	}
	if resolved == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", fmt.Errorf("locate home: %w", err)
		}
		resolved = filepath.Join(home, ".kube", "config")
	}
	cfg, err := kubeconfig.Load(resolved)
	if err != nil {
		return "", fmt.Errorf("load kubeconfig: %w", err)
	}
	return cfg.ContextCluster(contextName), nil
}

// dockerContainerRunning asks the local Docker daemon whether
// `name` is a running container. Returns (false, nil) when the
// container does not exist (legitimately "not docker"), and
// (_, err) for daemon-level failures the caller must surface
// (daemon down, socket perms, API mismatch).
func dockerContainerRunning(ctx context.Context, name string) (bool, error) {
	cli, err := dockerexec.New()
	if err != nil {
		return false, fmt.Errorf("docker client: %w", err)
	}
	defer func() { _ = cli.Close() }()
	return dockerexec.IsRunning(ctx, cli, name)
}

// qemuRunning checks the qemu provisioner's pidfile convention:
// <cache-dir>/<name>.pid contains a live PID. The cache dir is
// $Y_CLUSTER_QEMU_CACHE_DIR when set, else ~/.cache/y-cluster-qemu --
// matching qemu.FromConfig's default. The env override exists so
// e2e tests can run an isolated cluster under t.TempDir() and
// still have detect/ctr/crictl find it.
//
// Returns (true, sshKeyPath, sshPort) on a hit; sshPort is read
// from the provisioner-written sidecar <cache>/<name>.json so
// callers can reach a cluster that was provisioned with a
// non-default port. sshPort is "" if the sidecar is missing or
// has no sshPort field -- caller falls back to the qemu
// provisioner's default.
//
// Returns (false, "", "") on no-hit.
func qemuRunning(name string) (bool, string, string) {
	cacheDir := os.Getenv("Y_CLUSTER_QEMU_CACHE_DIR")
	if cacheDir == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return false, "", ""
		}
		cacheDir = filepath.Join(home, ".cache", "y-cluster-qemu")
	}
	pidPath := filepath.Join(cacheDir, name+".pid")
	data, err := os.ReadFile(pidPath)
	if err != nil {
		return false, "", ""
	}
	var pid int
	if _, err := fmt.Sscanf(strings.TrimSpace(string(data)), "%d", &pid); err != nil {
		return false, "", ""
	}
	if !pidAlive(pid) {
		return false, "", ""
	}
	sshKey := filepath.Join(cacheDir, name+"-ssh")
	sshPort := readQemuStateSSHPort(filepath.Join(cacheDir, name+".json"))
	return true, sshKey, sshPort
}

// readQemuStateSSHPort reads the `sshPort` field out of the qemu
// provisioner's state sidecar at the given path. Returns "" on
// any failure (missing file, bad JSON, no field) -- the caller
// is expected to fall back to a hardcoded default in that case.
// We only care about one field, so we don't import the qemu
// package's full state struct (which would risk an import cycle
// since qemu imports cluster).
func readQemuStateSSHPort(path string) string {
	data, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	var s struct {
		SSHPort string `json:"sshPort"`
	}
	if err := json.Unmarshal(data, &s); err != nil {
		return ""
	}
	return s.SSHPort
}

// pidAlive is the stdlib equivalent of `kill -0 <pid>`.
// signal(0) does the kernel's existence + permission check
// without delivering anything. ESRCH = "no such process",
// EPERM = "process exists but we can't signal it" (treated as
// alive because the existence check is what we care about).
func pidAlive(pid int) bool {
	if pid <= 0 {
		return false
	}
	proc, err := os.FindProcess(pid)
	if err != nil {
		return false
	}
	err = proc.Signal(syscall.Signal(0))
	if err == nil {
		return true
	}
	if errors.Is(err, os.ErrProcessDone) || errors.Is(err, syscall.ESRCH) {
		return false
	}
	if errors.Is(err, syscall.EPERM) {
		return true
	}
	return false
}
