package qemu

import (
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"go.uber.org/zap"
)

// processState is the state letter from /proc/<pid>/stat; T is
// "stopped by a signal".
func processState(t *testing.T, pid int) string {
	t.Helper()
	stat, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
	if err != nil {
		t.Fatalf("read stat of pid %d: %v", pid, err)
	}
	// "<pid> (<comm>) <state> ...", and comm may contain spaces.
	rest := string(stat)[strings.LastIndexByte(string(stat), ')')+1:]
	return strings.Fields(rest)[0]
}

// waitForState polls because a signal is delivered asynchronously.
func waitForState(t *testing.T, pid int, want func(state string) bool, what string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !want(processState(t, pid)) {
		if time.Now().After(deadline) {
			t.Fatalf("pid %d is in state %q, want %s", pid, processState(t, pid), what)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestPauseResume(t *testing.T) {
	dir := t.TempDir()
	cmd := startVMStandin(t, pidFilePath(dir, "vm"), false)
	pid := cmd.Process.Pid

	if err := Pause(dir, "vm", nil); err != nil {
		t.Fatalf("Pause: %v", err)
	}
	waitForState(t, pid, func(s string) bool { return s == "T" }, "T (stopped)")

	if err := Resume(dir, "vm", nil); err != nil {
		t.Fatalf("Resume: %v", err)
	}
	waitForState(t, pid, func(s string) bool { return s != "T" }, "anything but T")

	// Resume of a VM that is not paused changes nothing.
	if err := Resume(dir, "vm", nil); err != nil {
		t.Fatalf("second Resume: %v", err)
	}
	if !pidAlive(pid) {
		t.Fatal("Resume of a running VM ended it")
	}
}

func TestPauseResume_NoVM(t *testing.T) {
	dir := t.TempDir()
	if err := Pause(dir, "vm", nil); err == nil {
		t.Error("Pause with no pidfile succeeded")
	}
	if err := Resume(dir, "vm", nil); err == nil {
		t.Error("Resume with no pidfile succeeded")
	}
}

// A stopped process leaves SIGTERM pending until it is continued. If
// shutdown did not wake a paused VM first it would sit out termGrace
// and then SIGKILL it: a hard power-off of a guest that was only ever
// asked to wait.
func TestShutdownVM_WakesAPausedVM(t *testing.T) {
	withGraceTimeouts(t, 20*time.Second, 5*time.Second)

	dir := t.TempDir()
	cmd := startVMStandin(t, pidFilePath(dir, "vm"), false)
	pid := cmd.Process.Pid
	if err := Pause(dir, "vm", nil); err != nil {
		t.Fatalf("Pause: %v", err)
	}
	waitForState(t, pid, func(s string) bool { return s == "T" }, "T (stopped)")

	// There is no state sidecar, so the guest poweroff step fails at
	// once and the signal ladder is what ends the process.
	start := time.Now()
	if err := shutdownVM(dir, "vm", zap.NewNop()); err != nil {
		t.Fatalf("shutdownVM: %v", err)
	}
	if elapsed := time.Since(start); elapsed > termGrace/2 {
		t.Errorf("shutdown took %s: the paused process did not get to handle SIGTERM and had to be killed", elapsed.Round(time.Millisecond))
	}
	if pidAlive(pid) {
		t.Error("process survived shutdown")
	}
	if _, err := os.Stat(pidFilePath(dir, "vm")); !os.IsNotExist(err) {
		t.Errorf("pidfile should be removed; stat err=%v", err)
	}
}

// pausedVMWithLifetime is a stand-in VM with a state sidecar whose
// deadline is `untilDeadline` away, paused.
func pausedVMWithLifetime(t *testing.T, now time.Time, untilDeadline time.Duration) (dir string) {
	t.Helper()
	dir = t.TempDir()
	pinClock(t, now.Add(untilDeadline-8*time.Hour))
	if err := saveState(Config{Name: "vm", CacheDir: dir, Lifetime: "8h", OnExpiry: "pause"}); err != nil {
		t.Fatal(err)
	}
	if _, err := armLifetime(dir, "vm"); err != nil {
		t.Fatal(err)
	}
	pinClock(t, now)
	startVMStandin(t, pidFilePath(dir, "vm"), false)
	if err := Pause(dir, "vm", nil); err != nil {
		t.Fatalf("Pause: %v", err)
	}
	return dir
}

// lifetime.onExpiry: pause is only cost control if it keeps applying
// after the operator comes back: resuming an expired VM starts a new
// budget, the way Start does for a VM that expiry stopped.
func TestResume_ExpiredLifetimeGetsAFreshBudget(t *testing.T) {
	now := time.Date(2026, 9, 21, 12, 0, 0, 0, time.UTC)
	dir := pausedVMWithLifetime(t, now, -time.Hour)
	if ls, _ := loadLifetime(dir, "vm"); !ls.Expired() {
		t.Fatalf("setup: deadline %s should have passed", ls.ExpiresAt)
	}

	if err := Resume(dir, "vm", nil); err != nil {
		t.Fatalf("Resume: %v", err)
	}
	ls, err := loadLifetime(dir, "vm")
	if err != nil {
		t.Fatal(err)
	}
	if want := now.Add(8 * time.Hour); !ls.ExpiresAt.Equal(want) {
		t.Errorf("deadline after resume = %s, want %s", ls.ExpiresAt, want)
	}
}

// Pausing by hand does not buy time: the deadline that was ahead
// before the pause is the deadline after it.
func TestResume_DeadlineStillAheadIsKept(t *testing.T) {
	now := time.Date(2026, 9, 21, 12, 0, 0, 0, time.UTC)
	dir := pausedVMWithLifetime(t, now, 3*time.Hour)

	if err := Resume(dir, "vm", nil); err != nil {
		t.Fatalf("Resume: %v", err)
	}
	ls, err := loadLifetime(dir, "vm")
	if err != nil {
		t.Fatal(err)
	}
	if want := now.Add(3 * time.Hour); !ls.ExpiresAt.Equal(want) {
		t.Errorf("deadline after resume = %s, want the unchanged %s", ls.ExpiresAt, want)
	}
}
