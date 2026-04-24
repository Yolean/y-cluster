//go:build e2e

// Package e2e tests y-cluster serve against the built binary.
//
// Fixture layout mirrors the ystack y-converge-checks-dag two-base pattern:
// a single y-cluster-serve.yaml pointing to two sources, each with a
// y-kustomize-bases/{group}/{name}/ tree.
//
// This test drives the CLI as an end-user would: build the binary, run
// `serve ensure`, hit the endpoints, then `serve stop`. It is the same
// path .github/workflows/e2e-release.yaml will take against a released
// archive.
package e2e

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

var (
	serveBinaryOnce sync.Once
	serveBinaryPath string
	serveBinaryErr  error
)

// buildServeBinary compiles cmd/y-cluster once per test process.
func buildServeBinary(t *testing.T) string {
	t.Helper()
	serveBinaryOnce.Do(func() {
		dir, err := os.MkdirTemp("", "y-cluster-serve-bin-*")
		if err != nil {
			serveBinaryErr = err
			return
		}
		out := filepath.Join(dir, "y-cluster")
		cmd := exec.Command("go", "build", "-o", out, "./cmd/y-cluster")
		cmd.Dir = ".."
		if outb, err := cmd.CombinedOutput(); err != nil {
			serveBinaryErr = fmt.Errorf("build: %s: %w", outb, err)
			return
		}
		serveBinaryPath = out
	})
	if serveBinaryErr != nil {
		t.Fatal(serveBinaryErr)
	}
	return serveBinaryPath
}

// freePort returns a TCP port that is free right now. Caller races any
// other process grabbing it, but the window is tiny.
func freePort(t *testing.T) int {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := l.Addr().(*net.TCPAddr).Port
	_ = l.Close()
	return port
}

// prepareFixture copies testdata/serve-ykustomize-local/ into a temp dir
// and substitutes __PORT__ in y-cluster-serve.yaml. Returns the absolute
// path of the prepared config directory.
func prepareFixture(t *testing.T, port int) string {
	t.Helper()
	src, err := filepath.Abs("../testdata/serve-ykustomize-local")
	if err != nil {
		t.Fatal(err)
	}
	dst := t.TempDir()
	if err := copyTree(src, dst); err != nil {
		t.Fatal(err)
	}
	cfgPath := filepath.Join(dst, "config", "y-cluster-serve.yaml")
	data, err := os.ReadFile(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	data = []byte(strings.ReplaceAll(string(data), "__PORT__", fmt.Sprintf("%d", port)))
	if err := os.WriteFile(cfgPath, data, 0o644); err != nil {
		t.Fatal(err)
	}
	return filepath.Join(dst, "config")
}

func copyTree(src, dst string) error {
	return filepath.Walk(src, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(src, path)
		if err != nil {
			return err
		}
		target := filepath.Join(dst, rel)
		if info.IsDir() {
			return os.MkdirAll(target, 0o755)
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		return os.WriteFile(target, data, info.Mode().Perm())
	})
}

func runServe(t *testing.T, bin, stateDir string, args ...string) ([]byte, error) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, bin, args...)
	cmd.Env = append(os.Environ(), "Y_CLUSTER_SERVE_STATE_DIR="+stateDir)
	return cmd.CombinedOutput()
}

