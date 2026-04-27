package cluster

import (
	"context"
	"fmt"
	"io"
	"strings"

	"github.com/Yolean/y-cluster/pkg/dockerexec"
	"github.com/Yolean/y-cluster/pkg/sshexec"
)

// RunCtr executes `ctr` on the cluster's node with the given
// args. stdin/stdout/stderr are passthrough so callers can pipe
// large payloads (e.g. `cat archive.tar | y-cluster ctr image
// import`) without buffering.
//
// Routing per backend:
//   - docker: exec via the Docker daemon API (stdcopy demux);
//     dockerexec.ExitError on non-zero exec exit.
//   - qemu:   `sudo k3s ctr <args>` over an x/crypto/ssh session;
//     *ssh.ExitError on non-zero remote exit.
//
// `ctr` rather than `k3s ctr` for docker because the rancher/k3s
// container image puts ctr on PATH directly.
func RunCtr(ctx context.Context, lr *LookupResult, args []string, stdin io.Reader, stdout, stderr io.Writer) error {
	return runOnNode(ctx, lr, "ctr", args, stdin, stdout, stderr)
}

// RunCrictl is RunCtr's sibling for crictl.
func RunCrictl(ctx context.Context, lr *LookupResult, args []string, stdin io.Reader, stdout, stderr io.Writer) error {
	return runOnNode(ctx, lr, "crictl", args, stdin, stdout, stderr)
}

func runOnNode(ctx context.Context, lr *LookupResult, binary string, args []string, stdin io.Reader, stdout, stderr io.Writer) error {
	switch lr.Backend {
	case BackendDocker:
		cli, err := dockerexec.New()
		if err != nil {
			return fmt.Errorf("docker client: %w", err)
		}
		defer func() { _ = cli.Close() }()
		return dockerexec.Exec(ctx, cli, lr.ContainerName,
			append([]string{binary}, args...),
			stdin, stdout, stderr)
	case BackendQEMU:
		return sshexec.ExecStream(ctx, sshexec.Target{
			Host: lr.SSHHost, Port: lr.SSHPort,
			User: lr.SSHUser, KeyPath: lr.SSHKey,
		}, buildQemuRemote(binary, args), stdin, stdout, stderr)
	default:
		return fmt.Errorf("unsupported backend %q", lr.Backend)
	}
}

// buildQemuRemote shapes the single-string command sshexec.ExecStream
// passes as the remote command. On a k3s VM, ctr/crictl live under
// `k3s` so we always wrap in `sudo k3s <binary>`. Args are
// shell-quoted because ssh executes the string under /bin/sh.
func buildQemuRemote(binary string, args []string) string {
	return "sudo k3s " + binary + shellQuoteJoin(args)
}

// shellQuoteJoin shell-quotes each arg with single quotes (POSIX-
// safe) and joins with leading spaces. Empty `args` returns "".
// Single quotes inside an arg become `'\''` per the standard
// trick — closing the quoted string, escaping a literal quote,
// reopening.
func shellQuoteJoin(args []string) string {
	if len(args) == 0 {
		return ""
	}
	var b strings.Builder
	for _, a := range args {
		b.WriteByte(' ')
		b.WriteByte('\'')
		b.WriteString(strings.ReplaceAll(a, "'", `'\''`))
		b.WriteByte('\'')
	}
	return b.String()
}
