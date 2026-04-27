//go:build e2e && kvm

package e2e

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"go.uber.org/zap"

	"github.com/Yolean/y-cluster/pkg/provision/config"
	"github.com/Yolean/y-cluster/pkg/provision/qemu"
)

// readPid reads <cacheDir>/<name>.pid and returns the integer.
// We read it before teardown so the assertion afterwards has
// something to point at -- teardown unlinks the pidfile on success.
func readPid(t *testing.T, cacheDir, name string) int {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(cacheDir, name+".pid"))
	if err != nil {
		t.Fatalf("read pidfile: %v", err)
	}
	var pid int
	if _, err := fmt.Sscanf(strings.TrimSpace(string(data)), "%d", &pid); err != nil {
		t.Fatalf("parse pidfile %q: %v", data, err)
	}
	return pid
}

// assertPidGone fails if the pid is still signalable. We use
// signal(0) (the POSIX liveness probe) directly rather than the
// internal pidAlive helper so this test stays in package e2e.
func assertPidGone(t *testing.T, pid int) {
	t.Helper()
	// Tiny grace: the kernel may still be tearing the process down
	// when the parent (init, since qemu daemonized) hasn't quite
	// reaped it yet. Five short polls beat a sleep.
	for i := 0; i < 5; i++ {
		err := syscall.Kill(pid, 0)
		if errors.Is(err, syscall.ESRCH) {
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatalf("VM pid %d still alive after teardown", pid)
}

// e2eQEMURuntime returns a runtime qemu.Config seeded from a freshly
// defaulted config.QEMUConfig. Tests then override individual fields
// to keep ports / cache dirs / contexts isolated from a developer's
// real cluster on the same host.
func e2eQEMURuntime() qemu.Config {
	c := &config.QEMUConfig{CommonConfig: config.CommonConfig{Provider: config.ProviderQEMU}}
	c.ApplyDefaults()
	return qemu.FromConfig(c)
}

// e2eUniqueForwards builds a port-forward list that won't collide
// with another e2e test running on the same machine. Required since
// Provision now installs k3s and needs a forward to guest 6443 to
// extract a working kubeconfig.
func e2eUniqueForwards(apiPort string) []qemu.PortForward {
	return []qemu.PortForward{{Host: apiPort, Guest: "6443"}}
}

func TestQemu_ProvisionTeardown(t *testing.T) {
	if _, err := os.Stat("/dev/kvm"); err != nil {
		t.Skip("QEMU tests require /dev/kvm")
	}
	if err := qemu.CheckPrerequisites(); err != nil {
		t.Skip(err)
	}

	logger, _ := zap.NewDevelopment()
	cfg := e2eQEMURuntime()
	cfg.Name = "y-cluster-e2e-qemu"
	cfg.Context = "y-cluster-e2e-qemu"
	cfg.CacheDir = t.TempDir()
	cfg.Memory = "4096"
	cfg.CPUs = "2"
	cfg.SSHPort = "2223" // avoid conflict with real cluster on 2222
	cfg.PortForwards = e2eUniqueForwards("26443")
	cfg.Kubeconfig = os.Getenv("KUBECONFIG")
	if cfg.Kubeconfig == "" {
		t.Skip("KUBECONFIG must be set")
	}

	// Provision-time registries.yaml: we only check that the file
	// is staged correctly, so the mirror endpoint can be invalid.
	cfg.Registries = config.Registries{
		Configs: map[string]config.RegistryConfig{
			"y-cluster-e2e-mirror.invalid": {
				Auth: &config.RegistryAuth{
					Username: "oauth2accesstoken",
					Password: "literal-test-token",
				},
			},
		},
	}

	ctx := context.Background()

	cluster, err := qemu.Provision(ctx, cfg, logger)
	if err != nil {
		t.Fatal(err)
	}

	// SSH works
	out, err := cluster.SSH(ctx, "hostname")
	if err != nil {
		t.Fatalf("SSH failed: %v", err)
	}
	t.Logf("hostname: %s", out)

	// k3s is up: kubectl through the merged kubeconfig sees a node.
	kc := exec.CommandContext(ctx, "kubectl", "--context="+cfg.Context, "get", "nodes", "--no-headers")
	kc.Env = append(os.Environ(), "KUBECONFIG="+cfg.Kubeconfig)
	kout, err := kc.CombinedOutput()
	if err != nil {
		t.Fatalf("kubectl get nodes: %s: %v", kout, err)
	}
	if !strings.Contains(string(kout), "Ready") {
		t.Fatalf("node not Ready: %s", kout)
	}

	// detect / ctr / crictl must work against the running VM
	// via the merged kubeconfig context.
	assertClusterFeatures(t, cfg.Context, "qemu")

	// Registries provision-time write reaches the VM at the
	// canonical k3s path with the expected body.
	regOut, err := cluster.SSH(ctx, "sudo cat /etc/rancher/k3s/registries.yaml")
	if err != nil {
		t.Fatalf("read registries.yaml: %s: %v", regOut, err)
	}
	for _, want := range []string{"y-cluster-e2e-mirror.invalid", "literal-test-token", "oauth2accesstoken"} {
		if !strings.Contains(string(regOut), want) {
			t.Fatalf("registries.yaml missing %q.\nGot:\n%s", want, regOut)
		}
	}

	// Capture the VM pid before teardown so we can assert it's
	// actually gone. Regression guard: an earlier teardown reported
	// success while the qemu process kept running and held its host
	// port forwards, blocking the next provision.
	vmPid := readPid(t, cfg.CacheDir, cfg.Name)

	if err := cluster.Teardown(false); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(cluster.DiskPath()); err == nil {
		t.Fatal("disk should be deleted after teardown with keepDisk=false")
	}
	if _, err := os.Stat(filepath.Join(cfg.CacheDir, cfg.Name+".pid")); !os.IsNotExist(err) {
		t.Fatalf("pidfile should be removed after teardown; stat err=%v", err)
	}
	assertPidGone(t, vmPid)
}

func TestQemu_TeardownKeepDisk(t *testing.T) {
	if _, err := os.Stat("/dev/kvm"); err != nil {
		t.Skip("QEMU tests require /dev/kvm")
	}
	if err := qemu.CheckPrerequisites(); err != nil {
		t.Skip(err)
	}

	logger, _ := zap.NewDevelopment()
	cfg := e2eQEMURuntime()
	cfg.Name = "y-cluster-e2e-keepdisk"
	cfg.Context = "y-cluster-e2e-keepdisk"
	cfg.CacheDir = t.TempDir()
	cfg.Memory = "4096"
	cfg.CPUs = "2"
	cfg.SSHPort = "2225"
	cfg.PortForwards = e2eUniqueForwards("26444")
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
	cfg := e2eQEMURuntime()
	cfg.Name = "y-cluster-e2e-export"
	cfg.Context = "y-cluster-e2e-export"
	cfg.CacheDir = t.TempDir()
	cfg.Memory = "4096"
	cfg.CPUs = "2"
	cfg.SSHPort = "2224"
	cfg.PortForwards = e2eUniqueForwards("26445")
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
