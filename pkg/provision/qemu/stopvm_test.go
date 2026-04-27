package qemu

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"
)

// withGraceTimeouts shrinks termGrace/killGrace for the duration of a
// test so we don't sit on the production 10s/5s budgets.
func withGraceTimeouts(t *testing.T, term, kill time.Duration) {
	t.Helper()
	prevTerm, prevKill := termGrace, killGrace
	termGrace, killGrace = term, kill
	t.Cleanup(func() {
		termGrace, killGrace = prevTerm, prevKill
	})
}

func writePidFile(t *testing.T, dir string, pid int) string {
	t.Helper()
	p := filepath.Join(dir, "vm.pid")
	if err := os.WriteFile(p, []byte(fmt.Sprintf("%d\n", pid)), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestStopVM_NoPidFile(t *testing.T) {
	if err := stopVM(filepath.Join(t.TempDir(), "missing.pid"), nil); err != nil {
		t.Fatalf("stopVM with no pidfile: %v", err)
	}
}

func TestStopVM_StalePID(t *testing.T) {
	dir := t.TempDir()
	// 999999999 reliably resolves to "no such process" on Linux.
	pidFile := writePidFile(t, dir, 999999999)
	if err := stopVM(pidFile, nil); err != nil {
		t.Fatalf("stopVM with stale pid: %v", err)
	}
	if _, err := os.Stat(pidFile); !os.IsNotExist(err) {
		t.Fatalf("pidfile should be removed for stale pid; stat err=%v", err)
	}
}

func TestStopVM_CorruptPidFile(t *testing.T) {
	dir := t.TempDir()
	pidFile := filepath.Join(dir, "vm.pid")
	if err := os.WriteFile(pidFile, []byte("not-a-number\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := stopVM(pidFile, nil); err != nil {
		t.Fatalf("stopVM with corrupt pidfile: %v", err)
	}
	if _, err := os.Stat(pidFile); !os.IsNotExist(err) {
		t.Fatalf("corrupt pidfile should be removed; stat err=%v", err)
	}
}

// startReapableChild spawns a child and starts a goroutine that
// Wait()s on it, so that once the kernel finishes killing the
// process we don't leave behind a zombie. In production qemu is
// `-daemonize`d and reparented to init, which reaps it; in tests
// we are the parent and must do it ourselves -- otherwise pidAlive
// keeps returning true for the zombie and stopVM "fails".
func startReapableChild(t *testing.T, name string, args ...string) *exec.Cmd {
	t.Helper()
	cmd := exec.Command(name, args...)
	if err := cmd.Start(); err != nil {
		t.Fatalf("start %s: %v", name, err)
	}
	go func() { _, _ = cmd.Process.Wait() }()
	t.Cleanup(func() { _ = cmd.Process.Kill() })
	return cmd
}

// TestStopVM_TerminatesOnSIGTERM spawns a real child that exits on
// SIGTERM, then asserts stopVM cleans it up without escalating.
func TestStopVM_TerminatesOnSIGTERM(t *testing.T) {
	withGraceTimeouts(t, 3*time.Second, 1*time.Second)

	cmd := startReapableChild(t, "sleep", "60")
	pidFile := writePidFile(t, t.TempDir(), cmd.Process.Pid)

	if err := stopVM(pidFile, nil); err != nil {
		t.Fatalf("stopVM: %v", err)
	}
	if pidAlive(cmd.Process.Pid) {
		t.Fatal("process should be dead after stopVM")
	}
	if _, err := os.Stat(pidFile); !os.IsNotExist(err) {
		t.Fatalf("pidfile should be removed; stat err=%v", err)
	}
}

// TestStopVM_EscalatesToSIGKILL covers the regression we're fixing:
// a process that ignores SIGTERM must still be killed (and the
// pidfile cleaned) by stopVM. The downstream agent saw qemu surviving
// teardown and blocking the next provision; this is the smaller test
// that asserts our SIGKILL escalation works.
func TestStopVM_EscalatesToSIGKILL(t *testing.T) {
	// termGrace must be small so the test is fast, but big enough
	// that the shell has time to install its trap before we signal.
	withGraceTimeouts(t, 1*time.Second, 5*time.Second)

	// bash trap '' TERM ignores SIGTERM until the shell exits; only
	// SIGKILL will reap it. `sleep infinity` keeps it alive without
	// burning CPU. exec sleep so the bash shell is replaced with the
	// process we want to outlive SIGTERM (otherwise SIGTERM would go
	// to bash, which forwards/handles signals differently).
	cmd := startReapableChild(t, "bash", "-c", "trap '' TERM; sleep infinity")

	// Give bash a moment to install the trap before we ask stopVM
	// to send SIGTERM. Without this the signal can race the trap
	// setup and the process exits "nicely", which would mask the
	// escalation path we want to exercise.
	time.Sleep(200 * time.Millisecond)

	pidFile := writePidFile(t, t.TempDir(), cmd.Process.Pid)

	start := time.Now()
	if err := stopVM(pidFile, nil); err != nil {
		t.Fatalf("stopVM: %v", err)
	}
	elapsed := time.Since(start)

	// SIGTERM was ignored, so we must have spent at least termGrace
	// before escalating. If we returned faster than that, the test
	// didn't actually exercise the escalation path.
	if elapsed < 1*time.Second {
		t.Fatalf("stopVM returned in %v; SIGKILL escalation path not exercised", elapsed)
	}
	if pidAlive(cmd.Process.Pid) {
		t.Fatal("process should be dead after SIGKILL escalation")
	}
	if _, err := os.Stat(pidFile); !os.IsNotExist(err) {
		t.Fatalf("pidfile should be removed; stat err=%v", err)
	}
	_, _ = cmd.Process.Wait()
}
