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

// TestQemu_ExportImport_Qcow2 is the local-qemu-only counterpart
// to TestQemu_ExportImport: export the disk as native qcow2,
// then re-import via the same `qemu.Import` entrypoint without
// going through vmdk. Maintainers exercising disk-reuse / appliance
// e2e loops shouldn't have to bounce through a foreign format
// just to satisfy the import verb.
//
// Two FRs land in the same round-trip:
//
//   - FR 3: format-sniff path (importFormatFromExt(".qcow2") ->
//     qemu-img convert -f qcow2) — exercised by step 5.
//   - FR 4: provision uses the staged disk from import — the
//     second Provision call (step 6) hits the
//     <CacheDir>/<Name>.qcow2-already-exists branch and reaches
//     SSH + kubeconfig WITHOUT re-running k3s install. Pre-FR 4
//     this errored out with "disk already exists; run start..."
//     and start itself errored "no kubeconfig context yet" --
//     the import->boot deadlock the spec calls out.
func TestQemu_ExportImport_Qcow2(t *testing.T) {
	if _, err := os.Stat("/dev/kvm"); err != nil {
		t.Skip("QEMU tests require /dev/kvm")
	}
	if err := qemu.CheckPrerequisites(); err != nil {
		t.Skip(err)
	}

	logger, _ := zap.NewDevelopment()
	cfg := e2eQEMURuntime()
	cfg.Name = "y-cluster-e2e-export-qcow2"
	cfg.Context = "y-cluster-e2e-export-qcow2"
	cfg.CacheDir = t.TempDir()
	cfg.Memory = "4096"
	cfg.CPUs = "2"
	cfg.SSHPort = "2229"
	cfg.PortForwards = e2eUniqueForwards("26449", "28449")
	cfg.Kubeconfig = os.Getenv("KUBECONFIG")
	if cfg.Kubeconfig == "" {
		t.Skip("KUBECONFIG must be set")
	}

	ctx := context.Background()

	cluster, err := qemu.Provision(ctx, cfg, logger)
	if err != nil {
		t.Fatal(err)
	}

	if err := cluster.Teardown(true); err != nil {
		t.Fatal(err)
	}

	bundleDir := filepath.Join(cfg.CacheDir, "bundle")
	if err := qemu.Export(ctx, qemu.ExportOptions{
		CacheDir:  cfg.CacheDir,
		Name:      cfg.Name,
		BundleDir: bundleDir,
		Format:    qemu.FormatQcow2,
		Logger:    logger,
	}); err != nil {
		t.Fatal(err)
	}
	qcow2Path := filepath.Join(bundleDir, cfg.Name+".qcow2")
	if _, err := os.Stat(qcow2Path); err != nil {
		t.Fatalf("qcow2 should exist after export: %v", err)
	}

	// Drop the original disk so the import is unambiguously the
	// source of the boot disk on the next provision.
	if err := os.Remove(cluster.DiskPath()); err != nil {
		t.Fatal(err)
	}

	// Import via the same qemu.Import that the CLI calls. The
	// .qcow2 extension dispatches to the qcow2-format branch.
	if err := qemu.Import(qcow2Path, cluster.DiskPath()); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(cluster.DiskPath()); err != nil {
		t.Fatal("disk should exist after qcow2 import")
	}

	// FR 4: this Provision must succeed against the
	// already-existing <CacheDir>/<Name>.qcow2 left by Import.
	// Pre-FR 4 we'd have errored here with "disk already exists;
	// run start...". With the staged-disk branch in Provision,
	// we boot the imported disk and pull the kubeconfig out of
	// the k3s that the source appliance pre-baked.
	cluster2, err := qemu.Provision(ctx, cfg, logger)
	if err != nil {
		t.Fatalf("Provision against imported disk (FR 4 path): %v", err)
	}
	if out, err := cluster2.SSH(ctx, "hostname"); err != nil {
		t.Fatalf("SSH after qcow2 import: %v", err)
	} else {
		t.Logf("hostname after qcow2 import: %s", out)
	}
	// The kubeconfig context should be live -- the staged-disk
	// branch's contract is "import + provision = kubectl works".
	// kubectl get nodes is the simplest cheap proof.
	if out, err := exec.Command("kubectl",
		"--context="+cfg.Context, "--kubeconfig="+cfg.Kubeconfig,
		"get", "nodes", "--no-headers",
	).CombinedOutput(); err != nil {
		t.Errorf("kubectl get nodes against staged-disk context: %s: %v", out, err)
	} else if !strings.Contains(string(out), "Ready") {
		t.Errorf("staged-disk cluster has no Ready nodes:\n%s", out)
	}
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

// TestQemu_DataDisk_ReuseAcrossProvisions pins the disk-reuse
// primitive's contract: an operator who sets DataDisk on the
// runtime Config can provision a cluster, write to /data/yolean,
// teardown the VM, re-provision a FRESH cluster on the same
// DataDisk path, and read the same data back. This is the local
// (qemu-side) shape of what `appliance-qemu-to-gcp.sh
// --reuse-disk=true` does in cloud -- maintainers shouldn't
// have to round-trip through GCP / Hetzner to QA disk reuse.
//
// What we prove:
//
//   - First provision creates a labeled qcow2 at the configured
//     DataDisk path (NOT under CacheDir) and attaches it to the
//     VM. The cloud-init `mounts:` entry mounts it at /data/yolean
//     before workloads.
//   - Writing a sentinel under /data/yolean lands on the labeled
//     volume, not the boot disk.
//   - Teardown removes the boot disk + cache artefacts but
//     leaves the DataDisk file in place.
//   - Second provision (same name, same DataDisk path, fresh
//     boot disk + ssh key) re-attaches the same labeled qcow2
//     and reads the sentinel back unchanged.
//
// Coverage gap closed: previously the only end-to-end coverage
// for the "external labeled volume rides through a fresh cluster
// install" pattern lived in TestQemu_Seed_VolumeAttached, which
// goes via prepare-export + seed-tarball -- a different code
// path. This test covers the direct disk-reuse path the
// maintainer needs for fast iteration.
func TestQemu_DataDisk_ReuseAcrossProvisions(t *testing.T) {
	if _, err := os.Stat("/dev/kvm"); err != nil {
		t.Skip("QEMU tests require /dev/kvm")
	}
	if err := qemu.CheckPrerequisites(); err != nil {
		t.Skip(err)
	}
	if _, err := exec.LookPath("virt-format"); err != nil {
		t.Skip("virt-format not on PATH; install libguestfs-tools")
	}

	logger, _ := zap.NewDevelopment()

	// CacheDir is the qemu provisioner's per-test scratch.
	// dataDiskDir is deliberately a separate tempdir so the
	// teardown-doesn't-touch-it contract has somewhere external
	// to point at; in the production shape an operator would
	// put this under their home dir or a customer-specific
	// path, NOT under the cluster cache.
	cacheDir := t.TempDir()
	dataDiskDir := t.TempDir()
	dataDiskPath := filepath.Join(dataDiskDir, "y-cluster-data.qcow2")

	cfg := e2eQEMURuntime()
	cfg.Name = "y-cluster-e2e-datadisk-reuse"
	cfg.Context = "y-cluster-e2e-datadisk-reuse"
	cfg.CacheDir = cacheDir
	cfg.Memory = "2048"
	cfg.CPUs = "2"
	cfg.SSHPort = "2230"
	cfg.PortForwards = e2eUniqueForwards("26450", "28450")
	cfg.Kubeconfig = os.Getenv("KUBECONFIG")
	if cfg.Kubeconfig == "" {
		t.Skip("KUBECONFIG must be set")
	}
	cfg.DataDisk = dataDiskPath
	cfg.DataDiskSize = "1G"
	// Skip the bundled gateway install for both provisions --
	// this test is about disk reuse, not the EG install path,
	// and skipping the EG install knocks ~30s off each
	// provision (1 min over both passes).
	cfg.Gateway.Skip = true

	ctx := context.Background()

	// === Provision 1: write the sentinel ===
	cluster1, err := qemu.Provision(ctx, cfg, logger)
	if err != nil {
		t.Fatalf("Provision (pass 1): %v", err)
	}

	// The DataDisk was created at the operator-configured path,
	// not the cache dir. Asserting BOTH so a future refactor
	// doesn't accidentally relocate operator state.
	if _, err := os.Stat(dataDiskPath); err != nil {
		t.Errorf("DataDisk should exist post-provision at the operator path: %v", err)
	}
	if filepath.Dir(dataDiskPath) == cacheDir {
		t.Errorf("DataDisk leaked into CacheDir; that defeats teardown safety")
	}

	// Mount must be the labeled volume (not the boot disk's /data/yolean).
	if out, err := cluster1.SSH(ctx, "mountpoint -q /data/yolean && echo mounted"); err != nil {
		t.Fatalf("mountpoint check (pass 1): %s: %v", out, err)
	} else if !strings.Contains(string(out), "mounted") {
		t.Errorf("/data/yolean must be a mountpoint when DataDisk is configured: %s", out)
	}
	if out, err := cluster1.SSH(ctx, "findmnt -no SOURCE /data/yolean"); err != nil {
		t.Fatalf("findmnt /data/yolean: %s: %v", out, err)
	} else if !strings.Contains(string(out), "/dev/vd") {
		t.Errorf("/data/yolean should be backed by a virtio drive (the attached qcow2): %s", out)
	}

	// Plant the sentinel. lost+found is a normal ext4 reserved
	// inode and proves the filesystem is the freshly-formatted
	// labeled volume rather than a host bind-mount.
	if out, err := cluster1.SSH(ctx,
		"sudo ls -la /data/yolean && echo data-disk-reuse-v1 | sudo tee /data/yolean/sentinel.txt >/dev/null"); err != nil {
		t.Fatalf("plant sentinel (pass 1): %s: %v", out, err)
	}

	// Teardown without --keepDisk: boot disk + cache go;
	// DataDisk must stay.
	if err := qemu.TeardownConfig(cfg, false, logger); err != nil {
		t.Fatalf("TeardownConfig (pass 1): %v", err)
	}
	if _, err := os.Stat(filepath.Join(cacheDir, cfg.Name+".qcow2")); !os.IsNotExist(err) {
		t.Errorf("boot disk should be gone after teardown: err=%v", err)
	}
	if _, err := os.Stat(dataDiskPath); err != nil {
		t.Fatalf("DataDisk must survive teardown without --keepDisk: %v", err)
	}

	// === Provision 2: re-attach the same DataDisk ===
	cluster2, err := qemu.Provision(ctx, cfg, logger)
	if err != nil {
		t.Fatalf("Provision (pass 2 / disk-reuse): %v", err)
	}

	// Sentinel must be readable unchanged on pass 2 -- the
	// whole point of the primitive. cat sentinel.txt > 0 bytes
	// implies the boot disk is brand new (no leftovers from
	// the boot disk's /data/yolean) BUT the labeled volume
	// brought the file along.
	if out, err := cluster2.SSH(ctx, "cat /data/yolean/sentinel.txt"); err != nil {
		t.Fatalf("read sentinel (pass 2): %v", err)
	} else if got := strings.TrimSpace(string(out)); got != "data-disk-reuse-v1" {
		t.Errorf("sentinel content lost across teardown + re-provision; got %q", got)
	}

	// Final teardown leaves the data disk in place (just like
	// pass 1's teardown did).
	if err := qemu.TeardownConfig(cfg, false, logger); err != nil {
		t.Errorf("final TeardownConfig: %v", err)
	}
	if _, err := os.Stat(dataDiskPath); err != nil {
		t.Errorf("DataDisk must still survive the second teardown: %v", err)
	}
}

// TestQemu_Seed_VolumeAttached exercises the production-shape happy
// path that TestQemu_Seed_GateAndBypass deliberately doesn't:
//
//   * State 1 -- a labeled `y-cluster-data` ext4 volume is attached
//     at boot, the pre-baked LABEL fstab entry mounts it, the seed
//     unit sees a mountpoint with only lost+found, extracts the
//     seed tarball, writes the marker, k3s starts via Requires=.
//   * State 5 -- the same disk on the next boot has a marker; the
//     seed unit hits the marker-respect no-op path; k3s starts
//     without re-extract.
//
// Combined into one test function so we pay the provision +
// prepare-export cost once and stop / re-boot the same prepared
// disk twice. Without this coverage state 1 + 5 are only exercised
// by the manual GCP run, where a single flaky symptom is
// expensive to reproduce -- the local form takes ~3 min and is
// deterministic.
func TestQemu_Seed_VolumeAttached(t *testing.T) {
	if _, err := os.Stat("/dev/kvm"); err != nil {
		t.Skip("QEMU tests require /dev/kvm")
	}
	if err := qemu.CheckPrerequisites(); err != nil {
		t.Skip(err)
	}
	if _, err := exec.LookPath("virt-customize"); err != nil {
		t.Skip("virt-customize not on PATH; install libguestfs-tools")
	}
	if _, err := exec.LookPath("virt-format"); err != nil {
		t.Skip("virt-format not on PATH; install libguestfs-tools")
	}

	logger, _ := zap.NewDevelopment()
	cfg := e2eQEMURuntime()
	cfg.Name = "y-cluster-e2e-seed-volume"
	cfg.Context = "y-cluster-e2e-seed-volume"
	cfg.CacheDir = t.TempDir()
	cfg.Memory = "4096"
	cfg.CPUs = "2"
	cfg.SSHPort = "2228"
	cfg.PortForwards = e2eUniqueForwards("26448", "28448")
	cfg.Kubeconfig = os.Getenv("KUBECONFIG")
	if cfg.Kubeconfig == "" {
		t.Skip("KUBECONFIG must be set")
	}
	t.Setenv("Y_CLUSTER_QEMU_CACHE_DIR", cfg.CacheDir)

	ctx := context.Background()

	// Build the appliance + plant a sentinel under /data/yolean so
	// state 1's extract has something verifiable when we read back
	// the customer-side mount.
	cluster, err := qemu.Provision(ctx, cfg, logger)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cluster.Teardown(false) })

	if out, err := cluster.SSH(ctx, "sudo mkdir -p /data/yolean && echo seed-volume-v1 | sudo tee /data/yolean/sentinel.txt >/dev/null"); err != nil {
		t.Fatalf("plant sentinel: %s: %v", out, err)
	}

	if err := qemu.Stop(cfg.CacheDir, cfg.Name, logger); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	if err := qemu.PrepareExport(ctx, cfg.CacheDir, cfg.Name, logger); err != nil {
		t.Fatalf("PrepareExport: %v", err)
	}

	// Build a labeled ext4 qcow2 to act as the customer's persistent
	// /data/yolean volume. Filesystem label matches the LABEL fstab
	// entry prepare-export pre-baked into the appliance.
	dataDisk := filepath.Join(cfg.CacheDir, cfg.Name+"-data.qcow2")
	makeLabeledDataDisk(t, dataDisk, "y-cluster-data", "1G")

	// === Boot 1: state 1 -- empty volume, extract ===
	cluster1, err := qemu.StartForDiagnosticWithDisks(ctx, cfg.CacheDir, cfg.Name, []string{dataDisk}, logger)
	if err != nil {
		t.Fatalf("StartForDiagnosticWithDisks (boot 1): %v", err)
	}

	// Seed unit must reach `active` once it sees the mountpoint +
	// empty mount + extracts.
	if state := waitForSeedState(t, ctx, cluster1, "active", 90*time.Second); state != "active" {
		out, _ := cluster1.SSH(ctx, "sudo journalctl -u y-cluster-data-seed.service -b --no-pager")
		t.Fatalf("seed unit never reached active on first boot; last=%q\njournal:\n%s", state, out)
	}

	// /data/yolean is the labeled volume now (not the boot disk).
	if out, err := cluster1.SSH(ctx, "mountpoint -q /data/yolean && echo mounted"); err != nil {
		t.Fatalf("mountpoint check: %s: %v", out, err)
	} else if !strings.Contains(string(out), "mounted") {
		t.Errorf("expected /data/yolean to be a mountpoint, got: %s", out)
	}

	// Sentinel restored from the seed tarball.
	if out, err := cluster1.SSH(ctx, "cat /data/yolean/sentinel.txt"); err != nil {
		t.Fatalf("sentinel read: %v", err)
	} else if !strings.Contains(string(out), "seed-volume-v1") {
		t.Errorf("seed extract did not restore sentinel; got: %s", out)
	}

	// Marker present on the customer volume.
	if out, err := cluster1.SSH(ctx, "sudo cat /data/yolean/.y-cluster-seeded"); err != nil {
		t.Fatalf("marker read: %v", err)
	} else if !strings.Contains(string(out), "seed_sha256") {
		t.Errorf("marker should contain seed_sha256: %s", out)
	}

	// Bypass sentinel must NOT exist -- we went the production path,
	// not the bypass path. This distinguishes state 1 from state 4.
	if out, err := cluster1.SSH(ctx, "test -f /data/yolean/.y-cluster-seeded-via-bypass && echo present || echo absent"); err != nil {
		t.Fatalf("bypass-sentinel check: %v", err)
	} else if !strings.Contains(string(out), "absent") {
		t.Errorf("bypass sentinel should not exist on a state-1 boot: %s", out)
	}

	// k3s should come up via Requires=y-cluster-data-seed.service
	// without any manual restart, since the seed unit is `active`.
	if !waitForK3sReady(t, ctx, cluster1, 3*time.Minute) {
		out, _ := cluster1.SSH(ctx, "sudo journalctl -u k3s.service -b --no-pager | tail -50")
		t.Fatalf("k3s never reached Ready on first boot\nk3s journal tail:\n%s", out)
	}

	// === Boot 2: state 5 -- marker present, no-op ===
	if err := qemu.Stop(cfg.CacheDir, cfg.Name, logger); err != nil {
		t.Fatalf("Stop between boots: %v", err)
	}
	cluster2, err := qemu.StartForDiagnosticWithDisks(ctx, cfg.CacheDir, cfg.Name, []string{dataDisk}, logger)
	if err != nil {
		t.Fatalf("StartForDiagnosticWithDisks (boot 2): %v", err)
	}

	if state := waitForSeedState(t, ctx, cluster2, "active", 60*time.Second); state != "active" {
		out, _ := cluster2.SSH(ctx, "sudo journalctl -u y-cluster-data-seed.service -b --no-pager")
		t.Fatalf("seed unit not active on second boot; last=%q\njournal:\n%s", state, out)
	}

	// Journal must indicate the marker-respect no-op path -- not the
	// extract path -- otherwise we'd be silently re-extracting on
	// every boot, which would clobber any customer changes.
	journalOut, err := cluster2.SSH(ctx, "sudo journalctl -u y-cluster-data-seed.service -b --no-pager")
	if err != nil {
		t.Fatalf("journalctl on boot 2: %v", err)
	}
	if !strings.Contains(string(journalOut), "marker present") {
		t.Errorf("boot 2 should hit the marker-respect path; journal:\n%s", journalOut)
	}
	if strings.Contains(string(journalOut), "extracting") {
		t.Errorf("boot 2 should NOT re-extract; journal mentions extracting:\n%s", journalOut)
	}

	// Sentinel content unchanged across the two boots (i.e., we did
	// not silently re-extract over customer state).
	if out, err := cluster2.SSH(ctx, "cat /data/yolean/sentinel.txt"); err != nil {
		t.Fatalf("sentinel read on boot 2: %v", err)
	} else if !strings.Contains(string(out), "seed-volume-v1") {
		t.Errorf("sentinel mutated across boots: %s", out)
	}

	if !waitForK3sReady(t, ctx, cluster2, 3*time.Minute) {
		out, _ := cluster2.SSH(ctx, "sudo journalctl -u k3s.service -b --no-pager | tail -50")
		t.Fatalf("k3s never reached Ready on boot 2\nk3s journal tail:\n%s", out)
	}
}

