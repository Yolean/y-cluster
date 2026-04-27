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

// prepareFixture copies a testdata/<name>/ tree into a temp dir and
// substitutes __PORT__ in y-cluster-serve.yaml. Returns the absolute
// path of the prepared config directory.
func prepareFixture(t *testing.T, name string, port int) string {
	t.Helper()
	src, err := filepath.Abs(filepath.Join("../testdata", name))
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
	cfgDir := prepareFixture(t, "serve-ykustomize-local", port)
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
	cfgDir := prepareFixture(t, "serve-ykustomize-local", port)
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

// waitForHealth polls /health and runs predicate on the decoded body
// until it returns true or timeout elapses. Use this for any check
// that depends on the in-cluster watch having seen a particular
// state — apply/patch/delete propagation, route count, etc. Polling
// /health (rather than a specific route URL) keeps each iteration
// cheap and decoupled from whatever the test is actually verifying.
func waitForHealth(t *testing.T, url string, predicate func(map[string]any) bool, timeout time.Duration, what string) map[string]any {
	t.Helper()
	deadline := time.Now().Add(timeout)
	var last map[string]any
	var lastErr error
	for {
		body, _, err := httpGet(url)
		if err == nil {
			var h map[string]any
			if err := json.Unmarshal(body, &h); err == nil {
				last = h
				if predicate(h) {
					return h
				}
			} else {
				lastErr = fmt.Errorf("decode: %w", err)
			}
		} else {
			lastErr = err
		}
		if time.Now().After(deadline) {
			t.Fatalf("waitForHealth %q after %s: last health=%v lastErr=%v", what, timeout, last, lastErr)
		}
		time.Sleep(100 * time.Millisecond)
	}
}

// waitForStatus polls a URL until it returns the expected status or
// timeout. Useful when the test cares specifically about a route's
// presence/absence — e.g. after deleting a Secret, we expect 404.
func waitForStatus(t *testing.T, url string, want int, timeout time.Duration, what string) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	var last int
	var lastErr error
	for {
		resp, err := http.Get(url)
		if err == nil {
			last = resp.StatusCode
			resp.Body.Close()
			if resp.StatusCode == want {
				return
			}
		} else {
			lastErr = err
		}
		if time.Now().After(deadline) {
			t.Fatalf("waitForStatus %q after %s: want %d, last=%d lastErr=%v", what, timeout, want, last, lastErr)
		}
		time.Sleep(100 * time.Millisecond)
	}
}

