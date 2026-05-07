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
//
// DiskSize is bumped from the 20G default to 40G: appliance e2e
// flows install workloads, build a seed tarball, prepare-export,
// and re-boot from the prepared disk -- the cumulative footprint
// pushes the 20G disk into pressure on the kubelet's image-gc
// thresholds, which surfaces as flaky pod evictions mid-test.
// 40G is well clear of that ceiling and the qcow2 sparse layout
// means the host-disk footprint only grows with actual usage.
func e2eQEMURuntime() qemu.Config {
	c := &config.QEMUConfig{CommonConfig: config.CommonConfig{Provider: config.ProviderQEMU}}
	c.ApplyDefaults()
	c.DiskSize = "40G"
	return qemu.FromConfig(c)
}

// e2eUniqueForwards builds a port-forward list that won't collide
// with another e2e test running on the same machine. Two forwards:
//
//   - apiPort -> guest 6443: required for Provision to extract a
//     working kubeconfig from the booted VM's k3s API.
//   - httpPort -> guest 80: required so any setup script that pokes
//     the gateway's HTTP listener (e.g. `curl 127.0.0.1:<httpPort>/...`
//     against an HTTPRoute / GRPCRoute the test installs) reaches
//     the VM. Several Yolean dev scripts assume this forward exists.
func e2eUniqueForwards(apiPort, httpPort string) []qemu.PortForward {
	return []qemu.PortForward{
		{Host: apiPort, Guest: "6443"},
		{Host: httpPort, Guest: "80"},
	}
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
	cfg.PortForwards = e2eUniqueForwards("26443", "28443")
	cfg.Kubeconfig = os.Getenv("KUBECONFIG")
	if cfg.Kubeconfig == "" {
		t.Skip("KUBECONFIG must be set")
	}
	// Point qemuRunning at the test's isolated cache so the
	// y-cluster binary spawned by detect/ctr/crictl finds the VM
	// we just provisioned (default lookup path is ~/.cache/y-cluster-qemu).
	t.Setenv("Y_CLUSTER_QEMU_CACHE_DIR", cfg.CacheDir)

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
	cfg.PortForwards = e2eUniqueForwards("26444", "28444")
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
	cfg.PortForwards = e2eUniqueForwards("26445", "28445")
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

	// Export to VMDK via the bundle-shaped API. The bundle dir
	// gets <name>.vmdk, <name>-ssh{,.pub}, and a README.md.
	bundleDir := filepath.Join(cfg.CacheDir, "bundle")
	if err := qemu.Export(ctx, qemu.ExportOptions{
		CacheDir:  cfg.CacheDir,
		Name:      cfg.Name,
		BundleDir: bundleDir,
		Format:    qemu.FormatVMDK,
		Logger:    logger,
	}); err != nil {
		t.Fatal(err)
	}
	vmdkPath := filepath.Join(bundleDir, cfg.Name+".vmdk")
	if _, err := os.Stat(vmdkPath); err != nil {
		t.Fatalf("VMDK should exist after export: %v", err)
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

// TestQemu_StopStart provisions, stops the VM, asserts disk +
// state sidecar are preserved while the pidfile is gone, then
// starts the VM and asserts kubectl still works against the
// merged context.
//
// This is the regression guard for the appliance lifecycle: a
// stop/start round-trip must produce an indistinguishable cluster
// (same kubeconfig context, same workloads in etcd, same node
// IP from the host's perspective).
func TestQemu_StopStart(t *testing.T) {
	if _, err := os.Stat("/dev/kvm"); err != nil {
		t.Skip("QEMU tests require /dev/kvm")
	}
	if err := qemu.CheckPrerequisites(); err != nil {
		t.Skip(err)
	}

	logger, _ := zap.NewDevelopment()
	cfg := e2eQEMURuntime()
	cfg.Name = "y-cluster-e2e-stopstart"
	cfg.Context = "y-cluster-e2e-stopstart"
	cfg.CacheDir = t.TempDir()
	cfg.Memory = "4096"
	cfg.CPUs = "2"
	cfg.SSHPort = "2226"
	cfg.PortForwards = e2eUniqueForwards("26446", "28446")
	cfg.Kubeconfig = os.Getenv("KUBECONFIG")
	if cfg.Kubeconfig == "" {
		t.Skip("KUBECONFIG must be set")
	}
	t.Setenv("Y_CLUSTER_QEMU_CACHE_DIR", cfg.CacheDir)

	ctx := context.Background()

	cluster, err := qemu.Provision(ctx, cfg, logger)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cluster.Teardown(false) })

	// Sanity: kubectl works against the freshly-provisioned cluster.
	assertNodeReady(t, cfg.Context, cfg.Kubeconfig)

	// Sidecar landed at provision time -- start needs it.
	if _, err := os.Stat(filepath.Join(cfg.CacheDir, cfg.Name+".json")); err != nil {
		t.Fatalf("state sidecar missing after Provision: %v", err)
	}

	// Stop. Pidfile should be gone, disk + sidecar preserved.
	vmPid := readPid(t, cfg.CacheDir, cfg.Name)
	if err := qemu.Stop(cfg.CacheDir, cfg.Name, logger); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	if _, err := os.Stat(filepath.Join(cfg.CacheDir, cfg.Name+".pid")); !os.IsNotExist(err) {
		t.Fatalf("pidfile should be gone after Stop; stat err=%v", err)
	}
	if _, err := os.Stat(cluster.DiskPath()); err != nil {
		t.Fatalf("disk should be preserved after Stop: %v", err)
	}
	if _, err := os.Stat(filepath.Join(cfg.CacheDir, cfg.Name+".json")); err != nil {
		t.Fatalf("state sidecar should be preserved after Stop: %v", err)
	}
	assertPidGone(t, vmPid)

	// Start from the saved sidecar. Should not need cfg passed in
	// -- everything required is on disk under cfg.CacheDir.
	cluster2, err := qemu.Start(ctx, cfg.CacheDir, cfg.Name, logger)
	if err != nil {
		t.Fatalf("Start: %v", err)
	}

	// kubectl works again. The cluster came back with all workloads
	// intact (etcd is on the qcow2 disk, not in RAM).
	assertNodeReady(t, cfg.Context, cfg.Kubeconfig)

	// SSH still works against the freshly-resumed VM.
	if out, err := cluster2.SSH(ctx, "hostname"); err != nil {
		t.Fatalf("SSH after Start: %s: %v", out, err)
	}
}

