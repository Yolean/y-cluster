//go:build e2e && kvm

package e2e

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"go.uber.org/zap"

	"github.com/Yolean/y-cluster/pkg/provision/qemu"
)

// TestQemu_BootDiskSwap_UpgradeAndRollback is APPLIANCE_MAINTENANCE.md
// Phase 3 as the customer performs it: power off, swap the appliance
// disk, keep the data drive, boot. Then the rollback the same document
// promises: swap the old appliance disk back in.
//
//	boot 1  appliance v(N),   empty data drive   -> seeds it
//	boot 2  appliance v(N+1), same data drive    -> upgrade
//	boot 3  appliance v(N),   same data drive    -> rollback
//
// The contract is about the data drive: whichever appliance disk
// boots, a marked drive is left exactly as found, k3s comes up on it,
// and what one appliance wrote the other one sees. The two appliance
// disks are copies of one prepared build, told apart by a file each
// boot writes to its own root filesystem; what differs between real
// releases (staged manifests, images) is outside this contract.
func TestQemu_BootDiskSwap_UpgradeAndRollback(t *testing.T) {
	if _, err := os.Stat("/dev/kvm"); err != nil {
		t.Skip("QEMU tests require /dev/kvm")
	}
	if err := qemu.CheckPrerequisites(); err != nil {
		t.Skip(err)
	}
	for _, bin := range []string{"virt-customize", "virt-format"} {
		if _, err := exec.LookPath(bin); err != nil {
			t.Skipf("%s not on PATH; install libguestfs-tools", bin)
		}
	}

	logger, _ := zap.NewDevelopment()
	cfg := e2eQEMURuntime()
	cfg.Name = "y-cluster-e2e-diskswap"
	cfg.Context = "y-cluster-e2e-diskswap"
	cfg.CacheDir = e2eQEMUCacheDir(t)
	cfg.Memory = "2048"
	cfg.CPUs = "2"
	cfg.SSHPort = "2240"
	cfg.PortForwards = e2eUniqueForwards("26469", "28469")
	cfg.Kubeconfig = os.Getenv("KUBECONFIG")
	if cfg.Kubeconfig == "" {
		t.Skip("KUBECONFIG must be set")
	}
	// The gateway is not what is under test.
	cfg.Gateway.Skip = true

	ctx := context.Background()
	cluster, err := qemu.Provision(ctx, cfg, logger)
	if err != nil {
		t.Fatalf("Provision: %v", err)
	}
	t.Cleanup(func() { _ = qemu.TeardownConfig(cfg, false, logger) })
	if out, err := cluster.SSH(ctx, "sudo mkdir -p /data/yolean && echo from-the-build | sudo tee /data/yolean/seeded.txt >/dev/null"); err != nil {
		t.Fatalf("plant build-time data: %s: %v", out, err)
	}
	if err := qemu.PrepareExport(ctx, cfg.CacheDir, cfg.Name, logger); err != nil {
		t.Fatalf("PrepareExport: %v", err)
	}

	// Two appliance disks from the one build. `attached` is the path
	// the VM boots from; swapping is renaming.
	attached := cluster.DiskPath()
	shelf := map[string]string{"vN": attached + ".vN", "vN+1": attached + ".vN+1"}
	if out, err := exec.Command("cp", "--sparse=always", attached, shelf["vN+1"]).CombinedOutput(); err != nil {
		t.Fatalf("copy the appliance disk: %s: %v", out, err)
	}
	current := "vN"
	swapTo := func(version string) {
		t.Helper()
		if err := os.Rename(attached, shelf[current]); err != nil {
			t.Fatal(err)
		}
		if err := os.Rename(shelf[version], attached); err != nil {
			t.Fatal(err)
		}
		current = version
	}

	dataDisk := filepath.Join(t.TempDir(), "customer-data.qcow2")
	makeLabeledDataDisk(t, dataDisk, "y-cluster-data", "1G")

	// boot starts the attached appliance disk with the data drive,
	// checks what the seed unit did, and returns once k3s is ready.
	boot := func(wantSeedLog, mustNotLog string) *qemu.Cluster {
		t.Helper()
		c, err := qemu.StartForDiagnosticWithDisks(ctx, cfg.CacheDir, cfg.Name, []string{dataDisk}, logger)
		if err != nil {
			t.Fatalf("boot %s: %v", current, err)
		}
		if state := waitForSeedState(t, ctx, c, "active", 2*time.Minute); state != "active" {
			out, _ := c.SSH(ctx, "sudo journalctl -u y-cluster-data-seed.service -b --no-pager")
			t.Fatalf("boot %s: seed unit is %q\n%s", current, state, out)
		}
		journal, err := c.SSH(ctx, "sudo journalctl -u y-cluster-data-seed.service -b --no-pager")
		if err != nil {
			t.Fatalf("boot %s: read seed journal: %v", current, err)
		}
		if !strings.Contains(string(journal), wantSeedLog) {
			t.Errorf("boot %s: seed journal lacks %q:\n%s", current, wantSeedLog, journal)
		}
		if mustNotLog != "" && strings.Contains(string(journal), mustNotLog) {
			t.Errorf("boot %s: seed journal has %q, the data drive was written over:\n%s", current, mustNotLog, journal)
		}
		if !waitForK3sReady(t, ctx, c, 4*time.Minute) {
			out, _ := c.SSH(ctx, "sudo journalctl -u k3s.service -b --no-pager | tail -50")
			t.Fatalf("boot %s: k3s never became ready\n%s", current, out)
		}
		return c
	}
	sh := func(c *qemu.Cluster, command string) string {
		t.Helper()
		out, err := c.SSH(ctx, command)
		if err != nil {
			t.Fatalf("on %s: %s: %s: %v", current, command, out, err)
		}
		return strings.TrimSpace(string(out))
	}
	powerOff := func() {
		t.Helper()
		if err := qemu.Stop(cfg.CacheDir, cfg.Name, logger); err != nil {
			t.Fatalf("power off %s: %v", current, err)
		}
	}
	// Which appliance disk is this? Each writes its name to its own
	// root filesystem on first boot and must find it, or nothing,
	// afterwards.
	claimRoot := func(c *qemu.Cluster) {
		t.Helper()
		got := sh(c, "if [ -f /etc/y-cluster-e2e-appliance ]; then cat /etc/y-cluster-e2e-appliance; fi")
		if got != "" && got != current {
			t.Fatalf("booted appliance disk %q, the test attached %q", got, current)
		}
		sh(c, "echo "+current+" | sudo tee /etc/y-cluster-e2e-appliance >/dev/null")
	}

	// Boot 1: first import. Empty drive, the seed is extracted.
	c := boot("extracting", "")
	claimRoot(c)
	if got := sh(c, "cat /data/yolean/seeded.txt"); got != "from-the-build" {
		t.Errorf("seeded data: %q", got)
	}
	sh(c, "echo since-first-import | sudo tee /data/yolean/written-by-vN.txt >/dev/null")
	powerOff()

	// Boot 2: upgrade. The drive is marked, so it is left alone.
	swapTo("vN+1")
	c = boot("marker present", "extracting")
	claimRoot(c)
	if got := sh(c, "cat /data/yolean/written-by-vN.txt"); got != "since-first-import" {
		t.Errorf("after upgrade, data written under v(N): %q", got)
	}
	sh(c, "echo since-upgrade | sudo tee /data/yolean/written-by-vN+1.txt >/dev/null")
	powerOff()

	// Boot 3: rollback. The old appliance finds the drive as the new
	// one left it.
	swapTo("vN")
	c = boot("marker present", "extracting")
	claimRoot(c)
	for file, want := range map[string]string{
		"seeded.txt":          "from-the-build",
		"written-by-vN.txt":   "since-first-import",
		"written-by-vN+1.txt": "since-upgrade",
	} {
		if got := sh(c, "cat '/data/yolean/"+file+"'"); got != want {
			t.Errorf("after rollback, %s: %q, want %q", file, got, want)
		}
	}
	powerOff()
}
