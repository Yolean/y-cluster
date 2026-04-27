package cluster

import (
	"context"
	"errors"
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