// TestQemu_Seed_GateAndBypass exercises the data-seed mount-required
// gate end-to-end against a real qemu boot:
//
//   - Provision a build cluster, plant a sentinel file under
//     /data/yolean so the seed has something verifiable.
//   - Stop, prepare-export (bakes the LABEL=y-cluster-data fstab
//     entry, generates the seed tarball, lays down the systemd unit).
//   - Boot the prepared disk in diagnostic mode -- StartForDiagnostic
//     gives us a *Cluster without waiting for k3s, which we expect
//     not to come up because no labeled volume is attached.
//   - Assert: sshd works, the seed unit is in `failed` state, the
//     journal mentions "not a mountpoint", k3s.service is NOT active.
//     This is the regression posture for the GCP-appliance failure
//     where the customer's volume mounted after k3s.
//   - Inject /run/y-cluster-seed-bypass (the cloud-init-style hosting
//     override; in this test we touch it directly via SSH) and
//     restart the seed unit. Assert the seed extract ran, the
//     sentinel is back under /data/yolean, the bypass sibling
//     sentinel is present, and k3s reaches Ready after a restart.
//
// Covers states 3 (no volume, no bypass -> fail), 4 (no volume +
// bypass -> extract), and 7 (sshd unaffected by seed failure) of
// the 7-state seed-check matrix; states 1, 2, 5, 6 are unit-tested
// via the embedded shell script under pkg/provision/qemu.
func TestQemu_Seed_GateAndBypass(t *testing.T) {
	if _, err := os.Stat("/dev/kvm"); err != nil {
		t.Skip("QEMU tests require /dev/kvm")
	}
	if err := qemu.CheckPrerequisites(); err != nil {
		t.Skip(err)
	}
	if _, err := exec.LookPath("virt-customize"); err != nil {
		t.Skip("virt-customize not on PATH; install libguestfs-tools")
	}

	logger, _ := zap.NewDevelopment()
	cfg := e2eQEMURuntime()
	cfg.Name = "y-cluster-e2e-seed-gate"
	cfg.Context = "y-cluster-e2e-seed-gate"
	cfg.CacheDir = t.TempDir()
	cfg.Memory = "4096"
	cfg.CPUs = "2"
	cfg.SSHPort = "2227"
	cfg.PortForwards = e2eUniqueForwards("26447", "28447")
	cfg.Kubeconfig = os.Getenv("KUBECONFIG")
	if cfg.Kubeconfig == "" {
		t.Skip("KUBECONFIG must be set")
	}
	t.Setenv("Y_CLUSTER_QEMU_CACHE_DIR", cfg.CacheDir)

	ctx := context.Background()

	// 1. Build the appliance.
	cluster, err := qemu.Provision(ctx, cfg, logger)
	if err != nil {
		t.Fatal(err)
	}
	// Cleanup runs even on test failure; teardown removes the disk +
	// pidfile + ssh key. Idempotent against the second VM (cluster2)
	// because it shares CacheDir/Name.
	t.Cleanup(func() { _ = cluster.Teardown(false) })

	// 2. Plant a sentinel under /data/yolean. PrepareExport's
	//    BuildSeedAssets snapshots /data/yolean into the tarball; the
	//    sentinel proves end-to-end that the bypass branch's extract
	//    actually wrote the build-time data back onto the customer
	//    side.
	if out, err := cluster.SSH(ctx, "sudo mkdir -p /data/yolean && echo seed-sentinel-v1 | sudo tee /data/yolean/sentinel.txt >/dev/null"); err != nil {
		t.Fatalf("plant sentinel: %s: %v", out, err)
	}

	// 3. Stop the build cluster cleanly.
	if err := qemu.Stop(cfg.CacheDir, cfg.Name, logger); err != nil {
		t.Fatalf("Stop: %v", err)
	}

	// 4. prepare-export: bake fstab + seed tarball + systemd unit.
	if err := qemu.PrepareExport(ctx, cfg.CacheDir, cfg.Name, logger); err != nil {
		t.Fatalf("PrepareExport: %v", err)
	}

	// 5. Boot in diagnostic mode. k3s won't come up because the seed
	//    unit will fail (no volume attached, no bypass). The Cluster
	//    handle still gives us SSH against the running VM.
	cluster2, err := qemu.StartForDiagnostic(ctx, cfg.CacheDir, cfg.Name, logger)
	if err != nil {
		t.Fatalf("StartForDiagnostic: %v", err)
	}

	// 6. SSH works -- sshd has no transitive dep on the seed unit.
	if out, err := cluster2.SSH(ctx, "hostname"); err != nil {
		t.Fatalf("SSH after diagnostic boot (sshd should be unaffected by seed failure): %s: %v", out, err)
	}

	// 7. Wait for the seed unit to settle. It's oneshot Before=k3s.service,
	//    runs early; expect it to be `failed` once cloud-init.service has
	//    completed and the gate has fired.
	if state := waitForSeedState(t, ctx, cluster2, "failed", 90*time.Second); state != "failed" {
		out, _ := cluster2.SSH(ctx, "sudo journalctl -u y-cluster-data-seed.service -b --no-pager")
		t.Fatalf("seed unit never reached failed; last state=%q\njournal:\n%s", state, out)
	}

	// 8. Journal carries the actionable error.
	journalOut, err := cluster2.SSH(ctx, "sudo journalctl -u y-cluster-data-seed.service -b --no-pager")
	if err != nil {
		t.Fatalf("journalctl: %v", err)
	}
	if !strings.Contains(string(journalOut), "not a mountpoint") {
		t.Errorf("journal missing 'not a mountpoint':\n%s", journalOut)
	}
	if !strings.Contains(string(journalOut), "LABEL=y-cluster-data") {
		t.Errorf("journal missing LABEL hint (resolution recipe):\n%s", journalOut)
	}

	// 9. k3s must NOT be active. Per the drop-in `Requires=` on the
	//    failed seed unit, k3s.service stays in "inactive (deps
	//    failed)" or similar.
	k3sOut, _ := cluster2.SSH(ctx, "systemctl is-active k3s.service")
	if state := strings.TrimSpace(string(k3sOut)); state == "active" {
		t.Fatalf("k3s.service should not be active when seed gate fires; got: %q", state)
	}

	// === Bypass path ===

	// 10. Inject the bypass flag the way Hetzner QA cloud-init would,
	//     except via SSH for test simplicity. /run is tmpfs.
	if out, err := cluster2.SSH(ctx, "sudo touch /run/y-cluster-seed-bypass"); err != nil {
		t.Fatalf("touch bypass flag: %s: %v", out, err)
	}

	// 11. Reset the failed state and restart the seed unit. With the
	//     bypass file in place, the script extracts regardless of
	//     mount status and exits 0.
	if out, err := cluster2.SSH(ctx, "sudo systemctl reset-failed y-cluster-data-seed.service && sudo systemctl restart y-cluster-data-seed.service"); err != nil {
		t.Fatalf("restart seed unit after bypass: %s: %v", out, err)
	}
	if state := waitForSeedState(t, ctx, cluster2, "active", 60*time.Second); state != "active" {
		out, _ := cluster2.SSH(ctx, "sudo journalctl -u y-cluster-data-seed.service -b --no-pager")
		t.Fatalf("seed unit never reached active after bypass; last state=%q\njournal:\n%s", state, out)
	}

	// 12. Sentinel from build time must be back under /data/yolean
	//     (extracted from the seed tarball into the boot-disk dir).
	sentOut, err := cluster2.SSH(ctx, "cat /data/yolean/sentinel.txt")
	if err != nil {
		t.Fatalf("read sentinel after bypass extract: %v", err)
	}
	if !strings.Contains(string(sentOut), "seed-sentinel-v1") {
		t.Errorf("seed extract didn't restore sentinel; got: %s", sentOut)
	}

	// 13. Bypass sibling-sentinel marks "we went down the bypass path"
	//     for forensic visibility.
	if out, err := cluster2.SSH(ctx, "test -f /data/yolean/.y-cluster-seeded-via-bypass && echo present"); err != nil {
		t.Errorf("bypass sentinel missing: %s: %v", out, err)
	} else if !strings.Contains(string(out), "present") {
		t.Errorf("bypass sentinel not present: %s", out)
	}

	// 14. k3s.service's Requires is now satisfied; restart should
	//     bring it up.
	if out, err := cluster2.SSH(ctx, "sudo systemctl reset-failed k3s.service 2>/dev/null; sudo systemctl restart k3s.service"); err != nil {
		t.Fatalf("restart k3s after bypass: %s: %v", out, err)
	}

	// 15. Wait for k3s to be Ready via in-VM kubectl (we don't import
	//     the kubeconfig in diagnostic mode, so use guest-side kubectl).
	deadline := time.Now().Add(3 * time.Minute)
	for time.Now().Before(deadline) {
		nodesOut, _ := cluster2.SSH(ctx, "sudo k3s kubectl get nodes --no-headers 2>/dev/null || true")
		if strings.Contains(string(nodesOut), "Ready") {
			return
		}
		time.Sleep(3 * time.Second)
	}
	out, _ := cluster2.SSH(ctx, "sudo journalctl -u k3s.service -b --no-pager | tail -50")
	t.Fatalf("k3s never reached Ready after bypass+restart\nk3s journal tail:\n%s", out)
}

