//go:build unix

package serve

import (
	"fmt"
	"os"
	"os/exec"
	"syscall"
	"time"
)

// spawnBackground re-execs the current binary in daemon mode, detaches
// it (Setsid), redirects stdout/stderr to paths.Log, closes stdin, and
// returns the child pid. The caller is responsible for not waiting on
// the child — we detach via cmd.Process.Release().
func spawnBackground(execPath string, args []string, paths StatePaths) (int, error) {
	logf, err := os.OpenFile(paths.Log, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return 0, fmt.Errorf("open log: %w", err)
	}
	defer logf.Close()

	devnull, err := os.OpenFile(os.DevNull, os.O_RDONLY, 0)
	if err != nil {
		return 0, fmt.Errorf("open /dev/null: %w", err)
	}
	defer devnull.Close()

	cmd := exec.Command(execPath, args...)
	cmd.Env = append(os.Environ(), daemonEnv+"=1")
	cmd.Stdin = devnull
	cmd.Stdout = logf
	cmd.Stderr = logf
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}

	if err := cmd.Start(); err != nil {
		return 0, fmt.Errorf("spawn: %w", err)
	}
	pid := cmd.Process.Pid
	if err := cmd.Process.Release(); err != nil {
		return pid, fmt.Errorf("release: %w", err)
	}
	// Brief pause so the child has a chance to write its pidfile; the
	// caller then polls /health, so this is just to avoid a tight loop.
	time.Sleep(50 * time.Millisecond)
	return pid, nil
}
