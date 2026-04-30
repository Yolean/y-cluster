package qemu

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/Yolean/y-cluster/pkg/provision/config"
)

// defaultedRuntimeConfig builds a runtime Config from a freshly
// defaulted config.QEMUConfig. Tests use this where they need a
// "typical" config without spelling out every field, exercising both
// the defaults applier (in the config package) and FromConfig (here).
func defaultedRuntimeConfig(t *testing.T) Config {
	t.Helper()
	c := &config.QEMUConfig{CommonConfig: config.CommonConfig{Provider: config.ProviderQEMU}}
	c.ApplyDefaults()
	return FromConfig(c)
}

func TestFromConfig_AppliesDefaults(t *testing.T) {
	cfg := defaultedRuntimeConfig(t)
	if cfg.Name != "y-cluster" {
		t.Fatalf("Name: %q", cfg.Name)
	}
	if cfg.DiskSize != "20G" {
		t.Fatalf("DiskSize: %q", cfg.DiskSize)
	}
	if cfg.Memory != "8192" {
		t.Fatalf("Memory: %q", cfg.Memory)
	}
	if cfg.SSHPort != "2222" {
		t.Fatalf("SSHPort: %q", cfg.SSHPort)
	}
	if cfg.Context != "local" {
		t.Fatalf("Context: %q", cfg.Context)
	}
	if cfg.CacheDir == "" {
		t.Fatal("CacheDir defaulted to empty (should fall back to ~/.cache/y-cluster-qemu)")
	}
	// Default port forwards land here when the on-disk config omits them.
	if len(cfg.PortForwards) != 3 {
		t.Fatalf("PortForwards: %v", cfg.PortForwards)
	}
}

func TestFromConfig_PreservesExplicitPortForwards(t *testing.T) {
	c := &config.QEMUConfig{
		CommonConfig: config.CommonConfig{
			Provider:     config.ProviderQEMU,
			PortForwards: []config.PortForward{{Host: "26443", Guest: "6443"}, {Host: "9090", Guest: "9090"}},
		},
	}
	c.ApplyDefaults()
	rt := FromConfig(c)
	if len(rt.PortForwards) != 2 || rt.PortForwards[1].Guest != "9090" {
		t.Fatalf("port forwards not preserved: %v", rt.PortForwards)
	}
}

func TestIsRunning_NoPidFile(t *testing.T) {
	cfg := defaultedRuntimeConfig(t)
	cfg.CacheDir = t.TempDir()
	running, _ := cfg.IsRunning()
	if running {
		t.Fatal("expected not running when no pid file")
	}
}

func TestIsRunning_StalePidFile(t *testing.T) {
	cfg := defaultedRuntimeConfig(t)
	cfg.CacheDir = t.TempDir()
	pidFile := filepath.Join(cfg.CacheDir, cfg.Name+".pid")
	if err := os.WriteFile(pidFile, []byte("999999999\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	running, _ := cfg.IsRunning()
	if running {
		t.Fatal("expected not running for stale pid")
	}
}

func TestExportVMDK_MissingDisk(t *testing.T) {
	if err := ExportVMDK("/nonexistent/disk.qcow2", "/tmp/out.vmdk"); err == nil {
		t.Fatal("expected error for missing disk")
	}
}

func TestImportVMDK_MissingVMDK(t *testing.T) {
	if err := ImportVMDK("/nonexistent/disk.vmdk", "/tmp/out.qcow2"); err == nil {
		t.Fatal("expected error for missing VMDK")
	}
}

func TestTeardownConfig_NoPidFile(t *testing.T) {
	cfg := defaultedRuntimeConfig(t)
	cfg.CacheDir = t.TempDir()
	cfg.Kubeconfig = ""
	if err := TeardownConfig(cfg, false, nil); err != nil {
		t.Fatal(err)
	}
}

func TestTeardownConfig_KeepDisk(t *testing.T) {
	cfg := defaultedRuntimeConfig(t)
	cfg.CacheDir = t.TempDir()
	cfg.Kubeconfig = ""
	diskPath := filepath.Join(cfg.CacheDir, cfg.Name+".qcow2")
	if err := os.WriteFile(diskPath, []byte("fake"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := TeardownConfig(cfg, true, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(diskPath); err != nil {
		t.Fatal("disk should be preserved with keepDisk=true")
	}
}

func TestTeardownConfig_DeleteDisk(t *testing.T) {
	cfg := defaultedRuntimeConfig(t)
	cfg.CacheDir = t.TempDir()
	cfg.Kubeconfig = ""
	diskPath := filepath.Join(cfg.CacheDir, cfg.Name+".qcow2")
	if err := os.WriteFile(diskPath, []byte("fake"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := TeardownConfig(cfg, false, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(diskPath); err == nil {
		t.Fatal("disk should be deleted with keepDisk=false")
	}
}
