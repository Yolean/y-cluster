package qemu

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"
)

// pidAlive reports whether `pid` refers to a process the current
// user can signal. signal(0) is the POSIX liveness probe -- no
// signal is delivered, only the permission/existence checks fire.
//
// The errors are typed: ESRCH = "no such process", EPERM =
// "exists but not owned by us". ESRCH is not-alive; EPERM is
// alive (we just can't signal it).
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

// vmAlive reports whether pid is alive AND is the qemu process that
// was started with pidFile. Liveness alone is not enough to act on: a
// pidfile outlives a reboot or a crashed qemu, the kernel hands the
// number to something else, and stop/teardown would SIGTERM and then
// SIGKILL a process that has nothing to do with y-cluster.
//
// The identity is qemu's own `-pidfile <path>` argument, read from
// /proc/<pid>/cmdline. Where /proc does not exist the question cannot
// be answered and liveness decides, as it did before; the qemu
// provider needs KVM, so in practice that is never.
func vmAlive(pid int, pidFile string) bool {
	if !pidAlive(pid) {
		return false
	}
	cmdline, err := os.ReadFile(fmt.Sprintf("/proc/%d/cmdline", pid))
	if err != nil {
		_, statErr := os.Stat("/proc/self/cmdline")
		// No /proc at all: unknown. /proc present but this pid
		// unreadable: it went away between the two checks.
		return statErr != nil
	}
	return cmdlineNamesPidfile(cmdline, pidFile)
}

// cmdlineNamesPidfile reports whether a NUL-separated argv contains
// `-pidfile <path>` for our pidfile. Paths are compared by base name
// when they are not identical: the cache dir may have been spelled
// differently (relative, symlinked) when the VM was started, and the
// base name carries the cluster name.
func cmdlineNamesPidfile(cmdline []byte, pidFile string) bool {
	args := strings.Split(string(cmdline), "\x00")
	for i, a := range args {
		if a != "-pidfile" || i+1 >= len(args) {
			continue
		}
		if args[i+1] == pidFile || filepath.Base(args[i+1]) == filepath.Base(pidFile) {
			return true
		}
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