// waitForSeedState polls `systemctl is-active y-cluster-data-seed.service`
// against the VM until it reports `want` or the timeout fires. Returns
// the last observed state so the caller can include it in the failure
// message.
func waitForSeedState(t *testing.T, ctx context.Context, cluster *qemu.Cluster, want string, timeout time.Duration) string {
	t.Helper()
	deadline := time.Now().Add(timeout)
	last := ""
	for time.Now().Before(deadline) {
		out, _ := cluster.SSH(ctx, "systemctl is-active y-cluster-data-seed.service 2>/dev/null || true")
		last = strings.TrimSpace(string(out))
		if last == want {
			return last
		}
		time.Sleep(2 * time.Second)
	}
	return last
}

// assertNodeReady polls `kubectl get nodes` against ctx until at
// least one Ready node is reported, up to 2 minutes. Shared by
// the lifecycle e2e legs.
func assertNodeReady(t *testing.T, ctxName, kcfgPath string) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Minute)
	for time.Now().Before(deadline) {
		kc := exec.Command("kubectl", "--context="+ctxName, "get", "nodes", "--no-headers")
		kc.Env = append(os.Environ(), "KUBECONFIG="+kcfgPath)
		out, err := kc.CombinedOutput()
		if err == nil && strings.Contains(string(out), "Ready") {
			return
		}
		time.Sleep(2 * time.Second)
	}
	t.Fatal("node never reached Ready within 2 minutes")
}
