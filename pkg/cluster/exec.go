package cluster

import (
	"context"
	"fmt"
	"io"

	"github.com/Yolean/y-cluster/pkg/dockerexec"
	"github.com/Yolean/y-cluster/pkg/multipassexec"
	"github.com/Yolean/y-cluster/pkg/shquote"
	"github.com/Yolean/y-cluster/pkg/sshexec"
)

// RunCtr executes `ctr` on the cluster's node with the given
// args. stdin/stdout/stderr are passthrough so callers can pipe
// large payloads (e.g. `cat archive.tar | y-cluster ctr image
// import`) without buffering.
//
// Routing per backend:
//   - docker:           exec via the Docker daemon API (stdcopy demux);
//     dockerexec.ExitError on non-zero exec exit.
//   - qemu / hetzner:   `sudo k3s ctr <args>` over an x/crypto/ssh session;
//     *ssh.ExitError on non-zero remote exit.
//   - multipass:        `multipass exec <name> -- sudo k3s ctr <args>`;
//     exit status comes from the local multipass CLI.
//
// `ctr` rather than `k3s ctr` for docker because the rancher/k3s
// container image puts ctr on PATH directly. qemu and multipass
// both wrap in `sudo k3s ctr` because that's how the binary is
// surfaced on a k3s VM node.
func RunCtr(ctx context.Context, lr *LookupResult, args []string, stdin io.Reader, stdout, stderr io.Writer) error {
	return runOnNode(ctx, lr, "ctr", args, stdin, stdout, stderr)
}

// RunCrictl is RunCtr's sibling for crictl.
func RunCrictl(ctx context.Context, lr *LookupResult, args []string, stdin io.Reader, stdout, stderr io.Writer) error {
	return runOnNode(ctx, lr, "crictl", args, stdin, stdout, stderr)
}

func runOnNode(ctx context.Context, lr *LookupResult, binary string, args []string, stdin io.Reader, stdout, stderr io.Writer) error {
	return execOnNode(ctx, lr, append([]string{binary}, args...), buildVMNodeRemote(binary, args), stdin, stdout, stderr)
}

// execOnNode is the one place that knows how each backend reaches
// its node. The docker backend's node is a container and takes an
// argv; the VM backends run one command line under the node's shell,
// over ssh or `multipass exec`.
func execOnNode(ctx context.Context, lr *LookupResult, containerArgv []string, vmCommand string, stdin io.Reader, stdout, stderr io.Writer) error {
	switch lr.Backend {
	case BackendDocker:
		cli, err := dockerexec.New()
		if err != nil {
			return fmt.Errorf("docker client: %w", err)
		}
		defer func() { _ = cli.Close() }()
		return dockerexec.Exec(ctx, cli, lr.ContainerName, containerArgv, stdin, stdout, stderr)
	case BackendQEMU, BackendHetzner:
		return sshexec.ExecStream(ctx, sshexec.Target{
			Host: lr.SSHHost, Port: lr.SSHPort,
			User: lr.SSHUser, KeyPath: lr.SSHKey,
		}, vmCommand, stdin, stdout, stderr)
	case BackendMultipass:
		return multipassexec.ExecStream(ctx, lr.MultipassName, vmCommand, stdin, stdout, stderr)
	default:
		return fmt.Errorf("unsupported backend %q", lr.Backend)
	}
}

// buildVMNodeRemote shapes the single command string for the VM
// backends: qemu and hetzner over ssh, multipass over `multipass
// exec`. All run it under sh inside the VM, so args are shell-quoted,
// and k3s puts ctr/crictl behind `sudo k3s`.
func buildVMNodeRemote(binary string, args []string) string {
	remote := "sudo k3s " + binary
	if len(args) > 0 {
		remote += " " + shquote.Join(args)
	}
	return remote
}

// RunShell executes an arbitrary shell command (parsed by `sh -c`)
// on the cluster node. Used by callers that need filesystem writes
// or other ad-hoc operations the ctr/crictl wrappers don't cover --
// the canonical example is `y-cluster manifests add` writing to
// /var/lib/y-cluster/manifests-staging/. The command runs as root
// on the node (sudo on qemu/multipass; the k3s container's user
// is already root for docker).
//
// stdin/stdout/stderr are passthrough so callers can pipe arbitrary
// bytes (manifest YAML on stdin, command output to stdout).
func RunShell(ctx context.Context, lr *LookupResult, cmd string, stdin io.Reader, stdout, stderr io.Writer) error {
	return execOnNode(ctx, lr, []string{"sh", "-c", cmd}, "sudo sh -c "+shquote.Quote(cmd), stdin, stdout, stderr)
}
