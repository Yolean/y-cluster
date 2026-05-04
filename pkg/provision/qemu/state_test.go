package qemu

import (
	"os"
	"path/filepath"
	"testing"
)

func TestSaveLoadState_Roundtrip(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("KUBECONFIG", "/path/to/kubeconfig")

	cfg := Config{
		Name:     "y-cluster-test",
		DiskSize: "20G",
		Memory:   "8192",
		CPUs:     "4",
		SSHPort:  "2222",
		PortForwards: []PortForward{
			{Host: "26443", Guest: "6443"},
			{Host: "8080", Guest: "80"},
		},
		Context:    "local",
		CacheDir:   dir,
		Kubeconfig: "/path/to/kubeconfig",
		K3s:        K3s{Version: "v1.35.4+k3s1", Install: "airgap"},
	}
	if err := saveState(cfg); err != nil {
		t.Fatalf("saveState: %v", err)
	}

	got, err := loadState(dir, cfg.Name)
	if err != nil {
		t.Fatalf("loadState: %v", err)
	}
	for _, c := range []struct{ name, got, want string }{
		{"Name", got.Name, cfg.Name},
		{"DiskSize", got.DiskSize, cfg.DiskSize},
		{"Memory", got.Memory, cfg.Memory},
		{"CPUs", got.CPUs, cfg.CPUs},
		{"SSHPort", got.SSHPort, cfg.SSHPort},
		{"Context", got.Context, cfg.Context},
		{"CacheDir", got.CacheDir, cfg.CacheDir},
		{"K3s.Version", got.K3s.Version, cfg.K3s.Version},
		{"K3s.Install", got.K3s.Install, cfg.K3s.Install},
		{"Kubeconfig", got.Kubeconfig, "/path/to/kubeconfig"},
	} {
		if c.got != c.want {
			t.Errorf("%s: got %q, want %q", c.name, c.got, c.want)
		}
	}
	if len(got.PortForwards) != 2 {
		t.Fatalf("PortForwards length: got %d, want 2", len(got.PortForwards))
	}
	if got.PortForwards[0] != cfg.PortForwards[0] || got.PortForwards[1] != cfg.PortForwards[1] {
		t.Fatalf("PortForwards mismatch: got %v, want %v", got.PortForwards, cfg.PortForwards)
	}
}

// TestLoadState_VersionMismatch covers the forward-compat guard:
// a sidecar with an unknown schema version errors loud rather
// than letting the caller proceed with a half-deserialized Config.
func TestLoadState_VersionMismatch(t *testing.T) {
	dir := t.TempDir()
	stale := []byte(`{"version":99,"name":"y-cluster-test","cacheDir":"/x"}`)
	if err := os.WriteFile(filepath.Join(dir, "y-cluster-test.json"), stale, 0o644); err != nil {
		t.Fatal(err)
	}
	_, err := loadState(dir, "y-cluster-test")
	if err == nil {
		t.Fatal("want error for stale version")
	}
	if !contains(err.Error(), "unsupported state version 99") {
		t.Fatalf("want version error, got %v", err)
	}
}

// TestLoadState_NotFound surfaces the os.ErrNotExist expected by
// `y-cluster start` so it can produce a friendly message ("no
// stopped cluster to start; run `y-cluster provision`").
func TestLoadState_NotFound(t *testing.T) {
	dir := t.TempDir()
	_, err := loadState(dir, "missing")
	if !os.IsNotExist(err) {
		t.Fatalf("want IsNotExist, got %v", err)
	}
}

func TestRemoveState_Idempotent(t *testing.T) {
	dir := t.TempDir()
	if err := removeState(dir, "missing"); err != nil {
		t.Fatalf("removeState on missing should be no-op: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "x.json"), []byte("{}"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := removeState(dir, "x"); err != nil {
		t.Fatalf("removeState: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "x.json")); !os.IsNotExist(err) {
		t.Fatal("file should have been removed")
	}
}

func contains(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}
