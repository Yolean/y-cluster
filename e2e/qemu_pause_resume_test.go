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

// procState is the process state letter from /proc/<pid>/stat: R, S,
// T (stopped by a signal), ...
func procState(t *testing.T, pid string) string {
	t.Helper()
	stat, err := os.ReadFile("/proc/" + pid + "/stat")
	if err != nil {
		t.Fatalf("read /proc/%s/stat: %v", pid, err)
	}
	// "<pid> (<comm>) <state> ...": comm may contain spaces, so
	// split after the closing parenthesis.
	rest := string(stat[strings.LastIndexByte(string(stat), ')')+1:])
	return strings.Fields(rest)[0]
}

// waitForProcState polls because signals are delivered
// asynchronously.
func waitForProcState(t *testing.T, pid string, stopped bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for (procState(t, pid) == "T") != stopped {
		if time.Now().After(deadline) {
			t.Fatalf("qemu process is in state %q; want stopped=%v", procState(t, pid), stopped)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func hostReadyz(kubeContext string) (string, error) {
	out, err := exec.Command("kubectl", "--context="+kubeContext, "--request-timeout=3s", "get", "--raw=/readyz").CombinedOutput()
	return strings.TrimSpace(string(out)), err
}

// TestQemu_PauseResume covers the pause and resume verbs, and what an
// operator does next with a paused VM. `lifetime.onExpiry: pause`
// leaves a VM paused until someone comes back to it, so a paused VM
// is a state the other verbs meet in normal use:
//
//   - pause freezes the VM: nothing answers, nothing is lost
//   - resume brings the same cluster back
//   - stop on a paused VM is still a graceful guest shutdown. A
//     frozen qemu neither serves ssh nor handles SIGTERM, so without
//     waking it first stop degrades to SIGKILL, the unclean exit
//     Stop's guest poweroff exists to avoid.
func TestQemu_PauseResume(t *testing.T) {
	if _, err := os.Stat("/dev/kvm"); err != nil {
		t.Skip("QEMU tests require /dev/kvm")
	}
	if err := qemu.CheckPrerequisites(); err != nil {
		t.Skip(err)
	}

	logger, _ := zap.NewDevelopment()
	cfg := e2eQEMURuntime()
	cfg.Name = "y-cluster-e2e-pause"
	cfg.Context = "y-cluster-e2e-pause"
	cfg.CacheDir = t.TempDir()
	cfg.Memory = "2048"
	cfg.CPUs = "2"
	cfg.SSHPort = "2237"
	cfg.PortForwards = e2eUniqueForwards("26467", "28467")
	cfg.Kubeconfig = os.Getenv("KUBECONFIG")
	if cfg.Kubeconfig == "" {
		t.Skip("KUBECONFIG must be set")
	}
	// The gateway is not what is under test.
	cfg.Gateway.Skip = true

	ctx := context.Background()
	if _, err := qemu.Provision(ctx, cfg, logger); err != nil {
		t.Fatalf("Provision: %v", err)
	}
	t.Cleanup(func() { _ = qemu.TeardownConfig(cfg, false, logger) })

	pidBytes, err := os.ReadFile(filepath.Join(cfg.CacheDir, cfg.Name+".pid"))
	if err != nil {
		t.Fatalf("read pidfile: %v", err)
	}
	pid := strings.TrimSpace(string(pidBytes))

	// Something to recognise the cluster by after resume.
	if out, err := exec.Command("kubectl", "--context="+cfg.Context, "create", "configmap", "survives-pause", "--from-literal=k=v").CombinedOutput(); err != nil {
		t.Fatalf("create configmap: %s: %v", out, err)
	}

	if err := qemu.Pause(cfg.CacheDir, cfg.Name, logger); err != nil {
		t.Fatalf("Pause: %v", err)
	}
	waitForProcState(t, pid, true)
	if out, err := hostReadyz(cfg.Context); err == nil {
		t.Fatalf("a paused VM answered /readyz: %s", out)
	}

	if err := qemu.Resume(cfg.CacheDir, cfg.Name, logger); err != nil {
		t.Fatalf("Resume: %v", err)
	}
	waitForProcState(t, pid, false)
	deadline := time.Now().Add(time.Minute)
	for {
		out, err := hostReadyz(cfg.Context)
		if err == nil && out == "ok" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("apiserver did not answer within 1m of resume: %s: %v", out, err)
		}
		time.Sleep(2 * time.Second)
	}
	if out, err := exec.Command("kubectl", "--context="+cfg.Context, "get", "configmap", "survives-pause").CombinedOutput(); err != nil {
		t.Fatalf("cluster state lost over pause/resume: %s: %v", out, err)
	}

	// Stop while paused.
	if err := qemu.Pause(cfg.CacheDir, cfg.Name, logger); err != nil {
		t.Fatalf("Pause before stop: %v", err)
	}
	waitForProcState(t, pid, true)
	if err := qemu.Stop(cfg.CacheDir, cfg.Name, logger); err != nil {
		t.Fatalf("Stop of a paused VM: %v", err)
	}
	if _, err := os.Stat("/proc/" + pid); err == nil {
		t.Fatalf("qemu pid %s still exists after Stop", pid)
	}

	// The guest's own record of how that boot ended. A SIGKILLed
	// qemu leaves a journal that just breaks off.
	cluster, err := qemu.Start(ctx, cfg.CacheDir, cfg.Name, logger)
	if err != nil {
		t.Fatalf("Start after stop: %v", err)
	}
	out, err := cluster.NodeExec(ctx, "sudo journalctl -b -1 --no-pager -o cat | grep 'poweroff.target' | wc -l", nil)
	if err != nil {
		t.Fatalf("read previous boot's journal: %s: %v", out, err)
	}
	if strings.TrimSpace(string(out)) == "0" {
		tail, _ := cluster.NodeExec(ctx, "sudo journalctl -b -1 --no-pager -o cat -n 15", nil)
		t.Errorf("the boot that was stopped while paused did not reach poweroff.target; it was killed, not shut down. Its journal ends:\n%s", tail)
	}
}
