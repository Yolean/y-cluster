package serve

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"time"

	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
)

// Options is the public input to the serve entry points.
type Options struct {
	// ConfigDirs are the `-c` arguments, each a directory containing
	// y-cluster-serve.yaml.
	ConfigDirs []string

	// Foreground makes Run block in the current process. When false,
	// Run re-execs itself detached and returns once /health is ready.
	Foreground bool

	// StateDir overrides the per-user state directory. Empty uses
	// DefaultStateDir().
	StateDir string

	// ExecPath is the binary path used for background re-exec. Empty
	// uses os.Executable(). Set by tests to pin a built binary.
	ExecPath string

	// HealthTimeout caps how long Ensure/Run wait for ports to become
	// healthy after start. Zero uses 10s.
	HealthTimeout time.Duration
}

// Run is the main entry point. If already in daemon mode (re-execed),
// it runs the server loop; otherwise it validates config and either
// runs in foreground or spawns a background child.
func Run(ctx context.Context, opts Options) error {
	if err := refuseRoot(); err != nil {
		return err
	}

	cfgs, err := LoadConfigDirs(opts.ConfigDirs)
	if err != nil {
		return err
	}
	paths, err := ResolveStatePaths(opts.StateDir)
	if err != nil {
		return err
	}

	if daemonMode() {
		return runAsDaemon(ctx, cfgs, paths)
	}

	if opts.Foreground {
		return runForeground(ctx, cfgs, paths)
	}

	return startBackground(ctx, cfgs, paths, opts)
}

// Ensure launches or restarts the daemon so that the configured set
// matches opts.ConfigDirs and /health returns 200 on every port.
// Returns started=true if a new daemon was launched.
func Ensure(ctx context.Context, opts Options) (bool, error) {
	if err := refuseRoot(); err != nil {
		return false, err
	}

	cfgs, err := LoadConfigDirs(opts.ConfigDirs)
	if err != nil {
		return false, err
	}
	paths, err := ResolveStatePaths(opts.StateDir)
	if err != nil {
		return false, err
	}

	want := Digest(cfgs)
	have, healthy := inspectRunning(paths, cfgs)
	if healthy && have == want {
		return false, nil
	}
	if have != "" {
		if err := stopByPidfile(paths, 10*time.Second); err != nil {
			return false, fmt.Errorf("stop stale daemon: %w", err)
		}
	}
	if err := startBackground(ctx, cfgs, paths, opts); err != nil {
		return false, err
	}
	return true, nil
}

// Stop terminates a running daemon. Idempotent.
func Stop(ctx context.Context, stateDir string) error {
	paths, err := ResolveStatePaths(stateDir)
	if err != nil {
		return err
	}
	return stopByPidfile(paths, 10*time.Second)
}

// Logs prints the contents of the serve log file to w. Follow=true
// tails it by repeatedly reading EOF until ctx is done.
func Logs(ctx context.Context, w io.Writer, stateDir string, follow bool) error {
	paths, err := ResolveStatePaths(stateDir)
	if err != nil {
		return err
	}
	f, err := os.Open(paths.Log)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	defer f.Close()
	if !follow {
		_, err = io.Copy(w, f)
		return err
	}
	br := bufio.NewReader(f)
	for {
		if err := ctx.Err(); err != nil {
			return nil
		}
		line, err := br.ReadString('\n')
		if len(line) > 0 {
			_, _ = w.Write([]byte(line))
		}
		if err == io.EOF {
			select {
			case <-ctx.Done():
				return nil
			case <-time.After(200 * time.Millisecond):
			}
			continue
		}
		if err != nil {
			return err
		}
	}
}

// --- internal helpers ---

