package provision

import (
	"errors"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Yolean/y-cluster/pkg/inventory"
)

// TestPreflight_PortFree exercises the happy path: a port that
// nobody is listening on passes the check.
func TestPreflight_PortFree(t *testing.T) {
	port := pickFreePort(t)
	if err := checkHostPort(port, PortBinderDaemon); err != nil {
		t.Fatalf("free port %s: %v", port, err)
	}
}

// TestPreflight_PortInUse: bind a port for the duration of the
// test so the check sees it as taken.
func TestPreflight_PortInUse(t *testing.T) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = l.Close() }()
	port := portFromAddr(l.Addr().String())
	err = checkHostPort(port, PortBinderDaemon)
	if err == nil {
		t.Fatalf("port %s should report in-use", port)
	}
	if !strings.Contains(err.Error(), port) || !strings.Contains(err.Error(), "in use") {
		t.Fatalf("error should name the port and 'in use': %v", err)
	}
}

// TestPreflight_WildcardListenerInUse is the ystack 8944
// regression: a host-local `y-cluster serve` holds *:8944, the
// provision then dies on the daemon's "address already in use".
// A loopback probe misses it -- SO_REUSEADDR lets 127.0.0.1:port
// bind alongside the wildcard on BSD -- so the check has to probe
// the wildcard, which is what the provider binds anyway. Both
// wildcard flavors matter: Docker Desktop's port publisher holds
// an IPv4-only socket ("tcp4"), while a Go server's default
// listen is a dual-stack IPv6 one ("tcp"); on Darwin a probe of
// the wrong family binds beside the other without conflict.
func TestPreflight_WildcardListenerInUse(t *testing.T) {
	for _, network := range []string{"tcp4", "tcp"} {
		t.Run(network, func(t *testing.T) {
			l, err := net.Listen(network, "0.0.0.0:0")
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = l.Close() }()
			port := portFromAddr(l.Addr().String())

			// Establish that this is the case a loopback probe lets
			// through, so the test keeps meaning what it says if the
			// probe address ever changes back.
			if lo, err := net.Listen("tcp4", net.JoinHostPort("127.0.0.1", port)); err == nil {
				_ = lo.Close()
			} else {
				t.Logf("loopback bind alongside wildcard already refused here (%v); "+
					"this platform would have caught the conflict either way", err)
			}

			err = checkHostPort(port, PortBinderDaemon)
			if err == nil {
				t.Fatalf("wildcard listener on port %s should report in-use", port)
			}
			if !strings.Contains(err.Error(), port) || !strings.Contains(err.Error(), "in use") {
				t.Fatalf("error should name the port and 'in use': %v", err)
			}
		})
	}
}

// TestPreflight_PortInUse_AttributedToInventory: when the host
// inventory records a cluster binding the conflicting port, the
// error names the cluster and hands over the exact teardown
// command instead of "likely another cluster".
func TestPreflight_PortInUse_AttributedToInventory(t *testing.T) {
	t.Setenv("Y_CLUSTER_INVENTORY_DIR", t.TempDir())
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = l.Close() }()
	port := portFromAddr(l.Addr().String())
	if err := inventory.Save(inventory.Record{
		Context:   "node-agent",
		Name:      "node-agent",
		Provider:  "docker",
		ConfigDir: "/repo/itest/cluster/docker",
		HostPorts: []string{port},
	}); err != nil {
		t.Fatal(err)
	}
	err = Preflight{HostPorts: []string{port}}.Run()
	if err == nil {
		t.Fatalf("port %s should report in-use", port)
	}
	for _, want := range []string{`"node-agent"`, "teardown -c /repo/itest/cluster/docker", port} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("error should contain %q; got %v", want, err)
		}
	}
}

// TestPreflight_PortEmpty: an empty Host (provider auto-assigns)
// must not error -- there's nothing to check.
func TestPreflight_PortEmpty(t *testing.T) {
	if err := checkHostPort("", PortBinderDaemon); err != nil {
		t.Fatalf("empty port should pass: %v", err)
	}
}

// TestPreflight_PrivilegedPortDaemonBinder is the ystack regression:
// `portForwards: [{host: "80", guest: "80"}]` on the docker
// provider, provisioned by a non-root user. The bind probe is
// refused for lack of privilege while the port is in fact free and
// dockerd would publish it happily, so the check must pass.
func TestPreflight_PrivilegedPortDaemonBinder(t *testing.T) {
	port := unbindablePortOrSkip(t)
	if err := checkHostPort(port, PortBinderDaemon); err != nil {
		t.Fatalf("free privileged port %s under a daemon binder: %v", port, err)
	}
}

// TestPreflight_PrivilegedPortSelfBinder: the same refused probe,
// but qemu binds hostfwd ports as this user, so the port really is
// unusable. Fail -- and say why, rather than pinning it on another
// cluster that doesn't exist.
func TestPreflight_PrivilegedPortSelfBinder(t *testing.T) {
	port := unbindablePortOrSkip(t)
	err := checkHostPort(port, PortBinderSelf)
	if err == nil {
		t.Fatalf("privileged port %s under a self binder should error", port)
	}
	if !strings.Contains(err.Error(), port) {
		t.Fatalf("error should name the port: %v", err)
	}
	if strings.Contains(err.Error(), "in use") {
		t.Fatalf("permission denied must not be reported as in-use: %v", err)
	}
}