func TestServe_EnsureRoundtrip(t *testing.T) {
	bin := buildServeBinary(t)
	port := freePort(t)
	cfgDir := prepareFixture(t, port)
	stateDir := t.TempDir()

	// 1. ensure → daemon starts, /health 200 on the configured port
	if out, err := runServe(t, bin, stateDir, "serve", "ensure", "-c", cfgDir); err != nil {
		t.Fatalf("ensure: %v\n%s", err, out)
	}
	t.Cleanup(func() {
		_, _ = runServe(t, bin, stateDir, "serve", "stop")
	})

	if err := httpGetStatus(fmt.Sprintf("http://127.0.0.1:%d/health", port)); err != nil {
		t.Fatalf("health: %v", err)
	}

	// 2. known routes from each source are served
	body, hdr, err := httpGet(fmt.Sprintf("http://127.0.0.1:%d/v1/blobs/setup-bucket-job/base-for-annotations.yaml", port))
	if err != nil {
		t.Fatalf("GET blobs: %v", err)
	}
	if !strings.Contains(string(body), "setup-bucket-job") {
		t.Fatalf("body missing marker: %q", body)
	}
	if ct := hdr.Get("Content-Type"); !strings.HasPrefix(ct, "application/yaml") {
		t.Fatalf("content-type: got %q, want application/yaml*", ct)
	}
	etag := hdr.Get("ETag")
	if etag == "" {
		t.Fatal("missing ETag")
	}
	if cc := hdr.Get("Cache-Control"); !strings.Contains(cc, "no-cache") {
		t.Fatalf("cache-control: got %q, want no-cache", cc)
	}

	// 3. conditional GET with matching ETag → 304
	code, err := httpGetWithETag(fmt.Sprintf("http://127.0.0.1:%d/v1/blobs/setup-bucket-job/base-for-annotations.yaml", port), etag)
	if err != nil {
		t.Fatalf("conditional GET: %v", err)
	}
	if code != http.StatusNotModified {
		t.Fatalf("conditional GET: got %d, want 304", code)
	}

	// 4. other source is merged under the same /v1/ namespace
	if _, _, err := httpGet(fmt.Sprintf("http://127.0.0.1:%d/v1/kafka/setup-topic-job/base-for-annotations.yaml", port)); err != nil {
		t.Fatalf("GET kafka: %v", err)
	}

	// 5. openapi snapshot lists every served path
	body, _, err = httpGet(fmt.Sprintf("http://127.0.0.1:%d/openapi.yaml", port))
	if err != nil {
		t.Fatalf("openapi: %v", err)
	}
	for _, want := range []string{
		"/v1/blobs/setup-bucket-job/base-for-annotations.yaml",
		"/v1/blobs/setup-bucket-job/values.yaml",
		"/v1/kafka/setup-topic-job/base-for-annotations.yaml",
	} {
		if !strings.Contains(string(body), want) {
			t.Fatalf("openapi missing %s", want)
		}
	}

	// 6. ensure a second time → no-op (pid unchanged)
	pidBefore, err := os.ReadFile(filepath.Join(stateDir, "serve.pid"))
	if err != nil {
		t.Fatalf("read pid: %v", err)
	}
	if out, err := runServe(t, bin, stateDir, "serve", "ensure", "-c", cfgDir); err != nil {
		t.Fatalf("ensure#2: %v\n%s", err, out)
	}
	pidAfter, err := os.ReadFile(filepath.Join(stateDir, "serve.pid"))
	if err != nil {
		t.Fatalf("read pid: %v", err)
	}
	if string(pidBefore) != string(pidAfter) {
		t.Fatalf("daemon restarted on identical ensure: %s → %s", pidBefore, pidAfter)
	}

	// 7. stop → pidfile gone, /health errors
	if out, err := runServe(t, bin, stateDir, "serve", "stop"); err != nil {
		t.Fatalf("stop: %v\n%s", err, out)
	}
	if _, err := os.Stat(filepath.Join(stateDir, "serve.pid")); !os.IsNotExist(err) {
		t.Fatalf("pidfile should be gone, err=%v", err)
	}

	// 8. stop is idempotent
	if out, err := runServe(t, bin, stateDir, "serve", "stop"); err != nil {
		t.Fatalf("stop#2: %v\n%s", err, out)
	}
}

func TestServe_LogsSubcommand(t *testing.T) {
	bin := buildServeBinary(t)
	port := freePort(t)
	cfgDir := prepareFixture(t, port)
	stateDir := t.TempDir()

	if out, err := runServe(t, bin, stateDir, "serve", "ensure", "-c", cfgDir); err != nil {
		t.Fatalf("ensure: %v\n%s", err, out)
	}
	t.Cleanup(func() {
		_, _ = runServe(t, bin, stateDir, "serve", "stop")
	})

	// Trigger at least one log line
	_, _, _ = httpGet(fmt.Sprintf("http://127.0.0.1:%d/health", port))

	out, err := runServe(t, bin, stateDir, "serve", "logs")
	if err != nil {
		t.Fatalf("logs: %v\n%s", err, out)
	}
	// Background daemon uses JSON logging per Q-S1. Expect valid JSON lines.
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		if line == "" {
			continue
		}
		var j map[string]any
		if err := json.Unmarshal([]byte(line), &j); err != nil {
			t.Fatalf("log line not JSON: %q: %v", line, err)
		}
	}
}

func httpGet(url string) ([]byte, http.Header, error) {
	resp, err := retryGET(url)
	if err != nil {
		return nil, nil, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, nil, err
	}
	if resp.StatusCode != http.StatusOK {
		return nil, nil, fmt.Errorf("status %d: %s", resp.StatusCode, body)
	}
	return body, resp.Header, nil
}

func httpGetStatus(url string) error {
	resp, err := retryGET(url)
	if err != nil {
		return err
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("status %d", resp.StatusCode)
	}
	return nil
}

func httpGetWithETag(url, etag string) (int, error) {
	req, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		return 0, err
	}
	req.Header.Set("If-None-Match", etag)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return 0, err
	}
	resp.Body.Close()
	return resp.StatusCode, nil
}

// retryGET tolerates the short window after `ensure` returns where the
// listener might still be binding. Ensure itself is supposed to wait for
// /health, but tests should not race on restart.
func retryGET(url string) (*http.Response, error) {
	var last error
	for i := 0; i < 40; i++ {
		resp, err := http.Get(url)
		if err == nil {
			return resp, nil
		}
		last = err
		time.Sleep(50 * time.Millisecond)
	}
	return nil, last
}
