package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// requireKubectl skips when kubectl isn't on PATH; the CLI shells
// out to it for kubeconfig parsing.
func requireKubectl(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("kubectl"); err != nil {
		t.Skip("kubectl not in PATH")
	}
}

func TestDetectCmd_NoMatchingBackendErrors(t *testing.T) {
	requireKubectl(t)
	root := t.TempDir()
	kc := filepath.Join(root, "kubeconfig")
	writeFile(t, kc, `apiVersion: v1
kind: Config
clusters:
- cluster:
    server: http://127.0.0.1:6443
  name: y-cluster-test-no-such-thing
contexts:
- context:
    cluster: y-cluster-test-no-such-thing
    user: y-cluster-test-no-such-thing
  name: local
current-context: local
users:
- name: y-cluster-test-no-such-thing
`)
	t.Setenv("KUBECONFIG", kc)

	cmd := rootCmd()
	var out, errBuf strings.Builder
	cmd.SetOut(&out)
	cmd.SetErr(&errBuf)
	cmd.SetArgs([]string{"detect"})
	if err := cmd.Execute(); err == nil {
		t.Fatalf("expected error for absent backends; stdout=%q stderr=%q", out.String(), errBuf.String())
	}
}

func TestDetectCmd_UnknownContextErrors(t *testing.T) {
	requireKubectl(t)
	root := t.TempDir()
	kc := filepath.Join(root, "kubeconfig")
	writeFile(t, kc, `apiVersion: v1
kind: Config
clusters: []
contexts: []
users: []
`)
	t.Setenv("KUBECONFIG", kc)

	cmd := rootCmd()
	var out strings.Builder
	cmd.SetOut(&out)
	cmd.SetErr(&out)
	cmd.SetArgs([]string{"detect", "--context=does-not-exist"})
	if err := cmd.Execute(); err == nil {
		t.Fatal("expected error for unknown context")
	}
}

func TestCtrCmd_ForwardsArgsAfterDoubleDash(t *testing.T) {
	requireKubectl(t)
	// We can't actually exec docker without a cluster, so we go
	// only as far as Lookup — which fails with a known error
	// when nothing is running. The point of this test is that
	// cobra parsed the `--` boundary correctly: the flag
	// `--context` is consumed by cobra, the rest forwards.
	t.Setenv("KUBECONFIG", "/dev/null")
	cmd := rootCmd()
	var out strings.Builder
	cmd.SetOut(&out)
	cmd.SetErr(&out)
	cmd.SetArgs([]string{"ctr", "--context=does-not-exist", "--", "image", "ls"})
	err := cmd.Execute()
	if err == nil {
		t.Fatal("expected lookup error")
	}
	// And `--help` is allowed on ctrCmd itself (cobra builtin).
	cmd2 := rootCmd()
	var helpOut strings.Builder
	cmd2.SetOut(&helpOut)
	cmd2.SetArgs([]string{"ctr", "--help"})
	if err := cmd2.Execute(); err != nil {
		t.Fatalf("ctr --help: %v", err)
	}
	if !strings.Contains(helpOut.String(), "Routes <args> to the cluster node's ctr") {
		t.Fatalf("help missing summary: %q", helpOut.String())
	}
}

// Ensure the binary still builds end-to-end via go build.
func TestBinaryBuilds(t *testing.T) {
	dir := t.TempDir()
	bin := filepath.Join(dir, "y-cluster")
	cmd := exec.Command("go", "build", "-o", bin, ".")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("go build: %s: %v", out, err)
	}
	if _, err := os.Stat(bin); err != nil {
		t.Fatal(err)
	}
}
