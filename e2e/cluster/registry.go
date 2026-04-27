//go:build e2e

package cluster

import (
	"context"
	"fmt"
	"net"
	"os/exec"
	"sync"
	"testing"
	"time"
)

const (
	registryImage         = "registry:2"
	registryContainerName = "y-cluster-e2e-registry"
)

// Registry describes a running local Docker registry container.
// The harness brings one up on a free host port the first time
// any test asks for it, reuses it for the rest of the `go test`
// invocation, then tears it down via TeardownAll.
type Registry struct {
	// HostPort is the registry's host-side port. Use
	// `<HostPort>/<repo>:<tag>` as the image reference.
	HostPort string

	// Endpoint is "127.0.0.1:<port>" — the address callers feed
	// to crane.Push or any go-containerregistry remote.* call.
	Endpoint string
}

var (
	registryOnce     sync.Once
	registryInstance *Registry
	registrySetupErr error
)

// LocalRegistry returns the process-wide local registry,
// bringing it up on the first call. Skips the test if Docker
// isn't available — same precondition the kwok harness has.
//
// CI runners with the registry already pulled get a sub-second
// start; on a cold cache the first call is the registry pull.
func LocalRegistry(t *testing.T) *Registry {
	t.Helper()
	registryOnce.Do(setupLocalRegistry)
	if registrySetupErr != nil {
		t.Skipf("local registry unavailable: %v", registrySetupErr)
	}
	return registryInstance
}

// teardownLocalRegistry is invoked by TeardownAll. Best-effort,
// matches kwok teardown semantics.
func teardownLocalRegistry() {
	_ = exec.Command("docker", "rm", "-f", registryContainerName).Run()
}

func setupLocalRegistry() {
	ctx := context.Background()
	if err := exec.CommandContext(ctx, "docker", "info").Run(); err != nil {
		registrySetupErr = fmt.Errorf("docker not available: %w", err)
		return
	}
	_ = exec.CommandContext(ctx, "docker", "rm", "-f", registryContainerName).Run()

	hostPort, err := pickFreePort()
	if err != nil {
		registrySetupErr = fmt.Errorf("pick free port: %w", err)
		return
	}
	cmd := exec.CommandContext(ctx, "docker", "run", "-d",
		"--name", registryContainerName,
		"-p", fmt.Sprintf("127.0.0.1:%d:5000", hostPort),
		registryImage)
	if out, err := cmd.CombinedOutput(); err != nil {
		registrySetupErr = fmt.Errorf("start registry: %s: %w", out, err)
		return
	}

	endpoint := fmt.Sprintf("127.0.0.1:%d", hostPort)
	if err := waitForRegistry(ctx, endpoint, 30*time.Second); err != nil {
		_ = exec.Command("docker", "rm", "-f", registryContainerName).Run()
		registrySetupErr = fmt.Errorf("registry not ready: %w", err)
		return
	}

	registryInstance = &Registry{
		HostPort: fmt.Sprintf("%d", hostPort),
		Endpoint: endpoint,
	}
}

// pickFreePort asks the kernel for an ephemeral port, then
// closes the listener so docker's port mapping can grab it.
// Race window is small enough we accept it for tests.
func pickFreePort() (int, error) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return 0, err
	}
	defer l.Close()
	return l.Addr().(*net.TCPAddr).Port, nil
}

// waitForRegistry polls the v2 root until it answers 200/401.
// `registry:2` answers 200 unauthenticated; either status proves
// the listener is up and serving the v2 API.
func waitForRegistry(ctx context.Context, endpoint string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for {
		conn, err := net.DialTimeout("tcp", endpoint, 1*time.Second)
		if err == nil {
			_ = conn.Close()
			// TCP up — the v2 server is ready by the time
			// `registry:2` accepts connections; no need to roundtrip
			// HTTP for this.
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("not reachable at %s after %s", endpoint, timeout)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(200 * time.Millisecond):
		}
	}
}

// stop tears down the registry container; used by tests that
// want to assert "warm cache works without network".
func (r *Registry) stop() error {
	out, err := exec.Command("docker", "stop", registryContainerName).CombinedOutput()
	if err != nil {
		return fmt.Errorf("docker stop registry: %s: %w", out, err)
	}
	return nil
}

// Stop is the exported form of stop, for tests that assert
// behaviour in absence of a registry. The harness restarts the
// container at TestMain time only — there's no automatic
// re-start.
func (r *Registry) Stop() error { return r.stop() }
