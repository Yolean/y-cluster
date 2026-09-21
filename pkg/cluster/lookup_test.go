package cluster

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
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
	// kubectl jsonpath returns empty string for no-match -- Lookup
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

// unreachableDocker points the docker client at a socket that does not
// exist, which is what a host without a docker daemon looks like.
func unreachableDocker(t *testing.T) {
	t.Helper()
	t.Setenv("DOCKER_HOST", "unix://"+filepath.Join(t.TempDir(), "no-docker.sock"))
}

// A qemu host needs no docker daemon; stop, ctr and images load must
// still find the cluster.
func TestLookup_FindsQemuWhenDockerIsUnreachable(t *testing.T) {
	requireKubectl(t)
	unreachableDocker(t)
	name := "y-cluster-test-nodocker"
	dir := t.TempDir()
	t.Setenv("Y_CLUSTER_QEMU_CACHE_DIR", dir)
	if err := os.WriteFile(filepath.Join(dir, name+".pid"),
		[]byte(fmt.Sprintf("%d\n", os.Getpid())), 0o644); err != nil {
		t.Fatal(err)
	}
	kc := writeKubeconfig(t, "local", name)

	got, err := Lookup(context.Background(), kc, "local")
	if err != nil {
		t.Fatalf("Lookup: %v", err)
	}
	if got.Backend != BackendQEMU {
		t.Fatalf("backend %q, want qemu", got.Backend)
	}
}

