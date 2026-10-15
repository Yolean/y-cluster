package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Yolean/y-cluster/pkg/kubeconfig"
)

// k3s.yaml as k3s writes it, trimmed to what the import touches.
const nodeK3sYAML = `apiVersion: v1
kind: Config
clusters:
- cluster:
    certificate-authority-data: Q0E=
    server: https://127.0.0.1:6443
  name: default
contexts:
- context:
    cluster: default
    user: default
  name: default
current-context: default
users:
- name: default
  user:
    client-certificate-data: Q0VSVA==
    client-key-data: S0VZ
`

func TestKubeconfigImport(t *testing.T) {
	path := filepath.Join(t.TempDir(), "kubeconfig")
	t.Setenv("KUBECONFIG", path)
	root := rootCmd()
	root.SetArgs([]string{"kubeconfig", "import", "--context", "edge", "--server", "https://127.0.0.1:16443"})
	root.SetIn(strings.NewReader(nodeK3sYAML))
	root.SetOut(&bytes.Buffer{})
	if err := root.Execute(); err != nil {
		t.Fatal(err)
	}
	f, err := kubeconfig.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := os.ReadFile(path)
	if len(f.Contexts) != 1 || f.Contexts[0].Name != "edge" || !strings.Contains(string(body), "server: https://127.0.0.1:16443") {
		t.Errorf("kubeconfig after import:\n%s", body)
	}

	// Something else on stdin, e.g. what sudo says without a password:
	// refused, and the file is left as it was.
	root = rootCmd()
	root.SetArgs([]string{"kubeconfig", "import", "--context", "other", "--server", "https://127.0.0.1:16444"})
	root.SetIn(strings.NewReader("sudo: a terminal is required to read the password\n"))
	root.SetOut(&bytes.Buffer{})
	root.SetErr(&bytes.Buffer{})
	if err := root.Execute(); err == nil {
		t.Error("non-kubeconfig input accepted")
	}
	if after, _ := os.ReadFile(path); !bytes.Equal(after, body) {
		t.Errorf("kubeconfig changed by a refused import:\n%s", after)
	}
}

func TestK3sBundle_RefusesOtherProviders(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "y-cluster-provision.yaml"), []byte("provider: docker\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{
		{"k3s", "bundle", "-c", dir, "--out", t.TempDir()},
		{"platform", "install", "-c", dir, "--check"},
	} {
		root := rootCmd()
		root.SetArgs(args)
		root.SetOut(&bytes.Buffer{})
		root.SetErr(&bytes.Buffer{})
		if err := root.Execute(); err == nil || !strings.Contains(err.Error(), `"host"`) {
			t.Errorf("%v: %v", args, err)
		}
	}
}
