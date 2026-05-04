package cluster

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

// requireKubectl skips the test when kubectl isn't on PATH;
// readClusterName shells out to it and there's no value in
// reimplementing kubeconfig parsing just for unit tests.
func requireKubectl(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("kubectl"); err != nil {
		t.Skip("kubectl not in PATH")
	}
}

func writeKubeconfig(t *testing.T, contextName, clusterName string) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "kubeconfig")
	body := `apiVersion: v1
kind: Config
clusters:
- cluster:
    server: http://127.0.0.1:6443
  name: ` + clusterName + `
contexts:
- context:
    cluster: ` + clusterName + `
    user: ` + clusterName + `
  name: ` + contextName + `
current-context: ` + contextName + `
users:
- name: ` + clusterName + `
`
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestReadClusterName_HappyPath(t *testing.T) {
	requireKubectl(t)
	kc := writeKubeconfig(t, "local", "my-cluster")
	got, err := readClusterName(context.Background(), kc, "local")
	if err != nil {
		t.Fatal(err)
	}
	if got != "my-cluster" {
		t.Fatalf("got %q", got)
	}
}

func TestReadClusterName_UnknownContext(t *testing.T) {
	requireKubectl(t)
	kc := writeKubeconfig(t, "local", "my-cluster")
	got, err := readClusterName(context.Background(), kc, "nope")
	if err != nil {
		t.Fatal(err)
	}
	// kubectl jsonpath returns empty string for no-match — Lookup
	// turns that into a user-facing error, but the helper itself
	// just propagates the empty value.
	if got != "" {
		t.Fatalf("got %q, want empty", got)
	}
}

func TestLookup_UnknownContextErrors(t *testing.T) {
	requireKubectl(t)
	kc := writeKubeconfig(t, "local", "my-cluster")
	_, err := Lookup(context.Background(), kc, "nope")
	if err == nil {
		t.Fatal("expected error for unknown context")
	}
}

func TestLookup_NoBackendMatchesIsErrNotFound(t *testing.T) {
	requireKubectl(t)
	// Pick a cluster name that is extremely unlikely to match a
	// real local docker container or qemu pidfile.
	kc := writeKubeconfig(t, "local", "y-cluster-test-no-such-thing-1234567890")
	_, err := Lookup(context.Background(), kc, "local")
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("want ErrNotFound, got %v", err)
	}
}

// TestReadQemuStateSSHPort pins the JSON shape: the qemu state
// sidecar (pkg/provision/qemu/state.go) marshals SSHPort as
// "sshPort". A field rename without updating the lookup-side
// reader here would silently fall back to the default qemu
// provisioner port and break any cluster provisioned on a
// non-default port -- exactly the regression that motivated
// this code path.
func TestReadQemuStateSSHPort(t *testing.T) {
	dir := t.TempDir()
	good := filepath.Join(dir, "good.json")
	if err := os.WriteFile(good, []byte(`{"version":1,"name":"x","sshPort":"2229"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := readQemuStateSSHPort(good); got != "2229" {
		t.Errorf("good: got %q, want %q", got, "2229")
	}

	noField := filepath.Join(dir, "no-field.json")
	if err := os.WriteFile(noField, []byte(`{"version":1,"name":"x"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := readQemuStateSSHPort(noField); got != "" {
		t.Errorf("no-field: got %q, want empty", got)
	}

	if got := readQemuStateSSHPort(filepath.Join(dir, "missing.json")); got != "" {
		t.Errorf("missing: got %q, want empty", got)
	}

	bad := filepath.Join(dir, "bad.json")
	if err := os.WriteFile(bad, []byte("not json"), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := readQemuStateSSHPort(bad); got != "" {
		t.Errorf("bad-json: got %q, want empty", got)
	}
}

// TestQemuRunning_PortFromState round-trips the discovery: write
// a fake pidfile + state, verify qemuRunning returns the port
// the state encodes (not the hardcoded fallback). Uses a live
// PID (the test process itself) since pidAlive requires one.
func TestQemuRunning_PortFromState(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("Y_CLUSTER_QEMU_CACHE_DIR", dir)

	name := "y-cluster-test-portfromstate"
	pid := os.Getpid()
	if err := os.WriteFile(filepath.Join(dir, name+".pid"),
		[]byte(fmt.Sprintf("%d\n", pid)), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, name+".json"),
		[]byte(`{"version":1,"name":"`+name+`","sshPort":"33445"}`), 0o644); err != nil {
		t.Fatal(err)
	}

	alive, sshKey, sshPort := qemuRunning(name)
	if !alive {
		t.Fatalf("expected alive=true (pid %d is this test process)", pid)
	}
	if sshPort != "33445" {
		t.Errorf("sshPort: got %q, want %q", sshPort, "33445")
	}
	wantKey := filepath.Join(dir, name+"-ssh")
	if sshKey != wantKey {
		t.Errorf("sshKey: got %q, want %q", sshKey, wantKey)
	}
}

// TestQemuRunning_PortFallbackWhenStateMissing pins the
// graceful-degrade behaviour: when the pidfile is alive but the
// state JSON isn't there (e.g., a really old cache), qemuRunning
// reports running but returns "" for sshPort. Lookup then
// substitutes the qemu provisioner's default.
func TestQemuRunning_PortFallbackWhenStateMissing(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("Y_CLUSTER_QEMU_CACHE_DIR", dir)

	name := "y-cluster-test-portfallback"
	if err := os.WriteFile(filepath.Join(dir, name+".pid"),
		[]byte(fmt.Sprintf("%d\n", os.Getpid())), 0o644); err != nil {
		t.Fatal(err)
	}
	// no <name>.json on purpose

	alive, _, sshPort := qemuRunning(name)
	if !alive {
		t.Fatal("expected alive=true")
	}
	if sshPort != "" {
		t.Errorf("sshPort: got %q, want empty (so caller falls back to default)", sshPort)
	}
}
