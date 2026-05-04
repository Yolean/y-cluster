package serve

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// seedYKCfg writes a minimal y-kustomize-local config plus a
// kustomize source dir that emits one Secret with one data key.
// Returns the absolute config dir path.
func seedYKCfg(t *testing.T, port int) string {
	t.Helper()
	root := t.TempDir()
	cfgDir := filepath.Join(root, "config")
	srcDir := filepath.Join(root, "src")
	if err := os.MkdirAll(filepath.Join(srcDir, "blobs-setup-bucket-job"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(srcDir, "blobs-setup-bucket-job/values.yaml"),
		[]byte("bucket: builds\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	kust := `apiVersion: kustomize.config.k8s.io/v1beta1
kind: Kustomization
secretGenerator:
- name: y-kustomize.blobs.setup-bucket-job
  options:
    disableNameSuffixHash: true
  files:
  - blobs-setup-bucket-job/values.yaml
`
	if err := os.WriteFile(filepath.Join(srcDir, "kustomization.yaml"), []byte(kust), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(cfgDir, 0o755); err != nil {
		t.Fatal(err)
	}
	body := fmt.Sprintf("port: %d\ntype: y-kustomize-local\nsources:\n- dir: %s\n", port, srcDir)
	if err := os.WriteFile(filepath.Join(cfgDir, ConfigFilename), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	return cfgDir
}

// portFromNet asks the kernel for a free TCP port. There is a tiny
// window between release and re-bind, but it is acceptable for tests.
func portFromNet(t *testing.T) int {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := l.Addr().(*net.TCPAddr).Port
	_ = l.Close()
	return port
}

// installFakeSpawn replaces spawnFn with an in-process runAsDaemon for
// the duration of the test. The "daemon" writes its pidfile with the
// test process's own pid (which is alive), runs until the returned
// cancel is called, and is cleaned up on test end.
func installFakeSpawn(t *testing.T) (cancel func()) {
	t.Helper()
	var mu sync.Mutex
	var cancels []context.CancelFunc
	var wg sync.WaitGroup

	orig := spawnFn
	spawnFn = func(execPath string, args []string, paths StatePaths) (int, error) {
		// Parse -c dirs from args
		var dirs []string
		for i := 0; i < len(args); i++ {
			if args[i] == "-c" && i+1 < len(args) {
				dirs = append(dirs, args[i+1])
			}
		}
		cfgs, err := LoadConfigDirs(dirs)
		if err != nil {
			return 0, err
		}

		ctx, c := context.WithCancel(context.Background())
		mu.Lock()
		cancels = append(cancels, c)
		mu.Unlock()

		wg.Add(1)
		go func() {
			defer wg.Done()
			_ = runAsDaemon(ctx, cfgs, paths)
		}()

		// Wait for pidfile to appear
		deadline := time.Now().Add(2 * time.Second)
		for time.Now().Before(deadline) {
			if pid, _ := ReadPidfile(paths.Pid); pid > 0 {
				return pid, nil
			}
			time.Sleep(10 * time.Millisecond)
		}
		return 0, fmt.Errorf("fake daemon never wrote pidfile")
	}
	t.Cleanup(func() {
		mu.Lock()
		for _, c := range cancels {
			c()
		}
		mu.Unlock()
		wg.Wait()
		spawnFn = orig
	})
	return func() {
		mu.Lock()
		for _, c := range cancels {
			c()
		}
		cancels = nil
		mu.Unlock()
		wg.Wait()
	}
}

func TestRun_Foreground(t *testing.T) {
	port := portFromNet(t)
	cfgDir := seedYKCfg(t, port)
	stateDir := t.TempDir()

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- Run(ctx, Options{
			ConfigDirs: []string{cfgDir},
			Foreground: true,
			StateDir:   stateDir,
		})
	}()

	// Wait until /health answers
	url := fmt.Sprintf("http://127.0.0.1:%d/health", port)
	if err := waitHealthy(context.Background(), []string{url}, 5*time.Second); err != nil {
		cancel()
		<-done
		t.Fatal(err)
	}

	// Known file served
	resp, err := http.Get(fmt.Sprintf("http://127.0.0.1:%d/v1/blobs/setup-bucket-job/values.yaml", port))
	if err != nil {
		cancel()
		<-done
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if !strings.Contains(string(body), "bucket") {
		cancel()
		<-done
		t.Fatalf("body: %q", body)
	}

	// openapi served
	resp, err = http.Get(fmt.Sprintf("http://127.0.0.1:%d/openapi.yaml", port))
	if err != nil {
		cancel()
		<-done
		t.Fatal(err)
	}
	oa, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if !strings.Contains(string(oa), "/v1/blobs/setup-bucket-job/values.yaml") {
		cancel()
		<-done
		t.Fatalf("openapi: %s", oa)
	}

	cancel()
	if err := <-done; err != nil {
		t.Fatalf("run: %v", err)
	}
}

func TestRun_BackgroundViaFakeSpawn(t *testing.T) {
	installFakeSpawn(t)

	port := portFromNet(t)
	cfgDir := seedYKCfg(t, port)
	stateDir := t.TempDir()

	if err := Run(context.Background(), Options{
		ConfigDirs: []string{cfgDir},
		Foreground: false,
		StateDir:   stateDir,
		ExecPath:   "/usr/bin/true", // never actually exec'd; fake spawn ignores
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(stateDir, "serve.pid")); err != nil {
		t.Fatalf("pidfile missing: %v", err)
	}
	if _, err := os.Stat(filepath.Join(stateDir, "serve.config.json")); err != nil {
		t.Fatalf("config snapshot missing: %v", err)
	}
}

func TestEnsure_FirstStartAndNoop(t *testing.T) {
	installFakeSpawn(t)

	port := portFromNet(t)
	cfgDir := seedYKCfg(t, port)
	stateDir := t.TempDir()

	res, err := Ensure(context.Background(), Options{
		ConfigDirs: []string{cfgDir},
		StateDir:   stateDir,
		ExecPath:   "/usr/bin/true",
	})
	if err != nil {
		t.Fatal(err)
	}
	if res.Action != EnsureStarted {
		t.Fatalf("first ensure: action=%s want started", res.Action)
	}
	if len(res.Ports) != 1 || res.Ports[0] != port {
		t.Fatalf("ports=%v want [%d]", res.Ports, port)
	}
	// Digest is the sha256 hex of the normalized config -- 64
	// chars. The CLI truncates to 12 for display, but the
	// EnsureResult carries the full string so callers that want
	// to compare programmatically can.
	if len(res.Digest) != 64 {
		t.Errorf("digest length: got %d, want 64 (full sha256 hex)", len(res.Digest))
	}

	res2, err := Ensure(context.Background(), Options{
		ConfigDirs: []string{cfgDir},
		StateDir:   stateDir,
		ExecPath:   "/usr/bin/true",
	})
	if err != nil {
		t.Fatal(err)
	}
	if res2.Action != EnsureNoop {
		t.Fatalf("second ensure: action=%s want noop", res2.Action)
	}
	// Same config, same digest -- the noop branch must
	// surface the same value the started branch did, so an
	// operator who compares the two outputs sees a match.
	if res2.Digest != res.Digest {
		t.Errorf("digest noop=%q started=%q; should be identical for same config", res2.Digest, res.Digest)
	}
}

// TestEnsure_RestartWhenStaleStatePresent covers the "daemon already
// died, stale pidfile left behind" path without requiring us to SIGTERM
// the live test process (which stopByPidfile would escalate to SIGKILL,
// killing the test itself).
func TestEnsure_RestartWhenStaleStatePresent(t *testing.T) {
	installFakeSpawn(t)

	port := portFromNet(t)
	cfgDir := seedYKCfg(t, port)
	stateDir := t.TempDir()

	// Pretend a previous daemon ran but died.
	if err := WritePidfile(filepath.Join(stateDir, "serve.pid"), 99999999); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(stateDir, "serve.config.json"),
		[]byte(`{"digest":"stale"}`), 0o600); err != nil {
		t.Fatal(err)
	}

	res, err := Ensure(context.Background(), Options{
		ConfigDirs: []string{cfgDir},
		StateDir:   stateDir,
		ExecPath:   "/usr/bin/true",
	})
	if err != nil {
		t.Fatal(err)
	}
	if res.Action != EnsureRestarted {
		t.Fatalf("ensure with stale pidfile: action=%s want restarted", res.Action)
	}
}

func TestStop_Idempotent(t *testing.T) {
	stateDir := t.TempDir()
	if err := Stop(context.Background(), stateDir); err != nil {
		t.Fatal(err)
	}
	// Stale pidfile
	pidfile := filepath.Join(stateDir, "serve.pid")
	if err := os.WriteFile(pidfile, []byte("99999999\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := Stop(context.Background(), stateDir); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(pidfile); !os.IsNotExist(err) {
		t.Fatal("pidfile should be gone after stop on stale")
	}
}

func TestStop_CorruptPidfile(t *testing.T) {
	stateDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(stateDir, "serve.pid"), []byte("not-a-number"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := Stop(context.Background(), stateDir); err != nil {
		t.Fatal(err)
	}
}


func TestLogs_Empty(t *testing.T) {
	var buf bytes.Buffer
	if err := Logs(context.Background(), &buf, t.TempDir(), false); err != nil {
		t.Fatal(err)
	}
	if buf.Len() != 0 {
		t.Fatalf("expected empty, got %q", buf.String())
	}
}

func TestLogs_ReadsFile(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "serve.log"), []byte("hello\nworld\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	if err := Logs(context.Background(), &buf, dir, false); err != nil {
		t.Fatal(err)
	}
	if buf.String() != "hello\nworld\n" {
		t.Fatalf("got %q", buf.String())
	}
}

func TestLogs_Follow(t *testing.T) {
	dir := t.TempDir()
	logPath := filepath.Join(dir, "serve.log")
	if err := os.WriteFile(logPath, []byte("first\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()
	var buf bytes.Buffer
	done := make(chan error, 1)
	go func() {
		done <- Logs(ctx, &buf, dir, true)
	}()

	// Append while following
	time.Sleep(100 * time.Millisecond)
	f, err := os.OpenFile(logPath, os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString("second\n"); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}

	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(buf.String(), "first") || !strings.Contains(buf.String(), "second") {
		t.Fatalf("follow missed a line: %q", buf.String())
	}
}

func TestBuildServers_StaticMissingDir(t *testing.T) {
	// A typo in static.dir used to be masked by the "not implemented"
	// stub. Now that static is wired up, verify the error surfaces at
	// buildServers time so the user notices during `serve ensure`.
	c := &Config{Port: 1, Type: TypeStatic, Static: &StaticConfig{Dir: "/does/not/exist"}, Dir: t.TempDir()}
	logger := newConsoleLogger()
	_, _, err := buildServers(context.Background(), []*Config{c}, logger)
	if err == nil {
		t.Fatal("want error for missing static dir")
	}
}

func TestBuildServers_UnknownType(t *testing.T) {
	c := &Config{Port: 1, Type: BackendType("weird"), Dir: t.TempDir()}
	logger := newConsoleLogger()
	_, _, err := buildServers(context.Background(), []*Config{c}, logger)
	if err == nil || !strings.Contains(err.Error(), "unknown type") {
		t.Fatalf("want unknown-type, got %v", err)
	}
}

func TestRefuseRoot_WhenNotRoot(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("running as root")
	}
	if err := refuseRoot(); err != nil {
		t.Fatalf("refuseRoot should pass when not root: %v", err)
	}
}

func TestLoggers_Build(t *testing.T) {
	j := newJSONLogger()
	if j == nil {
		t.Fatal("json logger nil")
	}
	_ = j.Sync()
	c := newConsoleLogger()
	if c == nil {
		t.Fatal("console logger nil")
	}
	_ = c.Sync()
}