// TestServe_InCluster covers the y-kustomize-in-cluster backend
// against a real kwok cluster. The test plays out the workflow that
// will replace ystack's y-kustomize deployment: apply a Secret with
// the y-kustomize convention, serve it, mutate it, verify the watch
// propagates, then delete it and verify the route disappears.
//
// The fixture files also serve as documentation for ystack migration;
// see docs/ystack-migration.md on the spec branch.
func TestServe_InCluster(t *testing.T) {
	setupCluster(t)
	bin := buildServeBinary(t)
	port := freePort(t)

	// Prepare the fixture with kubeconfig + port substituted.
	src, err := filepath.Abs("../testdata/serve-ykustomize-incluster")
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
	rendered := strings.ReplaceAll(string(data), "__PORT__", fmt.Sprintf("%d", port))
	rendered = strings.ReplaceAll(rendered, "__KUBECONFIG__", clusterKubeconfig)
	if err := os.WriteFile(cfgPath, []byte(rendered), 0o644); err != nil {
		t.Fatal(err)
	}
	cfgDir := filepath.Join(dst, "config")

	// Clean slate: remove the Secret if a previous run left it
	// behind. KUBECONFIG must be explicit — relying on the env set
	// by setupCluster races other tests that may have unset it via
	// their own t.Cleanup.
	secretName := "y-kustomize.blobs.setup-bucket-job"
	clean := exec.Command("kubectl", "--context="+contextName, "delete", "secret", secretName,
		"--ignore-not-found=true", "--namespace=default")
	clean.Env = append(os.Environ(), "KUBECONFIG="+clusterKubeconfig)
	_ = clean.Run()

	// Apply the initial Secret. This is the same manifest a
	// ystack-style module would ship.
	secretPath := filepath.Join(dst, "secrets", "blobs.yaml")
	apply := exec.Command("kubectl", "--context="+contextName, "apply", "-f", secretPath)
	apply.Env = append(os.Environ(), "KUBECONFIG="+clusterKubeconfig)
	if out, err := apply.CombinedOutput(); err != nil {
		t.Fatalf("apply initial secret: %s: %v", out, err)
	}
	t.Cleanup(func() {
		cleanup := exec.Command("kubectl", "--context="+contextName, "delete", "secret", secretName,
			"--ignore-not-found=true", "--namespace=default")
		cleanup.Env = append(os.Environ(), "KUBECONFIG="+clusterKubeconfig)
		_ = cleanup.Run()
	})

	stateDir := t.TempDir()
	if out, err := runServe(t, bin, stateDir, "serve", "ensure", "-c", cfgDir); err != nil {
		t.Fatalf("ensure: %v\n%s", err, out)
	}
	t.Cleanup(func() {
		_, _ = runServe(t, bin, stateDir, "serve", "stop", "--state-dir", stateDir)
	})

	healthURL := fmt.Sprintf("http://127.0.0.1:%d/health", port)
	routeURL := fmt.Sprintf("http://127.0.0.1:%d/v1/blobs/setup-bucket-job/base-for-annotations.yaml", port)
	valuesURL := fmt.Sprintf("http://127.0.0.1:%d/v1/blobs/setup-bucket-job/values.yaml", port)

	// First-run readiness: kwok's apiserver and our informer both
	// need a moment after `serve ensure` returns. WaitForCacheSync
	// is supposed to gate the daemon's listener, but on a freshly
	// started kwok container the LIST occasionally lands before the
	// just-applied Secret is visible. Wait on /health.routes
	// instead of polling the route URL — /health is the cheap
	// canonical status, the route URL is the test's actual subject.
	const propagation = 30 * time.Second
	h := waitForHealth(t, healthURL, func(h map[string]any) bool {
		r, ok := h["routes"].(float64)
		return ok && int(r) >= 1
	}, propagation, "initial routes >= 1")
	if h["namespace"] != "default" {
		t.Fatalf("health.namespace: %v", h["namespace"])
	}

	body, hdr, err := httpGet(routeURL)
	if err != nil {
		t.Fatalf("GET route: %v", err)
	}
	if !strings.Contains(string(body), "setup-bucket-job") {
		t.Fatalf("body missing marker: %q", body)
	}
	if ct := hdr.Get("Content-Type"); !strings.HasPrefix(ct, "application/yaml") {
		t.Fatalf("content-type: got %q, want application/yaml", ct)
	}

	// The second data key in the Secret is served as its own route.
	body, _, err = httpGet(valuesURL)
	if err != nil {
		t.Fatalf("GET values.yaml: %v", err)
	}
	if !strings.Contains(string(body), "bucket: builds") {
		t.Fatalf("values body: %q", body)
	}

	// openapi reflects the current watch state (SERVE_FEATURE.md says
	// the spec adapts to the watch -- rendered on every request).
	oa, _, err := httpGet(fmt.Sprintf("http://127.0.0.1:%d/openapi.yaml", port))
	if err != nil {
		t.Fatalf("openapi: %v", err)
	}
	if !strings.Contains(string(oa), "/v1/blobs/setup-bucket-job/base-for-annotations.yaml") {
		t.Fatalf("openapi missing route: %s", oa)
	}

	// Mutate the Secret's values.yaml; watch should propagate.
	patch := `{"stringData":{"values.yaml":"bucket: builds-v2\n"}}`
	p := exec.Command("kubectl", "--context="+contextName, "patch", "secret", secretName,
		"--namespace=default", "--type=merge", "-p", patch)
	p.Env = append(os.Environ(), "KUBECONFIG="+clusterKubeconfig)
	if out, err := p.CombinedOutput(); err != nil {
		t.Fatalf("patch: %s: %v", out, err)
	}

	patchedDeadline := time.Now().Add(propagation)
	for {
		body, _, err := httpGet(valuesURL)
		if err == nil && strings.Contains(string(body), "builds-v2") {
			break
		}
		if time.Now().After(patchedDeadline) {
			t.Fatalf("patched body never propagated within %s: %v", propagation, err)
		}
		time.Sleep(100 * time.Millisecond)
	}

	// Delete the Secret; route should 404 shortly.
	d := exec.Command("kubectl", "--context="+contextName, "delete", "secret", secretName,
		"--namespace=default")
	d.Env = append(os.Environ(), "KUBECONFIG="+clusterKubeconfig)
	if out, err := d.CombinedOutput(); err != nil {
		t.Fatalf("delete: %s: %v", out, err)
	}

	waitForStatus(t, routeURL, http.StatusNotFound, propagation, "route 404 after delete")
}

