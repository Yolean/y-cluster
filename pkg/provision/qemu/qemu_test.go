package qemu

import (
	"os"
	"path/filepath"
	"testing"
)

func TestDefaultConfig(t *testing.T) {
	cfg := DefaultConfig()
	if cfg.Name != "ystack-qemu" {
		t.Fatalf("expected ystack-qemu, got %s", cfg.Name)
	}
	if cfg.DiskSize != "40G" {
		t.Fatalf("expected 40G, got %s", cfg.DiskSize)
	}
	if cfg.Memory != "8192" {
		t.Fatalf("expected 8192, got %s", cfg.Memory)
	}
	if cfg.SSHPort != "2222" {
		t.Fatalf("expected 2222, got %s", cfg.SSHPort)
	}
	if cfg.Context != "local" {
		t.Fatalf("expected local, got %s", cfg.Context)
	}
}

func TestIsRunning_NoPidFile(t *testing.T) {
	cfg := DefaultConfig()
	cfg.CacheDir = t.TempDir()
	running, _ := cfg.IsRunning()
	if running {
		t.Fatal("expected not running when no pid file")
	}
}

func TestIsRunning_StalePidFile(t *testing.T) {
	cfg := DefaultConfig()
	cfg.CacheDir = t.TempDir()
	// Write a pid file with a non-existent PID
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
	err := ExportVMDK("/nonexistent/disk.qcow2", "/tmp/out.vmdk")
	if err == nil {
		t.Fatal("expected error for missing disk")
	}
}

func TestImportVMDK_MissingVMDK(t *testing.T) {
	err := ImportVMDK("/nonexistent/disk.vmdk", "/tmp/out.qcow2")
	if err == nil {
		t.Fatal("expected error for missing VMDK")
	}
}

func TestTeardownConfig_NoPidFile(t *testing.T) {
	cfg := DefaultConfig()
	cfg.CacheDir = t.TempDir()
	cfg.Kubeconfig = ""
	// Should not error when nothing to tear down
	if err := TeardownConfig(cfg, false, nil); err != nil {
		t.Fatal(err)
	}
}

func TestTeardownConfig_KeepDisk(t *testing.T) {
	cfg := DefaultConfig()
	cfg.CacheDir = t.TempDir()
	cfg.Kubeconfig = ""
	diskPath := filepath.Join(cfg.CacheDir, cfg.Name+".qcow2")
	if err := os.WriteFile(diskPath, []byte("fake"), 0o644); err != nil {
		t.Fatal(err)
	}

	if err := TeardownConfig(cfg, true, nil); err != nil {
		t.Fatal(err)
	}
	// Disk should still exist
	if _, err := os.Stat(diskPath); err != nil {
		t.Fatal("disk should be preserved with keepDisk=true")
	}
}

func TestTeardownConfig_DeleteDisk(t *testing.T) {
	cfg := DefaultConfig()
	cfg.CacheDir = t.TempDir()
	cfg.Kubeconfig = ""
	diskPath := filepath.Join(cfg.CacheDir, cfg.Name+".qcow2")
	if err := os.WriteFile(diskPath, []byte("fake"), 0o644); err != nil {
		t.Fatal(err)
	}

	if err := TeardownConfig(cfg, false, nil); err != nil {
		t.Fatal(err)
	}
	// Disk should be deleted
	if _, err := os.Stat(diskPath); err == nil {
		t.Fatal("disk should be deleted with keepDisk=false")
	}
}
