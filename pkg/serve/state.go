package serve

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"syscall"
)

// stateDirEnv lets tests (and advanced users) pin the state dir without
// depending on OS-specific env vars.
const stateDirEnv = "Y_CLUSTER_SERVE_STATE_DIR"

// StatePaths groups the files the daemon and CLI share via the state dir.
type StatePaths struct {
	Dir    string
	Pid    string // serve.pid
	Log    string // serve.log
	Config string // serve.config.json (normalized digest/manifest)
}

// DefaultStateDir resolves the per-user state directory using OS
// conventions. Callers that already know the directory should pass it
// explicitly to ResolveStatePaths instead.
func DefaultStateDir() (string, error) {
	if v := os.Getenv(stateDirEnv); v != "" {
		return filepath.Clean(v), nil
	}
	switch runtime.GOOS {
	case "darwin":
		home, err := os.UserHomeDir()
		if err != nil {
			return "", err
		}
		return filepath.Join(home, "Library", "Application Support", "y-cluster", "serve"), nil
	case "windows":
		if v := os.Getenv("LocalAppData"); v != "" {
			return filepath.Join(v, "y-cluster", "serve"), nil
		}
		home, err := os.UserHomeDir()
		if err != nil {
			return "", err
		}
		return filepath.Join(home, "AppData", "Local", "y-cluster", "serve"), nil
	default:
		if v := os.Getenv("XDG_STATE_HOME"); v != "" {
			return filepath.Join(v, "y-cluster", "serve"), nil
		}
		home, err := os.UserHomeDir()
		if err != nil {
			return "", err
		}
		return filepath.Join(home, ".local", "state", "y-cluster", "serve"), nil
	}
}

// ResolveStatePaths returns the full set of paths, creating the dir if
// it does not exist.
func ResolveStatePaths(dir string) (StatePaths, error) {
	if dir == "" {
		d, err := DefaultStateDir()
		if err != nil {
			return StatePaths{}, err
		}
		dir = d
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return StatePaths{}, fmt.Errorf("create state dir %s: %w", dir, err)
	}
	return StatePaths{
		Dir:    dir,
		Pid:    filepath.Join(dir, "serve.pid"),
		Log:    filepath.Join(dir, "serve.log"),
		Config: filepath.Join(dir, "serve.config.json"),
	}, nil
}

// WritePidfile writes pid to the pidfile with 0600 perms.
func WritePidfile(path string, pid int) error {
	return os.WriteFile(path, []byte(strconv.Itoa(pid)+"\n"), 0o600)
}

// ReadPidfile returns the pid; (0, nil) if the pidfile does not exist.
func ReadPidfile(path string) (int, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return 0, nil
		}
		return 0, err
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(data)))
	if err != nil {
		return 0, fmt.Errorf("pidfile %s: %w", path, err)
	}
	return pid, nil
}

// PidAlive reports whether pid is alive. Returns false for pid==0.
func PidAlive(pid int) bool {
	if pid <= 0 {
		return false
	}
	proc, err := os.FindProcess(pid)
	if err != nil {
		return false
	}
	return proc.Signal(syscall.Signal(0)) == nil
}
