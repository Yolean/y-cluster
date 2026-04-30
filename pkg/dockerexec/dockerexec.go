// Package dockerexec wraps the Docker daemon API
// (github.com/moby/moby/client) for the y-cluster paths that
// need to run, exec into, remove, or read logs from a container.
//
// The point is error categorisation, not "bring your own
// docker". Same daemon socket as the docker CLI, just with
// machine-readable errors:
//
//   cerrdefs.IsNotFound        → container missing
//   cerrdefs.IsConflict        → name in use
//   cerrdefs.IsPermissionDenied → socket perms / rootless misconfig
//   net.OpError                → daemon down (no such file/socket)
//
// We share the wrapper between the docker provisioner (which
// runs/removes the container) and pkg/cluster (which exec's
// into a container Lookup found). Without this both packages
// would duplicate the moby/client + stdcopy demux dance.
package dockerexec

import (
	"context"
	"fmt"
	"io"

	cerrdefs "github.com/containerd/errdefs"
	"github.com/moby/moby/api/pkg/stdcopy"
	"github.com/moby/moby/client"
)

// New opens a daemon connection. Honors $DOCKER_HOST and friends
// via client.FromEnv; negotiates the API version with the daemon
// so we don't pin a server-side level.
func New() (*client.Client, error) {
	// API-version negotiation is the moby client default since the
	// option got deprecated as a no-op; we don't pass it explicitly.
	return client.New(client.FromEnv)
}

// Remove force-removes the named container. NotFound is treated
// as success because the operation is idempotent and that's how
// every caller wants it.
func Remove(ctx context.Context, cli *client.Client, name string) error {
	_, err := cli.ContainerRemove(ctx, name, client.ContainerRemoveOptions{Force: true})
	if err != nil && !cerrdefs.IsNotFound(err) {
		return fmt.Errorf("remove %s: %w", name, err)
	}
	return nil
}

// PullIfMissing fetches the named image from its registry when the
// daemon doesn't already have it. ImageInspect returns a NotFound
// error when the image is absent; that's the only case we pull,
// keeping warm caches free of round trips. Errors during the pull
// surface via ImagePullResponse.Wait so a flaky registry doesn't
// silently produce a half-resolved image.
//
// `docker run` auto-pulls; ContainerCreate doesn't. The docker
// provisioner is the only ContainerCreate caller in y-cluster, but
// the helper lives here so anything else that takes the daemon-API
// path inherits the same behaviour.
func PullIfMissing(ctx context.Context, cli *client.Client, image string) error {
	if _, err := cli.ImageInspect(ctx, image); err == nil {
		return nil
	} else if !cerrdefs.IsNotFound(err) {
		return fmt.Errorf("inspect %s: %w", image, err)
	}
	resp, err := cli.ImagePull(ctx, image, client.ImagePullOptions{})
	if err != nil {
		return fmt.Errorf("pull %s: %w", image, err)
	}
	defer resp.Close()
	if err := resp.Wait(ctx); err != nil {
		return fmt.Errorf("pull %s: %w", image, err)
	}
	return nil
}

// IsRunning reports whether the container exists and is in the
// Running state. Returns (false, nil) for "not found"; only
// daemon-level errors propagate.
func IsRunning(ctx context.Context, cli *client.Client, name string) (bool, error) {
	res, err := cli.ContainerInspect(ctx, name, client.ContainerInspectOptions{})
	if err != nil {
		if cerrdefs.IsNotFound(err) {
			return false, nil
		}
		return false, fmt.Errorf("inspect %s: %w", name, err)
	}
	return res.Container.State != nil && res.Container.State.Running, nil
}

