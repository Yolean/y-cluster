// Package multipassexec wraps the `multipass` CLI so packages that
// talk to a Multipass-managed VM (the multipass provisioner, the
// runtime cluster discovery + ctr/crictl routing) share one
// implementation rather than re-spelling the shellouts.
//
// Sibling to pkg/dockerexec and pkg/sshexec: each backend has one
// thin helper package and consumers (provisioning, detect, image
// load) layer on top.
package multipassexec

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"strings"
	"time"
)

// Run invokes `multipass <args...>` with the given stdin (nil ok)
// and returns combined stdout+stderr. Errors carry the captured
// output verbatim so the operator sees the CLI's diagnostic.
func Run(ctx context.Context, stdin io.Reader, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, "multipass", args...)
	if stdin != nil {
		cmd.Stdin = stdin
	}
	var buf bytes.Buffer
	cmd.Stdout = &buf
	cmd.Stderr = &buf
	if err := cmd.Run(); err != nil {
		return buf.Bytes(), fmt.Errorf("multipass %s: %w", strings.Join(args, " "), err)
	}
	return buf.Bytes(), nil
}

// Exec runs `command` inside the named VM via
// `multipass exec <name> -- sh -c <command>`. stdin (when non-nil)
// is piped to the remote process. Returns combined stdout+stderr.
func Exec(ctx context.Context, name, command string, stdin io.Reader) ([]byte, error) {
	return Run(ctx, stdin, "exec", name, "--", "sh", "-c", command)
}

// ExecStream is Exec's streaming variant: stdin/stdout/stderr are
// passed through unbuffered. Used by image load and ctr/crictl
// routing where output volume is large enough that combined-buffer
// capture would be wasteful.
func ExecStream(ctx context.Context, name, command string, stdin io.Reader, stdout, stderr io.Writer) error {
	cmd := exec.CommandContext(ctx, "multipass", "exec", name, "--", "sh", "-c", command)
	cmd.Stdin = stdin
	cmd.Stdout = stdout
	cmd.Stderr = stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("multipass exec %s: %w", name, err)
	}
	return nil
}

// Transfer copies a host file into the named VM via
// `multipass transfer <local> <name>:<remote>`.
func Transfer(ctx context.Context, name, localPath, remotePath string) error {
	out, err := Run(ctx, nil, "transfer", localPath, name+":"+remotePath)
	if err != nil {
		return fmt.Errorf("transfer %s -> %s:%s: %s: %w", localPath, name, remotePath, out, err)
	}
	return nil
}

// VMInfo is the slice of `multipass info <name> --format json` we
// care about. The CLI emits other keys (release, mounts, memory,
// disk_usage); we ignore them.
type VMInfo struct {
	State string   `json:"state"`
	IPv4  []string `json:"ipv4"`
}

// Info returns the parsed info for a single VM. Returns
// (nil, ErrNotFound) when the VM does not exist (multipass exits
// non-zero with a recognizable message), so callers can treat
// "absent" as a normal state during teardown / preflight.
func Info(ctx context.Context, name string) (*VMInfo, error) {
	out, err := Run(ctx, nil, "info", name, "--format", "json")
	if err != nil {
		if isNotFoundOutput(out) {
			return nil, ErrNotFound
		}
		return nil, fmt.Errorf("multipass info %s: %s: %w", name, out, err)
	}
	var doc struct {
		Info map[string]VMInfo `json:"info"`
	}
	if err := json.Unmarshal(out, &doc); err != nil {
		return nil, fmt.Errorf("parse multipass info %s: %w (output: %s)", name, err, out)
	}
	v, ok := doc.Info[name]
	if !ok {
		return nil, ErrNotFound
	}
	return &v, nil
}

// FirstIPv4 returns the first non-empty IPv4 entry from info, or
// "" when the slice has no usable entry.
func FirstIPv4(info *VMInfo) string {
	for _, ip := range info.IPv4 {
		if ip != "" {
			return ip
		}
	}
	return ""
}

// IsRunning reports whether the named VM exists and reports a
// `Running` state. Used by pkg/cluster.Lookup as the multipass
// existence probe.
func IsRunning(ctx context.Context, name string) (bool, error) {
	info, err := Info(ctx, name)
	if errors.Is(err, ErrNotFound) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return strings.EqualFold(info.State, "running"), nil
}

// ErrNotFound is the sentinel Info / Stop / Delete return when the
// VM is absent. Teardown uses errors.Is to treat it as success.
var ErrNotFound = errors.New("multipass: VM not found")

// isNotFoundOutput recognises the various phrasings multipass uses
// for "no such VM". The exact text has shifted across releases;
// matching on substrings keeps the check robust without us pinning
// a multipass version.
func isNotFoundOutput(out []byte) bool {
	s := strings.ToLower(string(out))
	switch {
	case strings.Contains(s, "does not exist"):
		return true
	case strings.Contains(s, "instance does not exist"):
		return true
	case strings.Contains(s, "unknown instance"):
		return true
	case strings.Contains(s, "not found"):
		return true
	}
	return false
}

// Stop wraps `multipass stop <name>`. ErrNotFound is returned for
// missing VMs so callers can treat that as success.
func Stop(ctx context.Context, name string) error {
	out, err := Run(ctx, nil, "stop", name)
	if err != nil {
		if isNotFoundOutput(out) {
			return ErrNotFound
		}
		return fmt.Errorf("multipass stop %s: %s: %w", name, out, err)
	}
	return nil
}

// Delete wraps `multipass delete <name>`. Pass purge=true to also
// run `multipass purge` afterwards (drops the recoverable state).
// ErrNotFound is returned when the VM is missing.
func Delete(ctx context.Context, name string, purge bool) error {
	out, err := Run(ctx, nil, "delete", name)
	if err != nil {
		if isNotFoundOutput(out) {
			return ErrNotFound
		}
		return fmt.Errorf("multipass delete %s: %s: %w", name, out, err)
	}
	if purge {
		if pout, perr := Run(ctx, nil, "purge"); perr != nil {
			return fmt.Errorf("multipass purge: %s: %w", pout, perr)
		}
	}
	return nil
}

// Version runs `multipass version` to verify the CLI is installed
// and the daemon is reachable.
func Version(ctx context.Context) error {
	out, err := Run(ctx, nil, "version")
	if err != nil {
		return fmt.Errorf("multipass version: %s: %w", out, err)
	}
	return nil
}

// Reachable reports whether `multipass version` returns
// successfully within timeout. Short-circuits when the binary
// isn't on PATH so callers don't pay the daemon round-trip on
// hosts where multipass isn't installed.
func Reachable(timeout time.Duration) bool {
	if _, err := exec.LookPath("multipass"); err != nil {
		return false
	}
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	return Version(ctx) == nil
}
