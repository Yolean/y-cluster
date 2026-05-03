package docker

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"go.uber.org/zap"

	"github.com/Yolean/y-cluster/pkg/provision/config"
)

// fakeKubectlOnPATH writes an executable shell script named `kubectl`
// to a fresh temp dir and prepends that dir to $PATH for the test.
// pollHostAPIServerReadyz exec's `kubectl` by name, so the resolved
// binary is the script rather than any real kubectl on the system.
// `body` is the shell body (no shebang); use `exit 0` for the
// success case and `exit 1` (with a stderr message) for failure.
func fakeKubectlOnPATH(t *testing.T, body string) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("fake kubectl shim is /bin/sh-only")
	}
	dir := t.TempDir()
	script := "#!/bin/sh\n" + body + "\n"
	if err := os.WriteFile(filepath.Join(dir, "kubectl"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
}

func newProbeTestCluster() *Cluster {
	return &Cluster{
		cfg: config.DockerConfig{
			CommonConfig: config.CommonConfig{Context: "unit-test-ctx"},
		},
		logger: zap.NewNop(),
	}
}

// First-call success: a kubectl that exits 0 returns nil immediately.
func TestPollHostAPIServerReadyz_Success(t *testing.T) {
	fakeKubectlOnPATH(t, "exit 0")
	c := newProbeTestCluster()
	if err := c.pollHostAPIServerReadyz(context.Background(), time.Second, 10*time.Millisecond); err != nil {
		t.Fatalf("expected success, got: %v", err)
	}
}

// Always-failing kubectl: deadline trips, the wrapped "never returned
// 200" error is returned (not ctx.Err()) and carries the context name.
func TestPollHostAPIServerReadyz_DeadlineHonored(t *testing.T) {
	fakeKubectlOnPATH(t, `echo 'connection refused' >&2; exit 1`)
	c := newProbeTestCluster()
	start := time.Now()
	err := c.pollHostAPIServerReadyz(context.Background(), 100*time.Millisecond, 20*time.Millisecond)
	elapsed := time.Since(start)
	if err == nil {
		t.Fatal("expected deadline error, got nil")
	}
	if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) {
		t.Fatalf("expected wrapped readiness error, got ctx error: %v", err)
	}
	if !strings.Contains(err.Error(), "/readyz never returned 200") {
		t.Fatalf("expected readiness deadline message, got: %v", err)
	}
	if !strings.Contains(err.Error(), "unit-test-ctx") {
		t.Fatalf("expected context name in error, got: %v", err)
	}
	// Sanity: we shouldn't have run anywhere near the production 60s.
	if elapsed > 5*time.Second {
		t.Fatalf("loop ran far longer than the test timeout: %s", elapsed)
	}
}

// Every PortBinding emitted by buildHostConfig must have a valid
// HostIP. The zero value of netip.Addr is `invalid`, which moby
// v1.54+ marshals to an empty JSON string and the Docker Engine
// daemon (28.x on ubuntu-latest) silently drops -- the container
// starts but NetworkSettings.Ports comes back as `{}` and the host
// cannot reach the apiserver. Regression guard for the
// silent-drop bug surfaced via Yolean/ystack PR 76.
func TestBuildHostConfig_PortBindingsHaveValidHostIP(t *testing.T) {
	cfg := config.DockerConfig{
		CommonConfig: config.CommonConfig{
			PortForwards: []config.PortForward{
				{Host: "6443", Guest: "6443"},
				{Host: "80", Guest: "80"},
				{Host: "443", Guest: "443"},
				{Host: "8944", Guest: "8944"},
			},
		},
	}
	hc, _, err := buildHostConfig(cfg)
	if err != nil {
		t.Fatalf("buildHostConfig: %v", err)
	}
	if got, want := len(hc.PortBindings), 4; got != want {
		t.Fatalf("PortBindings length: got %d, want %d", got, want)
	}
	for guest, bindings := range hc.PortBindings {
		if len(bindings) == 0 {
			t.Errorf("PortBindings[%s] empty", guest)
			continue
		}
		for _, b := range bindings {
			if !b.HostIP.IsValid() {
				t.Errorf("PortBindings[%s][HostPort=%s] HostIP %v is invalid -- docker daemon will silently drop this binding", guest, b.HostPort, b.HostIP)
			}
		}
	}
}

// TestBuildHostConfig_ExposedPortsMirrorBindings is the
// regression guard for issue #16: ExposedPorts must list every
// guest port that PortBindings carries. The Docker CLI's
// `docker run -p ...` auto-fills both; the moby SDK's
// ContainerCreate does not. Engine 28+ silently drops bindings
// when ExposedPorts is missing in some request shapes -- the
// container starts but NetworkSettings.Ports comes back `{}`
// and the host can't reach the apiserver, despite the
// in-process go-test path succeeding on the same runner.
//
// Both fields must agree key-by-key. Drift here = the bug
// reappears, possibly in a way that's only visible when the
// released binary runs under bash on a fresh ubuntu-latest.
func TestBuildHostConfig_ExposedPortsMirrorBindings(t *testing.T) {
	cfg := config.DockerConfig{
		CommonConfig: config.CommonConfig{
			PortForwards: []config.PortForward{
				{Host: "6443", Guest: "6443"},
				{Host: "80", Guest: "80"},
				{Host: "443", Guest: "443"},
				{Host: "8944", Guest: "8944"},
			},
		},
	}
	hc, exposed, err := buildHostConfig(cfg)
	if err != nil {
		t.Fatalf("buildHostConfig: %v", err)
	}
	if got, want := len(exposed), len(hc.PortBindings); got != want {
		t.Fatalf("ExposedPorts length %d != PortBindings length %d", got, want)
	}
	for guest := range hc.PortBindings {
		if _, ok := exposed[guest]; !ok {
			t.Errorf("ExposedPorts missing guest %s carried by PortBindings; Engine 28+ silently drops bindings without a matching ExposedPorts entry", guest)
		}
	}
	// And the inverse: no extra ExposedPorts entries that
	// don't appear in PortBindings (would mean we're declaring
	// surface area we don't intend to publish).
	for guest := range exposed {
		if _, ok := hc.PortBindings[guest]; !ok {
			t.Errorf("ExposedPorts has guest %s not carried by PortBindings; declares surface y-cluster doesn't intend to publish", guest)
		}
	}
}

// Caller-cancelled ctx: the loop returns ctx.Err() rather than the
// readiness deadline message. Guards against a refactor that drops
// the select { <-ctx.Done() } branch and silently makes the wait
// non-cancellable.
func TestPollHostAPIServerReadyz_ContextCanceled(t *testing.T) {
	fakeKubectlOnPATH(t, `echo failing >&2; exit 1`)
	c := newProbeTestCluster()
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	err := c.pollHostAPIServerReadyz(ctx, 10*time.Second, 5*time.Millisecond)
	if err == nil {
		t.Fatal("expected ctx error, got nil")
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("expected context.DeadlineExceeded, got: %v", err)
	}
}
