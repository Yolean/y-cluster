//go:build e2e && kvm

package e2e

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

// TestQemu_LifetimeTimerReaps runs the whole local expiry path as an
// operator gets it: `y-cluster provision` with a lifetime arms a host
// timer, the timer fires `y-cluster lifetime reap` at the deadline,
// and reap stops the VM while keeping its disk.
//
// TestQemu_Lifetime covers the deadline bookkeeping by calling
// qemu.Stop itself. What only a real timer shows is whether the reap
// it starts can find the cluster: a transient systemd unit does not
// inherit the environment of the process that armed it, so reap
// needs KUBECONFIG and the cache dir handed over. Both are set to
// non-default places here, as they are under the ystack convention.
func TestQemu_LifetimeTimerReaps(t *testing.T) {
	if _, err := os.Stat("/dev/kvm"); err != nil {
		t.Skip("QEMU tests require /dev/kvm")
	}
	if out, err := exec.Command("systemd-run", "--user", "--quiet", "--wait", "true").CombinedOutput(); err != nil {
		t.Skipf("no usable systemd user manager for the host timer: %v: %s", err, out)
	}
	if os.Getenv("KUBECONFIG") == "" {
		t.Skip("KUBECONFIG must be set")
	}
	bin := buildBinary(t)

	const name = "y-cluster-e2e-reap"
	const maxRun = 4 * time.Minute
	cacheDir := e2eQEMUCacheDir(t)
	t.Setenv("Y_CLUSTER_QEMU_CACHE_DIR", cacheDir)
	t.Setenv("Y_CLUSTER_INVENTORY_DIR", t.TempDir())
	cfgDir := t.TempDir()
	cfg := fmt.Sprintf(`provider: qemu
name: %[1]s
context: %[1]s
cacheDir: %[2]s
sshPort: "2234"
memory: "2048"
cpus: "2"
diskSize: 40G
portForwards:
  - {host: "26490", guest: "6443"}
gateway:
  skip: true
lifetime:
  maxRun: %[3]s
  onExpiry: stop
`, name, cacheDir, maxRun)
	if err := os.WriteFile(filepath.Join(cfgDir, "y-cluster-provision.yaml"), []byte(cfg), 0o644); err != nil {
		t.Fatal(err)
	}

	run := func(timeout time.Duration, args ...string) (string, error) {
		ctx, cancel := context.WithTimeout(context.Background(), timeout)
		defer cancel()
		out, err := exec.CommandContext(ctx, bin, args...).CombinedOutput()
		return string(out), err
	}
	t.Cleanup(func() {
		if out, err := run(3*time.Minute, "teardown", "-c", cfgDir); err != nil {
			t.Logf("teardown: %v\n%s", err, out)
		}
	})

	started := time.Now()
	if out, err := run(15*time.Minute, "provision", "-c", cfgDir); err != nil {
		t.Fatalf("provision: %v\n%s", err, out)
	}

	unit := "y-cluster-lifetime-" + name + ".timer"
	if out, err := exec.Command("systemctl", "--user", "is-active", unit).CombinedOutput(); err != nil {
		t.Fatalf("provision did not leave an active host timer %s: %v: %s", unit, err, out)
	}
	pidBytes, err := os.ReadFile(filepath.Join(cacheDir, name+".pid"))
	if err != nil {
		t.Fatalf("read pidfile: %v", err)
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(pidBytes)))
	if err != nil {
		t.Fatal(err)
	}
	alive := func() bool { return syscall.Kill(pid, 0) == nil }
	if !alive() {
		t.Fatal("VM is not running right after provision")
	}

	// Nothing here stops the VM; only the timer can.
	deadline := started.Add(maxRun + 3*time.Minute)
	for alive() {
		if time.Now().After(deadline) {
			status, _ := exec.Command("systemctl", "--user", "status", strings.TrimSuffix(unit, ".timer")+".service", "--no-pager", "-n", "30").CombinedOutput()
			t.Fatalf("VM still running %s after provision started, with maxRun %s; the timer's reap did not stop it.\n%s",
				time.Since(started).Round(time.Second), maxRun, status)
		}
		time.Sleep(5 * time.Second)
	}
	if ran := time.Since(started); ran < maxRun-30*time.Second {
		t.Errorf("VM stopped after %s, well before its %s budget", ran.Round(time.Second), maxRun)
	}
	// onExpiry: stop keeps the cluster resumable.
	if _, err := os.Stat(filepath.Join(cacheDir, name+".qcow2")); err != nil {
		t.Errorf("disk should survive a stop on expiry: %v", err)
	}
}
