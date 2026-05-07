//go:build e2e && kvm

package e2e

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"go.uber.org/zap"

	"github.com/Yolean/y-cluster/pkg/provision/qemu"
)

// TestQemu_PrepareExport_GracefulShutdown covers the "y-cluster
// stop -> prepare-export gives workloads their full
// terminationGracePeriodSeconds before snapshotting" property.
//
// Without this, a workload whose final-state write happens in its
// SIGTERM handler (mariadb's grastate.dat being the canonical
// example -- a missing grastate.dat in the seed bundle puts the
// customer's first boot into Galera force-bootstrap and
// CrashLoopBackOff) loses that final state from the seed bundle,
// and a customer-side first boot from the seed misses it.
//
// The synthetic workload sleeps 15s in its SIGTERM handler while
// writing one marker per second to a local-path PVC. Local-path
// PVs land under /data/yolean, which prepare-export packs into
// /var/lib/y-cluster/data-seed.tar.zst. The test cracks open the
// tarball post-export via guestfish and asserts step-15.txt +
// done.txt are present, which proves the kubelet honored the full
// 30s terminationGracePeriodSeconds across the cluster shutdown.
//
// Failure modes the test surfaces:
//   - SIGTERM not delivered to pods on `y-cluster stop` ->
//     no markers past started.txt.
//   - Grace period cut short (kubelet kill at <15s) ->
//     step-N.txt for some N<15, no step-15.txt, no done.txt.
//   - Test workload's PVC didn't reach /data/yolean ->
//     started.txt timeout in the wait loop, fail before stop.
func TestQemu_PrepareExport_GracefulShutdown(t *testing.T) {
	if _, err := os.Stat("/dev/kvm"); err != nil {
		t.Skip("QEMU tests require /dev/kvm")
	}
	if err := qemu.CheckPrerequisites(); err != nil {
		t.Skip(err)
	}
	if _, err := exec.LookPath("virt-customize"); err != nil {
		t.Skip("virt-customize not on PATH; install libguestfs-tools")
	}
	if _, err := exec.LookPath("guestfish"); err != nil {
		t.Skip("guestfish not on PATH; install libguestfs-tools")
	}
	if _, err := exec.LookPath("zstd"); err != nil {
		t.Skip("zstd not on PATH")
	}

	logger, _ := zap.NewDevelopment()
	cfg := e2eQEMURuntime()
	cfg.Name = "y-cluster-e2e-graceful"
	cfg.Context = "y-cluster-e2e-graceful"
	cfg.CacheDir = t.TempDir()
	cfg.Memory = "4096"
	cfg.CPUs = "2"
	cfg.SSHPort = "2229"
	cfg.PortForwards = e2eUniqueForwards("26449", "28449")
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

	// Apply the test workload manifest. Bundled
	// local-path-provisioner creates the PV under
	// /data/yolean/<ns>_<pvc>-<uid>/, and the Pod's marker writes
	// land there.
	if err := kubectlApply(ctx, cfg.Context, cfg.Kubeconfig,
		"../testdata/prepare-export-signaling/deployment.yaml"); err != nil {
		t.Fatalf("kubectl apply: %v", err)
	}

	// Wait for the workload to be Available. kubectl wait short-
	// circuits the existing kubelet ordering: by the time it
	// returns, the Pod is Ready and the trap is installed.
	if err := kubectlWaitAvailable(ctx, cfg.Context, cfg.Kubeconfig,
		"prepare-export-signaling", "deployment/shutdown-tester",
		3*time.Minute); err != nil {
		out, _ := cluster.SSH(ctx,
			"sudo k3s kubectl -n prepare-export-signaling get pods,pvc,events 2>&1")
		t.Fatalf("workload not Available: %v\ncluster state:\n%s", err, out)
	}

	// Wait for the workload to actually write its startup marker.
	// Available != bytes-on-disk; the trap-arming + first
	// `date > started.txt` runs after the Ready probe passes.
	startedPath := waitForStartedMarker(t, ctx, cluster, 60*time.Second)
	if startedPath == "" {
		out, _ := cluster.SSH(ctx, "sudo find /data/yolean -path '*prepare-export-signaling*' -ls 2>&1")
		t.Fatalf("started.txt never appeared after 60s\nfind output:\n%s", out)
	}
	pvDir := filepath.Dir(startedPath)
	t.Logf("workload PV dir on appliance: %s", pvDir)

	// Stop -- the path under test. systemd shuts down the
	// kubelet/containerd/k3s, which in turn SIGTERMs running
	// pods. The trap should run for 15s and write step-{1..15}.txt
	// + done.txt before the 30s terminationGracePeriodSeconds
	// expires.
	stopStart := time.Now()
	if err := qemu.Stop(cfg.CacheDir, cfg.Name, logger); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	t.Logf("Stop elapsed: %s", time.Since(stopStart))

	// prepare-export packs /data/yolean (which now contains the
	// post-shutdown markers) into /var/lib/y-cluster/data-seed.tar.zst.
	if err := qemu.PrepareExport(ctx, cfg.CacheDir, cfg.Name, logger); err != nil {
		t.Fatalf("PrepareExport: %v", err)
	}

	// Crack open the seed tarball via libguestfs. The disk has
	// been prepared (cluster is offline at this point), so we
	// copy the tarball out via guestfish directly.
	seedDest := t.TempDir()
	if out, err := exec.Command("guestfish",
		"--ro",
		"-a", cluster.DiskPath(),
		"-i",
		"copy-out", "/var/lib/y-cluster/data-seed.tar.zst", seedDest,
	).CombinedOutput(); err != nil {
		t.Fatalf("guestfish copy-out: %s: %v", out, err)
	}
	seedTarball := filepath.Join(seedDest, "data-seed.tar.zst")
	if _, err := os.Stat(seedTarball); err != nil {
		t.Fatalf("seed tarball not extracted: %v", err)
	}

	listCmd := exec.Command("sh", "-c",
		fmt.Sprintf("zstd -d --stdout %q | tar tf -",
			seedTarball))
	listOut, err := listCmd.CombinedOutput()
	if err != nil {
		t.Fatalf("inspect seed: %s: %v", listOut, err)
	}
	listing := string(listOut)

	// Required markers. step-15.txt missing means the trap got
	// killed before its loop completed; done.txt missing means
	// the trap ran but didn't reach its final write.
	for _, want := range []string{
		"prepare-export-signaling_markers",
		"started.txt",
		"step-15.txt",
		"done.txt",
	} {
		if !strings.Contains(listing, want) {
			t.Errorf("seed bundle missing %q\nfull listing:\n%s", want, listing)
		}
	}

	// Diagnostic: how many step markers actually made it. A
	// healthy run logs 15/15. A truncated run shows where the
	// kubelet pulled the rug.
	stepRE := regexp.MustCompile(`/step-(\d+)\.txt`)
	matches := stepRE.FindAllStringSubmatch(listing, -1)
	t.Logf("step markers in seed: %d/15", len(matches))
}

