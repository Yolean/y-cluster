package serve

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestDefaultStateDir_EnvOverride(t *testing.T) {
	t.Setenv(stateDirEnv, "/tmp/override")
	got, err := DefaultStateDir()
	if err != nil {
		t.Fatal(err)
	}
	if got != "/tmp/override" {
		t.Fatalf("env override ignored: %s", got)
	}
}

func TestDefaultStateDir_PerOS(t *testing.T) {
	t.Setenv(stateDirEnv, "")
	switch runtime.GOOS {
	case "linux":
		t.Setenv("XDG_STATE_HOME", "/tmp/xdg")
		got, err := DefaultStateDir()
		if err != nil {
			t.Fatal(err)
		}
		if got != "/tmp/xdg/y-cluster/serve" {
			t.Fatalf("xdg: %s", got)
		}
		t.Setenv("XDG_STATE_HOME", "")
		got, err = DefaultStateDir()
		if err != nil {
			t.Fatal(err)
		}
		if !strings.HasSuffix(got, ".local/state/y-cluster/serve") {
			t.Fatalf("fallback: %s", got)
		}
	case "darwin":
		got, err := DefaultStateDir()
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(got, "Library/Application Support/y-cluster/serve") {
			t.Fatalf("darwin: %s", got)
		}
	}
}

func TestResolveStatePaths_CreatesDir(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "a", "b")
	sp, err := ResolveStatePaths(dir)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(dir); err != nil {
		t.Fatal(err)
	}
	if sp.Pid == "" || sp.Log == "" || sp.Config == "" {
		t.Fatal("paths empty")
	}
}

func TestResolveStatePaths_EmptyUsesDefault(t *testing.T) {
	t.Setenv(stateDirEnv, filepath.Join(t.TempDir(), "envdir"))
	sp, err := ResolveStatePaths("")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasSuffix(sp.Dir, "envdir") {
		t.Fatalf("dir: %s", sp.Dir)
	}
}

func TestPidfileRoundtrip(t *testing.T) {
	p := filepath.Join(t.TempDir(), "pid")
	if pid, err := ReadPidfile(p); err != nil || pid != 0 {
		t.Fatalf("missing pidfile should be (0, nil), got (%d, %v)", pid, err)
	}
	if err := WritePidfile(p, 1234); err != nil {
		t.Fatal(err)
	}
	pid, err := ReadPidfile(p)
	if err != nil || pid != 1234 {
		t.Fatalf("round-trip: %d %v", pid, err)
	}
}

func TestReadPidfile_Corrupt(t *testing.T) {
	p := filepath.Join(t.TempDir(), "pid")
	if err := os.WriteFile(p, []byte("not-a-number"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := ReadPidfile(p); err == nil {
		t.Fatal("want parse error")
	}
}

func TestPidAlive_SelfAndZero(t *testing.T) {
	if !PidAlive(os.Getpid()) {
		t.Fatal("self should be alive")
	}
	if PidAlive(0) || PidAlive(-1) {
		t.Fatal("0/-1 should be not alive")
	}
	if PidAlive(0x7fffffff) {
		t.Fatal("unlikely pid should be not alive")
	}
}
