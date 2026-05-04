package qemu

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"go.uber.org/zap"

	"github.com/Yolean/y-cluster/pkg/kubeconfig"
	"github.com/Yolean/y-cluster/pkg/sshexec"
)

// Pause freezes the running qemu process via SIGSTOP. The VM's
// guest is paused mid-clock; no in-flight syscalls complete until
// Resume sends SIGCONT. Use for "I want CPU back for a minute"
// rather than "I'm done"; Stop is the latter.
//
// Pidfile-driven so callers don't need a *Cluster handle from the
// original Provision call -- the y-cluster CLI's `pause` subcommand
// finds the cluster via cluster.Lookup and hands its cacheDir/name
// to this function.
func Pause(cacheDir, name string, logger *zap.Logger) error {
	if logger == nil {
		logger = zap.NewNop()
	}
	pid, err := readPidFile(pidFilePath(cacheDir, name))
	if err != nil {
		return err
	}
	logger.Info("pausing qemu VM", zap.String("name", name), zap.Int("pid", pid))
	return pidSignal(pid, syscall.SIGSTOP)
}

// Resume sends SIGCONT to a paused qemu process. No-op when the
// process isn't paused.
func Resume(cacheDir, name string, logger *zap.Logger) error {
	if logger == nil {
		logger = zap.NewNop()
	}
	pid, err := readPidFile(pidFilePath(cacheDir, name))
	if err != nil {
		return err
	}
	logger.Info("resuming qemu VM", zap.String("name", name), zap.Int("pid", pid))
	return pidSignal(pid, syscall.SIGCONT)
}

// Stop gracefully shuts down a running qemu VM. Order:
//
//  1. Try to issue `sudo sync; sudo poweroff` over SSH so the
//     guest's systemd-shutdown sequence flushes k3s/containerd
//     state cleanly. This was the root cause of "exec format
//     error" crash loops on the imported side of the appliance
//     round-trip: qemu's SIGTERM exit (~200ms) drops the guest
//     pagecache mid-write and containerd's overlayfs snapshot
//     files end up zero-byte.
//  2. Wait up to gracefulShutdownGrace for qemu to exit on its
//     own (the guest's poweroff propagates back through qemu).
//  3. Fall back to stopVM's existing SIGTERM -> SIGKILL ladder
//     for the cases where SSH is unreachable (sshd not up yet,
//     network broken, key changed) or the guest hangs.
//
// Disk and state sidecar are preserved so Start can resume.
// The kubeconfig context is left intact -- consumers who want
// to "permanently" stop should use Teardown.
func Stop(cacheDir, name string, logger *zap.Logger) error {
	if logger == nil {
		logger = zap.NewNop()
	}
	logger.Info("stopping qemu VM", zap.String("name", name))

	pidFile := pidFilePath(cacheDir, name)
	pid, err := readPidFile(pidFile)
	if err != nil {
		// No live cluster -- nothing to do. stopVM is idempotent
		// and handles a missing/stale pidfile by returning nil.
		return stopVM(pidFile, logger)
	}

	// Best-effort graceful guest shutdown via ssh. Failures here
	// are logged but not fatal; we always fall through to the
	// signal ladder below.
	if err := guestPoweroff(cacheDir, name, pid, logger); err != nil {
		logger.Warn("graceful guest shutdown failed; falling back to qemu signals",
			zap.Error(err))
	} else if !pidAlive(pid) {
		_ = os.Remove(pidFile)
		return nil
	}

	return stopVM(pidFile, logger)
}

// gracefulShutdownGrace caps how long Stop waits for the guest's
// poweroff to propagate to qemu exit. k3s + containerd flush
// state during the systemd shutdown sequence; 60s is generous on
// a healthy cluster, well below the 10s + 5s SIGTERM/SIGKILL
// fallback that an unhealthy cluster would hit.
var gracefulShutdownGrace = 60 * time.Second

// guestPoweroff runs `sudo sync; sudo poweroff` over the qemu's
// host-port-forwarded ssh and then polls until the qemu pid
// exits. Errors are typed:
//   - load/state errors: caller logs and falls back to signals.
//   - ssh dial failures: same.
//   - waitForExit timeout: caller logs and falls back to signals.
//
// The ssh command itself returns in seconds (poweroff signals
// systemd-shutdown and returns); the longer wait is for the
// guest to actually finish unmounting filesystems and qemu to
// notice the guest powered off.
func guestPoweroff(cacheDir, name string, pid int, logger *zap.Logger) error {
	cfg, err := loadState(cacheDir, name)
	if err != nil {
		return fmt.Errorf("load state: %w", err)
	}
	target := sshexec.Target{
		Host:    "127.0.0.1",
		Port:    cfg.SSHPort,
		User:    "ystack",
		KeyPath: filepath.Join(cfg.CacheDir, cfg.Name+"-ssh"),
	}
	// sync first so any pending writes hit disk before systemd
	// kills off the writers. poweroff is async; the command
	// returns immediately and shutdown propagates.
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	logger.Info("requesting graceful guest shutdown via ssh poweroff",
		zap.String("name", name), zap.Int("pid", pid))
	if _, err := sshexec.Exec(ctx, target, "sudo sync; sudo poweroff", nil); err != nil {
		// Some sshd configs drop the connection during the
		// poweroff exec; treat that as expected-not-fatal here
		// and let the wait loop decide based on pid liveness.
		logger.Debug("ssh poweroff returned an error (may be expected on disconnect)",
			zap.Error(err))
	}
	if !waitForExit(pid, gracefulShutdownGrace) {
		return fmt.Errorf("qemu pid %d still alive after %s of graceful shutdown",
			pid, gracefulShutdownGrace)
	}
	logger.Info("qemu exited cleanly via guest poweroff", zap.Int("pid", pid))
	return nil
}

