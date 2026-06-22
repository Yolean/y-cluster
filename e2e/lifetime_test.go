//go:build e2e && kvm

package e2e

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"go.uber.org/zap"

	"github.com/Yolean/y-cluster/pkg/provision/qemu"
)

// TestQemu_Lifetime exercises the local cost-control auto-expiry path
// against a real qemu boot:
//
//   - Provision with a lifetime budget; assert the deadline is armed
//     in the sidecar and anchored to roughly now+budget.
//   - Push the deadline into the past (the time-travel a real expiry
//     would reach naturally) and assert Expired() flips.
//   - Run the onExpiry action (stop) and assert the VM is down with
//     disk + sidecar preserved -- the cost is gone, the cluster is
//     resumable.
//   - Start and assert the deadline is re-anchored to this start (the
//     "count from when the cluster starts" guarantee), giving a fresh
//     budget rather than an already-expired one.
//
// The host timer (systemd-run/at) is unit-tested in pkg/lifetime;
// here we cover the runtime substance: persistence, expiry detection,
// the real stop action, and the start re-anchor.
func TestQemu_Lifetime(t *testing.T) {
	if _, err := os.Stat("/dev/kvm"); err != nil {
		t.Skip("QEMU tests require /dev/kvm")
	}
	if err := qemu.CheckPrerequisites(); err != nil {
		t.Skip(err)
	}

	logger, _ := zap.NewDevelopment()
	cfg := e2eQEMURuntime()
	cfg.Name = "y-cluster-e2e-lifetime"
	cfg.Context = "y-cluster-e2e-lifetime"
	cfg.CacheDir = t.TempDir()
	cfg.Memory = "4096"
	cfg.CPUs = "2"
	cfg.SSHPort = "2227"
	cfg.PortForwards = e2eUniqueForwards("26447", "28447")
	cfg.Kubeconfig = os.Getenv("KUBECONFIG")
	if cfg.Kubeconfig == "" {
		t.Skip("KUBECONFIG must be set")
	}
	cfg.Lifetime = "2h"
	cfg.OnExpiry = "stop"
	t.Setenv("Y_CLUSTER_QEMU_CACHE_DIR", cfg.CacheDir)

	ctx := context.Background()

	cluster, err := qemu.Provision(ctx, cfg, logger)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cluster.Teardown(false) })

	// Deadline armed at provision, anchored ~now+2h.
	ls, err := qemu.LoadLifetime(cfg.CacheDir, cfg.Name)
	if err != nil {
		t.Fatalf("LoadLifetime after provision: %v", err)
	}
	if !ls.Enabled() {
		t.Fatal("lifetime should be enabled after provision with maxRun set")
	}
	if ls.ExpiresAt.IsZero() {
		t.Fatal("ExpiresAt should be armed after provision")
	}
	if rem := time.Until(ls.ExpiresAt); rem < 90*time.Minute || rem > 2*time.Hour+5*time.Minute {
		t.Fatalf("provision deadline %s out of expected ~2h window (remaining %s)", ls.ExpiresAt, rem)
	}
	provisionDeadline := ls.ExpiresAt

	// Simulate the deadline elapsing: push it three hours into the
	// past so it is unambiguously due.
	if _, err := qemu.ExtendDeadline(cfg.CacheDir, cfg.Name, -3*time.Hour); err != nil {
		t.Fatalf("ExtendDeadline (negative, to force expiry): %v", err)
	}
	ls, err = qemu.LoadLifetime(cfg.CacheDir, cfg.Name)
	if err != nil {
		t.Fatal(err)
	}
	if !ls.Expired() {
		t.Fatalf("deadline %s should be expired after pushing it into the past", ls.ExpiresAt)
	}

	// The onExpiry action: stop. Pidfile gone, disk + sidecar kept.
	vmPid := readPid(t, cfg.CacheDir, cfg.Name)
	if err := qemu.Stop(cfg.CacheDir, cfg.Name, logger); err != nil {
		t.Fatalf("Stop (reap action): %v", err)
	}
	if _, err := os.Stat(filepath.Join(cfg.CacheDir, cfg.Name+".pid")); !os.IsNotExist(err) {
		t.Fatalf("pidfile should be gone after expiry stop; stat err=%v", err)
	}
	if _, err := os.Stat(cluster.DiskPath()); err != nil {
		t.Fatalf("disk should be preserved after expiry stop: %v", err)
	}
	if _, err := os.Stat(filepath.Join(cfg.CacheDir, cfg.Name+".json")); err != nil {
		t.Fatalf("state sidecar should be preserved after expiry stop: %v", err)
	}
	assertPidGone(t, vmPid)

	// Start re-anchors the deadline to now: a stopped-then-started
	// cluster gets a fresh budget, not the expired one we forced.
	cluster2, err := qemu.Start(ctx, cfg.CacheDir, cfg.Name, logger)
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	_ = cluster2
	assertNodeReady(t, cfg.Context, cfg.Kubeconfig)

	ls, err = qemu.LoadLifetime(cfg.CacheDir, cfg.Name)
	if err != nil {
		t.Fatal(err)
	}
	if ls.Expired() {
		t.Fatal("deadline should be re-anchored (not expired) after Start")
	}
	if !ls.ExpiresAt.After(provisionDeadline) {
		t.Fatalf("start deadline %s should be later than the original provision deadline %s",
			ls.ExpiresAt, provisionDeadline)
	}
}
