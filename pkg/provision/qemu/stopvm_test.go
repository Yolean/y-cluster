package qemu

import (
	"bufio"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
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

// TestHelperVM is not a test. Re-executed by startVMStandin, the test
// binary plays the part of a running qemu: it stays alive, and
// optionally ignores SIGTERM like a wedged VM does.
func TestHelperVM(t *testing.T) {
	mode := os.Getenv("Y_CLUSTER_TEST_VM_STANDIN")
	if mode == "" {
		return
	}
	if mode == "ignore-term" {
		signal.Ignore(syscall.SIGTERM)
	}
	fmt.Println("ready")
	time.Sleep(60 * time.Second)
	os.Exit(0)
}

// startVMStandin starts a process that stop/teardown will accept as
// the qemu owning pidFile, and writes its pid there. What makes it
// acceptable is what makes a real qemu acceptable: `-pidfile
// <pidFile>` on its command line.
func startVMStandin(t *testing.T, pidFile string, ignoreTerm bool) *exec.Cmd {
	t.Helper()
	mode := "exit-on-term"
	if ignoreTerm {
		mode = "ignore-term"
	}
	cmd := exec.Command(os.Args[0], "-test.run=^TestHelperVM$", "--", "-pidfile", pidFile)
	cmd.Env = append(os.Environ(), "Y_CLUSTER_TEST_VM_STANDIN="+mode)
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatalf("start VM stand-in: %v", err)
	}
	// "ready" is printed after the signal disposition is in place.
	if line, err := bufio.NewReader(stdout).ReadString('\n'); err != nil || strings.TrimSpace(line) != "ready" {
		t.Fatalf("VM stand-in did not come up: %q %v", line, err)
	}
	// Reaped here for the reason startReapableChild explains.
	go func() { _, _ = cmd.Process.Wait() }()
	t.Cleanup(func() { _ = cmd.Process.Kill() })
	if err := os.WriteFile(pidFile, []byte(fmt.Sprintf("%d\n", cmd.Process.Pid)), 0o644); err != nil {
		t.Fatal(err)
	}
	return cmd
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

	pidFile := filepath.Join(t.TempDir(), "vm.pid")
	cmd := startVMStandin(t, pidFile, false)

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

// TestStop_FallsBackToSignalsWhenSSHUnreachable covers the
// graceful-shutdown path's failure mode: when sshd isn't
// reachable (no real qemu, port closed) Stop must still kill
// the recorded pid via the SIGTERM/SIGKILL ladder. We simulate
// this with a sleep process whose pidfile is what Stop reads,
// and an unused SSH port so sshexec.Exec dial fails fast.
func TestStop_FallsBackToSignalsWhenSSHUnreachable(t *testing.T) {
	withGraceTimeouts(t, 2*time.Second, 1*time.Second)

	// Shrink the graceful budget so the test doesn't sit on the
	// production 60s.
	prevGrace := gracefulShutdownGrace
	gracefulShutdownGrace = 500 * time.Millisecond
	t.Cleanup(func() { gracefulShutdownGrace = prevGrace })

	cacheDir := t.TempDir()
	cfg := defaultedRuntimeConfig(t)
	cfg.CacheDir = cacheDir
	cfg.SSHPort = "1" // privileged port, nothing listens; ssh dial fails fast
	if err := saveState(cfg); err != nil {
		t.Fatal(err)
	}

	pidFile := pidFilePath(cacheDir, cfg.Name)
	cmd := startVMStandin(t, pidFile, false)
	// Leave a placeholder ssh key file so guestPoweroff doesn't
	// fail at the file-read step (the dial is the failure we
	// want to exercise).
	if err := os.WriteFile(filepath.Join(cacheDir, cfg.Name+"-ssh"), []byte("not-a-key"), 0o600); err != nil {
		t.Fatal(err)
	}

	if err := Stop(cacheDir, cfg.Name, nil); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	if pidAlive(cmd.Process.Pid) {
		t.Fatal("process should be dead after Stop fallback")
	}
	if _, err := os.Stat(pidFile); !os.IsNotExist(err) {
		t.Fatalf("pidfile should be removed; stat err=%v", err)
	}
}

// TestStopVM_EscalatesToSIGKILL: a process that ignores SIGTERM must
// still be killed (and the pidfile cleaned) by stopVM. A qemu that
// survives teardown keeps its port forwards and blocks the next
// provision.
func TestStopVM_EscalatesToSIGKILL(t *testing.T) {
	// termGrace small so the test is fast; the stand-in has its
	// signal disposition in place before startVMStandin returns.
	withGraceTimeouts(t, 1*time.Second, 5*time.Second)

	pidFile := filepath.Join(t.TempDir(), "vm.pid")
	cmd := startVMStandin(t, pidFile, true)

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

// The pid in a pidfile can belong to anything by the time it is read:
// the file survives a reboot or a crashed qemu, and the kernel reuses
// the number. Such a process must not be signalled.
func TestStopVM_LeavesAnUnrelatedProcessAlone(t *testing.T) {
	withGraceTimeouts(t, 1*time.Second, 1*time.Second)

	cmd := startReapableChild(t, "sleep", "60")
	pidFile := writePidFile(t, t.TempDir(), cmd.Process.Pid)

	if err := stopVM(pidFile, nil); err != nil {
		t.Fatalf("stopVM: %v", err)
	}
	if !pidAlive(cmd.Process.Pid) {
		t.Fatal("stopVM killed a process that is not the VM")
	}
	if _, err := os.Stat(pidFile); !os.IsNotExist(err) {
		t.Fatalf("the stale pidfile should be removed; stat err=%v", err)
	}
}

func TestIsRunning_PidReusedByAnotherProcess(t *testing.T) {
	cfg := defaultedRuntimeConfig(t)
	cfg.CacheDir = t.TempDir()
	// This test process: certainly alive, certainly not the VM.
	if err := os.WriteFile(pidFilePath(cfg.CacheDir, cfg.Name), []byte(fmt.Sprintf("%d\n", os.Getpid())), 0o644); err != nil {
		t.Fatal(err)
	}
	if running, pid := cfg.IsRunning(); running {
		t.Fatalf("pid %d is alive but is not this cluster's qemu", pid)
	}
}

func TestCmdlineNamesPidfile(t *testing.T) {
	argv := func(args ...string) []byte { return []byte(strings.Join(args, "\x00") + "\x00") }
	const pidFile = "/home/u/.cache/y-cluster-qemu/local.pid"
	for _, tc := range []struct {
		name    string
		cmdline []byte
		want    bool
	}{
		{"qemu started with this pidfile", argv("qemu-system-x86_64", "-name", "local", "-daemonize", "-pidfile", pidFile), true},
		{"same cluster, cache dir spelled differently", argv("qemu-system-x86_64", "-pidfile", "../.cache/y-cluster-qemu/local.pid"), true},
		{"qemu of another cluster", argv("qemu-system-x86_64", "-name", "other", "-pidfile", "/home/u/.cache/y-cluster-qemu/other.pid"), false},
		{"unrelated process", argv("sleep", "60"), false},
		{"path mentioned but not as the -pidfile argument", argv("tail", "-f", pidFile), false},
		{"-pidfile as the last argument", argv("qemu-system-x86_64", "-pidfile"), false},
		{"empty", nil, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := cmdlineNamesPidfile(tc.cmdline, pidFile); got != tc.want {
				t.Fatalf("got %v, want %v", got, tc.want)
			}
		})
	}
}
