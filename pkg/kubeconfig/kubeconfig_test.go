package kubeconfig

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestNew_RequiresKUBECONFIG(t *testing.T) {
	os.Unsetenv("KUBECONFIG")
	_, err := New("local", "ystack-qemu", nil)
	if err == nil {
		t.Fatal("expected error when KUBECONFIG not set")
	}
}

func TestNew_ReadsKUBECONFIG(t *testing.T) {
	os.Setenv("KUBECONFIG", "/tmp/test-kubeconfig")
	defer os.Unsetenv("KUBECONFIG")

	m, err := New("local", "ystack-qemu", nil)
	if err != nil {
		t.Fatal(err)
	}
	if m.Path != "/tmp/test-kubeconfig" {
		t.Fatalf("expected /tmp/test-kubeconfig, got %s", m.Path)
	}
	if m.Context != "local" {
		t.Fatalf("expected local, got %s", m.Context)
	}
	if m.ClusterName != "ystack-qemu" {
		t.Fatalf("expected ystack-qemu, got %s", m.ClusterName)
	}
}

func TestFixNullLists(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "kubeconfig")

	content := `apiVersion: v1
clusters: null
contexts: null
kind: Config
users: null
`
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}

	m := &Manager{Path: path}
	m.fixNullLists()

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	result := string(data)
	if strings.Contains(result, "null") {
		t.Fatalf("null should be replaced: %s", result)
	}
	if !strings.Contains(result, "clusters: []") {
		t.Fatalf("expected clusters: [], got: %s", result)
	}
	if !strings.Contains(result, "contexts: []") {
		t.Fatalf("expected contexts: [], got: %s", result)
	}
	if !strings.Contains(result, "users: []") {
		t.Fatalf("expected users: [], got: %s", result)
	}
}

func TestFixNullLists_NoChange(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "kubeconfig")

	content := `apiVersion: v1
clusters: []
contexts: []
kind: Config
users: []
`
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}

	m := &Manager{Path: path}
	m.fixNullLists()

	data, _ := os.ReadFile(path)
	if string(data) != content {
		t.Fatalf("should not modify when no nulls: %s", data)
	}
}

func TestFixNullLists_MissingFile(t *testing.T) {
	m := &Manager{Path: "/nonexistent/kubeconfig"}
	// Should not panic
	m.fixNullLists()
}

func TestImport_NewKubeconfig(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "kubeconfig")
	os.Setenv("KUBECONFIG", path)
	defer os.Unsetenv("KUBECONFIG")

	m, err := New("local", "ystack-test", nil)
	if err != nil {
		t.Fatal(err)
	}

	raw := []byte(`apiVersion: v1
kind: Config
clusters:
- cluster:
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
  user: {}
`)

	if err := m.Import(raw); err != nil {
		t.Fatal(err)
	}

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	result := string(data)

	// Context should be renamed
	if !strings.Contains(result, "name: local") {
		t.Fatalf("expected context name 'local': %s", result)
	}
	// Cluster and user should be renamed
	if !strings.Contains(result, "name: ystack-test") {
		t.Fatalf("expected cluster name 'ystack-test': %s", result)
	}
}

func TestImport_MergeExisting(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "kubeconfig")

	// Write an existing kubeconfig with a different context
	existing := `apiVersion: v1
kind: Config
clusters:
- cluster:
    server: https://10.0.0.1:6443
  name: prod
contexts:
- context:
    cluster: prod
    user: prod
  name: prod
current-context: prod
users:
- name: prod
  user: {}
`
	if err := os.WriteFile(path, []byte(existing), 0o600); err != nil {
		t.Fatal(err)
	}

	os.Setenv("KUBECONFIG", path)
	defer os.Unsetenv("KUBECONFIG")

	m, err := New("local", "ystack-test", nil)
	if err != nil {
		t.Fatal(err)
	}

	raw := []byte(`apiVersion: v1
kind: Config
clusters:
- cluster:
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
  user: {}
`)

	if err := m.Import(raw); err != nil {
		t.Fatal(err)
	}

	data, _ := os.ReadFile(path)
	result := string(data)

	// Both contexts should exist
	if !strings.Contains(result, "name: local") {
		t.Fatalf("expected local context: %s", result)
	}
	if !strings.Contains(result, "name: prod") {
		t.Fatalf("expected prod context preserved: %s", result)
	}
}

func TestCleanupStale_NoError(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "kubeconfig")
	if err := os.WriteFile(path, []byte("apiVersion: v1\nkind: Config\nclusters: []\ncontexts: []\nusers: []\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	os.Setenv("KUBECONFIG", path)
	defer os.Unsetenv("KUBECONFIG")

	m, _ := New("nonexistent", "nonexistent", nil)
	// Should not panic or error when entries don't exist
	m.CleanupStale()
}