// TestServe_Static covers the static backend end to end: yamlToJson
// transform, dirTrailingSlash=redirect, and openapi snapshot. Uses
// testdata/serve-static/ as the worked example.
func TestServe_Static(t *testing.T) {
	bin := buildServeBinary(t)
	port := freePort(t)
	cfgDir := prepareFixture(t, "serve-static", port)
	stateDir := t.TempDir()

	if out, err := runServe(t, bin, stateDir, "serve", "ensure", "-c", cfgDir); err != nil {
		t.Fatalf("ensure: %v\n%s", err, out)
	}
	t.Cleanup(func() {
		_, _ = runServe(t, bin, stateDir, "serve", "stop", "--state-dir", stateDir)
	})

	if err := httpGetStatus(fmt.Sprintf("http://127.0.0.1:%d/health", port)); err != nil {
		t.Fatalf("health: %v", err)
	}

	// yamlToJson path: hello.yaml is served transformed.
	body, hdr, err := httpGet(fmt.Sprintf("http://127.0.0.1:%d/assets/greetings/hello.yaml", port))
	if err != nil {
		t.Fatalf("GET hello.yaml: %v", err)
	}
	if ct := hdr.Get("Content-Type"); ct != "application/json" {
		t.Fatalf("content-type: got %q, want application/json", ct)
	}
	var parsed map[string]any
	if err := json.Unmarshal(body, &parsed); err != nil {
		t.Fatalf("transformed body is not json: %v: %s", err, body)
	}
	if strings.Contains(string(body), "  ") {
		t.Fatalf("expected minified json, got %q", body)
	}

	// Non-yaml passes through unchanged.
	body, hdr, err = httpGet(fmt.Sprintf("http://127.0.0.1:%d/assets/README.txt", port))
	if err != nil {
		t.Fatalf("GET README.txt: %v", err)
	}
	if !strings.HasPrefix(hdr.Get("Content-Type"), "text/plain") {
		t.Fatalf("txt content-type: %s", hdr.Get("Content-Type"))
	}
	if !strings.Contains(string(body), "served by y-cluster serve") {
		t.Fatalf("text body: %q", body)
	}

	// dirTrailingSlash=redirect: hitting a directory without the
	// trailing slash redirects, query string preserved.
	client := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	resp, err := client.Get(fmt.Sprintf("http://127.0.0.1:%d/assets/greetings?pick=latest", port))
	if err != nil {
		t.Fatalf("GET dir: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusFound {
		t.Fatalf("dir redirect: got %d, want 302", resp.StatusCode)
	}
	if loc := resp.Header.Get("Location"); loc != "/assets/greetings/?pick=latest" {
		t.Fatalf("Location: %q", loc)
	}

	// openapi lists routes, with content-type reflecting the transform
	// (hello.yaml shows application/json, README.txt shows text/plain).
	oa, _, err := httpGet(fmt.Sprintf("http://127.0.0.1:%d/openapi.yaml", port))
	if err != nil {
		t.Fatalf("openapi: %v", err)
	}
	if !strings.Contains(string(oa), "/assets/greetings/hello.yaml") {
		t.Fatalf("openapi missing hello.yaml: %s", oa)
	}
	if !strings.Contains(string(oa), "application/json") {
		t.Fatalf("openapi should advertise json for transformed yaml: %s", oa)
	}
}