// Start re-launches a previously-stopped qemu VM. Reads the state
// sidecar Provision wrote, invokes startVM against the existing
// qcow2 (no cloud-init re-run -- the disk already has the user
// and SSH key from first boot), waits for k3s to come back up,
// then re-imports the kubeconfig so the host-side context is
// fresh even if it was cleaned while the cluster was down.
func Start(ctx context.Context, cacheDir, name string, logger *zap.Logger) (*Cluster, error) {
	if logger == nil {
		logger = zap.NewNop()
	}
	cfg, err := loadState(cacheDir, name)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, fmt.Errorf("no saved state for %q in %s; run `y-cluster provision` first", name, cacheDir)
		}
		return nil, fmt.Errorf("load state: %w", err)
	}

	if running, pid := cfg.IsRunning(); running {
		return nil, fmt.Errorf("VM %q already running (pid %d)", name, pid)
	}

	diskPath := filepath.Join(cfg.CacheDir, cfg.Name+".qcow2")
	if _, err := os.Stat(diskPath); err != nil {
		return nil, fmt.Errorf("disk %s not found; re-provision", diskPath)
	}

	kubecfg, err := kubeconfig.New(cfg.Context, clusterName(cfg.Name), logger)
	if err != nil {
		return nil, err
	}

	c := &Cluster{
		cfg:        cfg,
		sshKey:     filepath.Join(cfg.CacheDir, cfg.Name+"-ssh"),
		pidFile:    pidFilePath(cfg.CacheDir, cfg.Name),
		logger:     logger,
		Kubeconfig: kubecfg,
	}

	if err := c.startVM(ctx, diskPath, ""); err != nil {
		return nil, fmt.Errorf("start VM: %w", err)
	}
	if err := c.waitForSSH(ctx); err != nil {
		return nil, fmt.Errorf("wait for SSH: %w", err)
	}
	logger.Info("VM up; waiting for k3s")
	if err := c.waitForK3sReady(ctx); err != nil {
		return nil, fmt.Errorf("wait for k3s: %w", err)
	}

	rawKubeconfig, err := c.extractKubeconfig(ctx)
	if err != nil {
		return nil, fmt.Errorf("extract kubeconfig: %w", err)
	}
	if err := kubecfg.Import(rawKubeconfig); err != nil {
		return nil, fmt.Errorf("merge kubeconfig: %w", err)
	}
	logger.Info("k3s ready", zap.String("context", cfg.Context))
	return c, nil
}

// pidFilePath is the canonical pidfile path used by Provision /
// Teardown / lifecycle. Mirrors the same join pattern; centralised
// so a layout change touches one spot.
func pidFilePath(cacheDir, name string) string {
	return filepath.Join(cacheDir, name+".pid")
}

// readPidFile parses the qemu pid out of pidFile and verifies the
// process is alive. Returns os.ErrNotExist when the file isn't
// there so callers can branch on errors.Is.
func readPidFile(pidFile string) (int, error) {
	data, err := os.ReadFile(pidFile)
	if err != nil {
		return 0, err
	}
	var pid int
	if _, err := fmt.Sscanf(strings.TrimSpace(string(data)), "%d", &pid); err != nil {
		return 0, fmt.Errorf("parse %s: %w", pidFile, err)
	}
	if !pidAlive(pid) {
		return 0, fmt.Errorf("%s: pid %d not alive (cluster stopped?)", pidFile, pid)
	}
	return pid, nil
}

// pidSignal sends sig to pid. Wraps the *os.Process boilerplate
// for the SIGSTOP/SIGCONT cases Pause/Resume need.
func pidSignal(pid int, sig syscall.Signal) error {
	proc, err := os.FindProcess(pid)
	if err != nil {
		return fmt.Errorf("find pid %d: %w", pid, err)
	}
	if err := proc.Signal(sig); err != nil {
		return fmt.Errorf("signal pid %d: %w", pid, err)
	}
	return nil
}