// With no backend claiming the cluster, the docker probe failure is
// the likeliest explanation and has to be in the error.
func TestLookup_ReportsDockerProbeFailureWhenNothingMatches(t *testing.T) {
	requireKubectl(t)
	unreachableDocker(t)
	t.Setenv("Y_CLUSTER_QEMU_CACHE_DIR", t.TempDir())
	kc := writeKubeconfig(t, "local", "y-cluster-test-no-such-thing-1234567890")

	_, err := Lookup(context.Background(), kc, "local")
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("want ErrNotFound, got %v", err)
	}
	if !strings.Contains(err.Error(), "docker could not be probed") {
		t.Fatalf("error should carry the docker probe failure: %v", err)
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
	if got, _ := readQemuState(good); got != "2229" {
		t.Errorf("good: got %q, want %q", got, "2229")
	}

	noField := filepath.Join(dir, "no-field.json")
	if err := os.WriteFile(noField, []byte(`{"version":1,"name":"x"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if got, _ := readQemuState(noField); got != "" {
		t.Errorf("no-field: got %q, want empty", got)
	}

	if got, _ := readQemuState(filepath.Join(dir, "missing.json")); got != "" {
		t.Errorf("missing: got %q, want empty", got)
	}

	bad := filepath.Join(dir, "bad.json")
	if err := os.WriteFile(bad, []byte("not json"), 0o644); err != nil {
		t.Fatal(err)
	}
	if got, _ := readQemuState(bad); got != "" {
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

	alive, sshKey, _, sshPort := qemuRunning(name)
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

	alive, _, sshHost, sshPort := qemuRunning(name)
	if !alive {
		t.Fatal("expected alive=true")
	}
	if sshPort != "" {
		t.Errorf("sshPort: got %q, want empty (so caller falls back to default)", sshPort)
	}
	if sshHost != "127.0.0.1" {
		t.Errorf("sshHost: got %q, want loopback (a forward without a recorded bind address is on the wildcard)", sshHost)
	}
}

// TestQemuRunning_SSHHostFollowsBindAddress: ctr/crictl/images load
// have to dial the address the ssh forward actually listens on.
func TestQemuRunning_SSHHostFollowsBindAddress(t *testing.T) {
	for bind, want := range map[string]string{
		"127.0.0.1":    "127.0.0.1",
		"0.0.0.0":      "127.0.0.1",
		"192.168.1.10": "192.168.1.10",
	} {
		dir := t.TempDir()
		t.Setenv("Y_CLUSTER_QEMU_CACHE_DIR", dir)
		name := "y-cluster-test-bind"
		if err := os.WriteFile(filepath.Join(dir, name+".pid"),
			[]byte(fmt.Sprintf("%d\n", os.Getpid())), 0o644); err != nil {
			t.Fatal(err)
		}
		state := fmt.Sprintf(`{"sshPort":"2229","bindAddress":%q}`, bind)
		if err := os.WriteFile(filepath.Join(dir, name+".json"), []byte(state), 0o644); err != nil {
			t.Fatal(err)
		}
		if _, _, got, _ := qemuRunning(name); got != want {
			t.Errorf("bindAddress %q: sshHost %q, want %q", bind, got, want)
		}
	}
}

// In tap mode there is no forward: sshd is on the guest's own address.
func TestQemuRunning_TapModeDialsTheGuest(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("Y_CLUSTER_QEMU_CACHE_DIR", dir)
	name := "y-cluster-test-tap"
	if err := os.WriteFile(filepath.Join(dir, name+".pid"),
		[]byte(fmt.Sprintf("%d\n", os.Getpid())), 0o644); err != nil {
		t.Fatal(err)
	}
	state := `{"sshPort":"","tap":{"ifname":"ycl0","guestAddress":"10.88.0.2/24"}}`
	if err := os.WriteFile(filepath.Join(dir, name+".json"), []byte(state), 0o644); err != nil {
		t.Fatal(err)
	}
	_, _, host, port := qemuRunning(name)
	if host != "10.88.0.2" || port != "22" {
		t.Fatalf("got %s:%s, want 10.88.0.2:22", host, port)
	}
}

// TestHetznerRunning_FromState round-trips the hetzner state
// discovery: write a fake state sidecar, verify hetznerRunning
// returns the IPv4 + sshUser the state encodes plus the
// canonical key path.
func TestHetznerRunning_FromState(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("Y_CLUSTER_HETZNER_CACHE_DIR", dir)

	name := "alice-dev"
	if err := os.WriteFile(filepath.Join(dir, name+".json"),
		[]byte(`{"context":"alice-dev","ipv4":"203.0.113.1","sshUser":"ystack","serverID":1}`),
		0o644); err != nil {
		t.Fatal(err)
	}

	alive, sshKey, sshHost, sshUser := hetznerRunning(name)
	if !alive {
		t.Fatal("expected alive=true with state present")
	}
	if sshHost != "203.0.113.1" {
		t.Errorf("sshHost: got %q, want %q", sshHost, "203.0.113.1")
	}
	if sshUser != "ystack" {
		t.Errorf("sshUser: got %q, want %q", sshUser, "ystack")
	}
	if got, want := sshKey, filepath.Join(dir, name+"-ssh"); got != want {
		t.Errorf("sshKey: got %q, want %q", got, want)
	}
}

// TestHetznerRunning_MissingOrInvalid: the predicate must return
// false when state is missing, malformed, or has empty required
// fields, so Lookup falls through to the not-found error rather
// than producing a half-populated result.
func TestHetznerRunning_MissingOrInvalid(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("Y_CLUSTER_HETZNER_CACHE_DIR", dir)

	if alive, _, _, _ := hetznerRunning("never-provisioned"); alive {
		t.Error("missing state must report alive=false")
	}

	// Malformed JSON.
	bad := "bad-json"
	if err := os.WriteFile(filepath.Join(dir, bad+".json"), []byte("{not json"), 0o644); err != nil {
		t.Fatal(err)
	}
	if alive, _, _, _ := hetznerRunning(bad); alive {
		t.Error("malformed JSON must report alive=false")
	}

	// Missing IPv4.
	noIP := "no-ip"
	if err := os.WriteFile(filepath.Join(dir, noIP+".json"),
		[]byte(`{"sshUser":"ystack"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if alive, _, _, _ := hetznerRunning(noIP); alive {
		t.Error("missing ipv4 must report alive=false")
	}
}