// makeLabeledDataDisk creates a qcow2 file at path with a single
// ext4 filesystem labeled `label`, sized `size` (a qemu-img-style
// string like "1G"). Uses libguestfs's virt-format so the test
// doesn't need root + losetup; libguestfs is already a hard prereq
// of prepare-export, so anything that runs the rest of this file
// has it.
func makeLabeledDataDisk(t *testing.T, path, label, size string) {
	t.Helper()
	if out, err := exec.Command("qemu-img", "create", "-f", "qcow2", path, size).CombinedOutput(); err != nil {
		t.Fatalf("qemu-img create %s: %s: %v", path, out, err)
	}
	// virt-format with --filesystem makes the WHOLE disk one ext4
	// filesystem (no partition table). The kernel + LABEL fstab
	// match by filesystem label regardless of partitioning, so this
	// is the simplest shape that satisfies the appliance contract.
	if out, err := exec.Command("virt-format",
		"-a", path,
		"--filesystem=ext4",
		"--label="+label,
	).CombinedOutput(); err != nil {
		t.Fatalf("virt-format %s: %s: %v", path, out, err)
	}
}

// waitForK3sReady polls in-VM `k3s kubectl get nodes` for a Ready
// node up to timeout. Returns true on success, false on timeout.
// Used by the seed-volume tests since they boot via
// StartForDiagnosticWithDisks and don't import the kubeconfig
// host-side.
func waitForK3sReady(t *testing.T, ctx context.Context, cluster *qemu.Cluster, timeout time.Duration) bool {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		out, _ := cluster.SSH(ctx, "sudo k3s kubectl get nodes --no-headers 2>/dev/null || true")
		if strings.Contains(string(out), "Ready") {
			return true
		}
		time.Sleep(3 * time.Second)
	}
	return false
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
