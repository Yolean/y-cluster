//go:build e2e && kvm

package e2e

import (
	"context"
	"os"
	"testing"

	"go.uber.org/zap"

	"github.com/Yolean/y-cluster/pkg/provision/qemu"
)

func TestQemu_ProvisionTeardown(t *testing.T) {
	if _, err := os.Stat("/dev/kvm"); err != nil {
		t.Skip("QEMU tests require /dev/kvm")
	}
	if err := qemu.CheckPrerequisites(); err != nil {
		t.Skip(err)
	}

	logger, _ := zap.NewDevelopment()
	cfg := qemu.DefaultConfig()
	cfg.Name = "y-cluster-e2e-qemu"
	cfg.CacheDir = t.TempDir()
	cfg.Memory = "4096"
	cfg.CPUs = "2"
	cfg.SSHPort = "2223"      // avoid conflict with real cluster on 2222
	cfg.PortForwards = nil    // no port forwards for isolated e2e
	cfg.Kubeconfig = os.Getenv("KUBECONFIG")
	if cfg.Kubeconfig == "" {
		t.Skip("KUBECONFIG must be set")
	}

	ctx := context.Background()

	// Provision
	cluster, err := qemu.Provision(ctx, cfg, logger)
	if err != nil {
		t.Fatal(err)
	}

	// Verify SSH works
	out, err := cluster.SSH(ctx, "hostname")
	if err != nil {
		t.Fatalf("SSH failed: %v", err)
	}
	t.Logf("hostname: %s", out)

	// Teardown with disk deleted
	if err := cluster.Teardown(false); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(cluster.DiskPath()); err == nil {
		t.Fatal("disk should be deleted after teardown with keepDisk=false")
	}
}

func TestQemu_TeardownKeepDisk(t *testing.T) {
	if _, err := os.Stat("/dev/kvm"); err != nil {
		t.Skip("QEMU tests require /dev/kvm")
	}
	if err := qemu.CheckPrerequisites(); err != nil {
		t.Skip(err)
	}

	logger, _ := zap.NewDevelopment()
	cfg := qemu.DefaultConfig()
	cfg.Name = "y-cluster-e2e-keepdisk"
	cfg.CacheDir = t.TempDir()
	cfg.Memory = "4096"
	cfg.CPUs = "2"
	cfg.SSHPort = "2223"
	cfg.PortForwards = nil
	cfg.Kubeconfig = os.Getenv("KUBECONFIG")
	if cfg.Kubeconfig == "" {
		t.Skip("KUBECONFIG must be set")
	}

	ctx := context.Background()
	cluster, err := qemu.Provision(ctx, cfg, logger)
	if err != nil {
		t.Fatal(err)
	}

	// Teardown with disk preserved
	if err := cluster.Teardown(true); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(cluster.DiskPath()); err != nil {
		t.Fatal("disk should be preserved with keepDisk=true")
	}
}

func TestQemu_ExportImport(t *testing.T) {
	if _, err := os.Stat("/dev/kvm"); err != nil {
		t.Skip("QEMU tests require /dev/kvm")
	}
	if err := qemu.CheckPrerequisites(); err != nil {
		t.Skip(err)
	}

	logger, _ := zap.NewDevelopment()
	cfg := qemu.DefaultConfig()
	cfg.Name = "y-cluster-e2e-export"
	cfg.CacheDir = t.TempDir()
	cfg.Memory = "4096"
	cfg.CPUs = "2"
	cfg.SSHPort = "2224"
	cfg.PortForwards = nil
	cfg.Kubeconfig = os.Getenv("KUBECONFIG")
	if cfg.Kubeconfig == "" {
		t.Skip("KUBECONFIG must be set")
	}

	ctx := context.Background()

	// Provision
	cluster, err := qemu.Provision(ctx, cfg, logger)
	if err != nil {
		t.Fatal(err)
	}

	// Stop VM but keep disk
	if err := cluster.Teardown(true); err != nil {
		t.Fatal(err)
	}

	// Export to VMDK
	vmdkPath := cfg.CacheDir + "/appliance.vmdk"
	if err := qemu.ExportVMDK(cluster.DiskPath(), vmdkPath); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(vmdkPath); err != nil {
		t.Fatal("VMDK should exist after export")
	}
	t.Logf("exported VMDK: %s", vmdkPath)

	// Delete original disk
	os.Remove(cluster.DiskPath())

	// Import from VMDK
	if err := qemu.ImportVMDK(vmdkPath, cluster.DiskPath()); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(cluster.DiskPath()); err != nil {
		t.Fatal("disk should exist after import")
	}

	// Provision from imported disk
	cluster2, err := qemu.Provision(ctx, cfg, logger)
	if err != nil {
		t.Fatal(err)
	}
	out, err := cluster2.SSH(ctx, "hostname")
	if err != nil {
		t.Fatalf("SSH after import: %v", err)
	}
	t.Logf("hostname after import: %s", out)

	// Clean up
	if err := cluster2.Teardown(false); err != nil {
		t.Fatal(err)
	}
}
