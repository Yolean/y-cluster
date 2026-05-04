package provision

import (
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestPreflight_PortFree exercises the happy path: a port that
// nobody is listening on passes the check.
func TestPreflight_PortFree(t *testing.T) {
	port := pickFreePort(t)
	if err := checkHostPort(port); err != nil {
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
	err = checkHostPort(port)
	if err == nil {
		t.Fatalf("port %s should report in-use", port)
	}
	if !strings.Contains(err.Error(), port) || !strings.Contains(err.Error(), "in use") {
		t.Fatalf("error should name the port and 'in use': %v", err)
	}
}

// TestPreflight_PortEmpty: an empty Host (provider auto-assigns)
// must not error -- there's nothing to check.
func TestPreflight_PortEmpty(t *testing.T) {
	if err := checkHostPort(""); err != nil {
		t.Fatalf("empty port should pass: %v", err)
	}
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