// Logs returns the most recent `tail` lines of `name`'s log,
// stdout and stderr concatenated. Used for diagnostics when
// other operations time out.
func Logs(ctx context.Context, cli *client.Client, name string, tail string) ([]byte, error) {
	rc, err := cli.ContainerLogs(ctx, name, client.ContainerLogsOptions{
		ShowStdout: true,
		ShowStderr: true,
		Tail:       tail,
	})
	if err != nil {
		return nil, fmt.Errorf("logs %s: %w", name, err)
	}
	defer rc.Close()
	return demux(rc)
}

// Exec runs cmd inside the container with stdin/stdout/stderr
// passthrough — the same shape exec.Cmd has, so callers that
// previously shelled out to `docker exec` switch with minimal
// friction. Returns the exec's exit code via *ExitError when
// non-zero so callers can categorise.
func Exec(ctx context.Context, cli *client.Client, name string, cmd []string, stdin io.Reader, stdout, stderr io.Writer) error {
	create, err := cli.ExecCreate(ctx, name, client.ExecCreateOptions{
		Cmd:          cmd,
		AttachStdin:  stdin != nil,
		AttachStdout: stdout != nil,
		AttachStderr: stderr != nil,
	})
	if err != nil {
		return fmt.Errorf("exec create %s: %w", name, err)
	}
	att, err := cli.ExecAttach(ctx, create.ID, client.ExecAttachOptions{})
	if err != nil {
		return fmt.Errorf("exec attach %s: %w", name, err)
	}
	defer att.Close()

	// Pump stdin (if any) and demux stdout/stderr in parallel.
	// stdin returns when the writer side closes, so we tear it
	// down explicitly via CloseWrite once the caller-side reader
	// EOFs.
	errCh := make(chan error, 2)
	if stdin != nil {
		go func() {
			_, copyErr := io.Copy(att.Conn, stdin)
			_ = att.CloseWrite()
			errCh <- copyErr
		}()
	} else {
		errCh <- nil
	}
	go func() {
		errCh <- demuxTo(att.Reader, stdout, stderr)
	}()

	var firstErr error
	for i := 0; i < 2; i++ {
		if e := <-errCh; e != nil && firstErr == nil {
			firstErr = e
		}
	}
	if firstErr != nil {
		return fmt.Errorf("exec stream %s: %w", name, firstErr)
	}

	// Inspect the exec to surface the remote exit code.
	insp, err := cli.ExecInspect(ctx, create.ID, client.ExecInspectOptions{})
	if err != nil {
		return fmt.Errorf("exec inspect %s: %w", name, err)
	}
	if insp.ExitCode != 0 {
		return &ExitError{ExitCode: insp.ExitCode, Cmd: cmd}
	}
	return nil
}

// ExitError carries a non-zero exec exit code so callers can
// errors.As() and decide whether to retry / surface verbatim /
// fail the test. Modeled on exec.ExitError for familiarity.
type ExitError struct {
	ExitCode int
	Cmd      []string
}

func (e *ExitError) Error() string {
	return fmt.Sprintf("docker exec %v: exit code %d", e.Cmd, e.ExitCode)
}

func demux(rc io.Reader) ([]byte, error) {
	var stdout, stderr writableBuffer
	if err := demuxTo(rc, &stdout, &stderr); err != nil {
		return nil, err
	}
	// Mirror the previous CombinedOutput semantics: stdout +
	// stderr concatenated.
	out := append(stdout.b, stderr.b...)
	return out, nil
}

func demuxTo(rc io.Reader, stdout, stderr io.Writer) error {
	if stdout == nil {
		stdout = io.Discard
	}
	if stderr == nil {
		stderr = io.Discard
	}
	_, err := stdcopy.StdCopy(stdout, stderr, rc)
	return err
}

// writableBuffer is a tiny io.Writer-bytes.Buffer hybrid used by
// demux to avoid pulling bytes.Buffer into the docs of the
// public API — kept private so callers can't hold onto it.
type writableBuffer struct{ b []byte }

func (w *writableBuffer) Write(p []byte) (int, error) {
	w.b = append(w.b, p...)
	return len(p), nil
}