// TestPreflight_HostPortAnswers covers the fallback probe on its
// own, since the privileged-port path can't be set up both ways in
// one unprivileged test: it has to spot a live listener without
// binding anything, and stop spotting it once the listener closes.
func TestPreflight_HostPortAnswers(t *testing.T) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := l.Addr().String()
	if !hostPortAnswers(addr) {
		t.Fatalf("live listener at %s should answer", addr)
	}
	if err := l.Close(); err != nil {
		t.Fatal(err)
	}
	if hostPortAnswers(addr) {
		t.Fatalf("closed listener at %s should not answer", addr)
	}
}

// unbindablePortOrSkip returns a privileged port that this process
// can neither bind nor reach, so checkHostPort's permission branch
// is the one under test. Skips where the environment can't produce
// that: Darwin, which lets any user bind a reserved port on the
// wildcard; running as root; an unprivileged range reaching this
// far down (containers default net.ipv4.ip_unprivileged_port_start
// to 0); or something already listening.
func unbindablePortOrSkip(t *testing.T) string {
	t.Helper()
	const port = "1023" // highest privileged port, least likely to be claimed
	l, err := net.Listen("tcp", net.JoinHostPort("0.0.0.0", port))
	if err == nil {
		_ = l.Close()
		t.Skipf("port %s is bindable here; no permission denial to test", port)
	}
	if !errors.Is(err, os.ErrPermission) {
		t.Skipf("port %s unavailable for a reason other than permission: %v", port, err)
	}
	return port
}

// TestPreflight_ContextAbsent: a context name that doesn't exist
// in kubeconfig is fine -- there's nothing to clobber.
func TestPreflight_ContextAbsent(t *testing.T) {
	path := writeKubeconfig(t, "")
	if err := checkKubeconfigContext(path, "absent", "y-cluster"); err != nil {
		t.Fatalf("absent context should pass: %v", err)
	}
}

// TestPreflight_ContextSameCluster: re-provisioning is the common
// case -- the context already exists pointing at the same cluster
// the new Provision will produce. Pass.
func TestPreflight_ContextSameCluster(t *testing.T) {
	path := writeKubeconfig(t, `
apiVersion: v1
kind: Config
clusters:
- name: y-cluster
  cluster:
    server: https://127.0.0.1:26443
contexts:
- name: local
  context:
    cluster: y-cluster
    user: ystack
`)
	if err := checkKubeconfigContext(path, "local", "y-cluster"); err != nil {
		t.Fatalf("re-provision (same cluster) should pass: %v", err)
	}
}

// TestPreflight_ContextDifferentCluster: this is the regression
// guard -- a second cluster that re-uses an existing context name
// would clobber the first cluster's kubectl access.
func TestPreflight_ContextDifferentCluster(t *testing.T) {
	path := writeKubeconfig(t, `
apiVersion: v1
kind: Config
clusters:
- name: y-cluster
  cluster:
    server: https://127.0.0.1:26443
contexts:
- name: local
  context:
    cluster: y-cluster
    user: ystack
`)
	err := checkKubeconfigContext(path, "local", "y-cluster-tiny")
	if err == nil {
		t.Fatal("context pointing at a different cluster should error")
	}
	for _, want := range []string{"local", "y-cluster", "y-cluster-tiny"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("error should name %q; got %v", want, err)
		}
	}
}

// TestPreflight_RunAccumulatesProblems: exercising Run as a
// whole. The point is a single error listing every conflict the
// caller has to fix, not a fail-fast that surfaces them one at a
// time.
func TestPreflight_RunAccumulatesProblems(t *testing.T) {
	// Hermetic inventory: port attribution must not read the
	// developer's real records.
	t.Setenv("Y_CLUSTER_INVENTORY_DIR", t.TempDir())
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = l.Close() }()
	boundPort := portFromAddr(l.Addr().String())
	path := writeKubeconfig(t, `
apiVersion: v1
kind: Config
clusters:
- name: y-cluster
  cluster:
    server: https://127.0.0.1:26443
contexts:
- name: local
  context:
    cluster: y-cluster
    user: ystack
`)
	pf := Preflight{
		HostPorts:      []string{boundPort},
		ContextName:    "local",
		ContextCluster: "y-cluster-tiny",
		KubeconfigPath: path,
	}
	err = pf.Run()
	if err == nil {
		t.Fatal("want aggregated error")
	}
	if !strings.Contains(err.Error(), "preflight checks failed") {
		t.Fatalf("missing summary header: %v", err)
	}
	if !strings.Contains(err.Error(), boundPort) {
		t.Fatalf("missing port in error: %v", err)
	}
	if !strings.Contains(err.Error(), "local") {
		t.Fatalf("missing context name in error: %v", err)
	}
}

// pickFreePort obtains a port number that's currently free by
// asking the kernel (Listen on :0, capture, close).
func pickFreePort(t *testing.T) string {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := portFromAddr(l.Addr().String())
	_ = l.Close()
	return port
}

func portFromAddr(addr string) string {
	_, port, _ := net.SplitHostPort(addr)
	return port
}

func writeKubeconfig(t *testing.T, body string) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "kubeconfig")
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}
