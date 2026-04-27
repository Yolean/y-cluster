package qemu

import (
	"errors"
	"fmt"
	"os"
	"syscall"
)

// pidAlive reports whether `pid` refers to a process the current
// user can signal. signal(0) is the POSIX liveness probe — no
// signal is delivered, only the permission/existence checks fire.
//
// Replaces a `kill -0 <pid>` shell-out. Stdlib gives us typed
// errors (ESRCH = "no such process", EPERM = "exists but not
// owned by us") that the bash version collapsed into "exit 1".
// We treat ESRCH as not-alive; EPERM as alive (we just can't
// signal it — still a running pid the test cares about).
func pidAlive(pid int) bool {
	if pid <= 0 {
		return false
	}
	proc, err := os.FindProcess(pid)
	if err != nil {
		return false
	}
	err = proc.Signal(syscall.Signal(0))
	if err == nil {
		return true
	}
	if errors.Is(err, os.ErrProcessDone) {
		return false
	}
	if errors.Is(err, syscall.ESRCH) {
		return false
	}
	if errors.Is(err, syscall.EPERM) {
		// Process exists; we lack permission to signal it. For
		// our use (own VM PIDs) this shouldn't happen, but if it
		// does the right answer is "yes, still there".
		return true
	}
	return false
}

// pidTerminate sends SIGTERM to pid. ESRCH (already gone) is
// returned wrapped so the caller can errors.Is against it; the
// stop sequence in stopVM treats that as success.
func pidTerminate(pid int) error {
	proc, err := os.FindProcess(pid)
	if err != nil {
		return fmt.Errorf("find pid %d: %w", pid, err)
	}
	if err := proc.Signal(syscall.SIGTERM); err != nil {
		return fmt.Errorf("signal pid %d: %w", pid, err)
	}
	return nil
}

// pidKill sends SIGKILL to pid. Same error conventions as
// pidTerminate. Used by stopVM as the escalation when SIGTERM
// doesn't make qemu exit within the polite-wait window.
func pidKill(pid int) error {
	proc, err := os.FindProcess(pid)
	if err != nil {
		return fmt.Errorf("find pid %d: %w", pid, err)
	}
	if err := proc.Signal(syscall.SIGKILL); err != nil {
		return fmt.Errorf("kill pid %d: %w", pid, err)
	}
	return nil
}