// inspectRunning reports the digest stored beside a live daemon, and
// whether /health on every configured port returns 200 right now.
// Returns ("", false) when no daemon is running.
func inspectRunning(paths StatePaths, cfgs []*Config) (string, bool) {
	pid, err := ReadPidfile(paths.Pid)
	if err != nil || pid == 0 || !PidAlive(pid) {
		return "", false
	}
	data, err := os.ReadFile(paths.Config)
	if err != nil {
		return "", false
	}
	var snap struct {
		Digest string `json:"digest"`
	}
	if err := json.Unmarshal(data, &snap); err != nil {
		return "", false
	}
	urls := make([]string, 0, len(cfgs))
	for _, c := range cfgs {
		urls = append(urls, fmt.Sprintf("http://127.0.0.1:%d/health", c.Port))
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	healthy := waitHealthy(ctx, urls, 2*time.Second) == nil
	return snap.Digest, healthy
}

// spawnFn is an injection point for tests; defaults to the real re-exec.
var spawnFn = spawnBackground

func startBackground(ctx context.Context, cfgs []*Config, paths StatePaths, opts Options) error {
	execPath := opts.ExecPath
	if execPath == "" {
		p, err := os.Executable()
		if err != nil {
			return fmt.Errorf("executable: %w", err)
		}
		execPath = p
	}
	args := []string{"serve", "--foreground", "--state-dir", paths.Dir}
	for _, d := range opts.ConfigDirs {
		args = append(args, "-c", d)
	}
	pid, err := spawnFn(execPath, args, paths)
	if err != nil {
		return err
	}
	healthTimeout := opts.HealthTimeout
	if healthTimeout == 0 {
		healthTimeout = 10 * time.Second
	}
	urls := make([]string, 0, len(cfgs))
	for _, c := range cfgs {
		urls = append(urls, fmt.Sprintf("http://127.0.0.1:%d/health", c.Port))
	}
	if err := waitHealthy(ctx, urls, healthTimeout); err != nil {
		return fmt.Errorf("daemon pid %d started but not healthy: %w", pid, err)
	}
	return nil
}

// runForeground runs the daemon body in-process with console logging to
// stderr. Does NOT write a pidfile — the point of foreground is to
// opt out of the single-instance contract.
func runForeground(parent context.Context, cfgs []*Config, paths StatePaths) error {
	logger := newConsoleLogger()
	defer func() { _ = logger.Sync() }()
	ctx, cancel := withSignals(parent)
	defer cancel()
	servers, _, err := buildServers(ctx, cfgs, logger)
	if err != nil {
		return err
	}
	return runDaemon(ctx, servers, logger)
}

// runAsDaemon is the child's entry point. Writes pidfile and digest
// snapshot, runs servers, removes pidfile on exit.
func runAsDaemon(parent context.Context, cfgs []*Config, paths StatePaths) (retErr error) {
	logger := newJSONLogger()
	defer func() { _ = logger.Sync() }()

	if err := WritePidfile(paths.Pid, os.Getpid()); err != nil {
		logger.Error("write pidfile", zap.Error(err))
		return err
	}
	defer func() {
		_ = os.Remove(paths.Pid)
	}()

	snap := map[string]string{"digest": Digest(cfgs)}
	data, _ := json.Marshal(snap)
	if err := os.WriteFile(paths.Config, data, 0o600); err != nil {
		logger.Error("write config snapshot", zap.Error(err))
		return err
	}
	defer func() {
		_ = os.Remove(paths.Config)
	}()

	defer func() {
		if r := recover(); r != nil {
			logger.Error("daemon panic", zap.Any("panic", r))
			retErr = fmt.Errorf("daemon panic: %v", r)
		}
	}()

	ctx, cancel := withSignals(parent)
	defer cancel()
	servers, _, err := buildServers(ctx, cfgs, logger)
	if err != nil {
		logger.Error("build servers", zap.Error(err))
		return err
	}
	return runDaemon(ctx, servers, logger)
}

// refuseRoot honors SERVE_FEATURE.md: the server must refuse to run as
// UID 0.
func refuseRoot() error {
	if os.Geteuid() == 0 {
		return errors.New("y-cluster serve refuses to run as root; use an unprivileged user")
	}
	return nil
}

// --- loggers ---

// newJSONLogger is used in the background daemon per Q-S1.
func newJSONLogger() *zap.Logger {
	cfg := zap.NewProductionConfig()
	cfg.OutputPaths = []string{"stdout"}
	cfg.ErrorOutputPaths = []string{"stderr"}
	cfg.EncoderConfig.EncodeTime = zapcore.ISO8601TimeEncoder
	logger, err := cfg.Build()
	if err != nil {
		panic(err)
	}
	return logger
}

// newConsoleLogger is used in --foreground so humans reading the tty
// see readable output.
func newConsoleLogger() *zap.Logger {
	cfg := zap.NewDevelopmentConfig()
	cfg.OutputPaths = []string{"stderr"}
	cfg.ErrorOutputPaths = []string{"stderr"}
	logger, err := cfg.Build()
	if err != nil {
		panic(err)
	}
	return logger
}