// kubectlApply runs `kubectl --context=<ctx> apply -f <file>`
// against the e2e cluster, with KUBECONFIG passed via env (the
// rest of qemu_test.go uses that pattern; mirror it here).
func kubectlApply(ctx context.Context, ctxName, kcfgPath, file string) error {
	cmd := exec.CommandContext(ctx, "kubectl",
		"--context="+ctxName,
		"apply", "-f", file)
	cmd.Env = append(os.Environ(), "KUBECONFIG="+kcfgPath)
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("%s: %w", out, err)
	}
	return nil
}

// kubectlWaitAvailable blocks until the named Deployment reports
// condition=Available or the timeout fires.
func kubectlWaitAvailable(ctx context.Context, ctxName, kcfgPath, namespace, target string, timeout time.Duration) error {
	cmd := exec.CommandContext(ctx, "kubectl",
		"--context="+ctxName,
		"-n", namespace,
		"wait", "--for=condition=available",
		"--timeout="+timeout.String(),
		target)
	cmd.Env = append(os.Environ(), "KUBECONFIG="+kcfgPath)
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("%s: %w", out, err)
	}
	return nil
}

// waitForStartedMarker polls in-VM for the test workload's
// /markers/started.txt until it appears or timeout. Returns the
// absolute on-disk path to the marker (under /data/yolean), or
// empty on timeout.
func waitForStartedMarker(t *testing.T, ctx context.Context, cluster *qemu.Cluster, timeout time.Duration) string {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		out, err := cluster.SSH(ctx,
			"sudo find /data/yolean -name started.txt -path '*prepare-export-signaling*' 2>/dev/null | head -1")
		if err == nil {
			line := strings.TrimSpace(string(out))
			if line != "" {
				return line
			}
		}
		time.Sleep(2 * time.Second)
	}
	return ""
}
