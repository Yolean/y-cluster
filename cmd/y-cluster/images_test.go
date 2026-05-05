package main

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const podWithInitYAML = `apiVersion: v1
kind: Pod
metadata:
  name: p
spec:
  containers:
  - name: c
    image: nginx:1.27
  initContainers:
  - name: i
    image: busybox:1.36
`

func TestImagesListCmd_File(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "pod.yaml")
	writeFile(t, path, podWithInitYAML)

	cmd := rootCmd()
	var out strings.Builder
	cmd.SetOut(&out)
	cmd.SetArgs([]string{"images", "list", path})
	if err := cmd.Execute(); err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(out.String()), "\n")
	if len(lines) != 2 || lines[0] != "busybox:1.36" || lines[1] != "nginx:1.27" {
		t.Fatalf("unexpected output: %q", out.String())
	}
}

func TestImagesListCmd_Stdin(t *testing.T) {
	cmd := rootCmd()
	var out strings.Builder
	cmd.SetOut(&out)
	cmd.SetIn(strings.NewReader(podWithInitYAML))
	cmd.SetArgs([]string{"images", "list", "-"})
	if err := cmd.Execute(); err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(out.String()), "\n")
	if len(lines) != 2 {
		t.Fatalf("got %d lines: %q", len(lines), out.String())
	}
}

func TestImagesListCmd_MissingArg(t *testing.T) {
	cmd := rootCmd()
	cmd.SetArgs([]string{"images", "list"})
	if err := cmd.Execute(); err == nil {
		t.Fatal("expected error for missing positional arg")
	}
}

func TestImagesListCmd_FileNotFound(t *testing.T) {
	cmd := rootCmd()
	cmd.SetArgs([]string{"images", "list", "/nonexistent/file.yaml"})
	if err := cmd.Execute(); err == nil {
		t.Fatal("expected error for missing file")
	}
}

// TestImagesListCmd_PositionalAndContextMutex pins the mutex
// rule: a positional input and --context can't both be set,
// because they pick incompatible input sources (YAML stream vs
// containerd ground truth).
func TestImagesListCmd_PositionalAndContextMutex(t *testing.T) {
	cmd := rootCmd()
	cmd.SetArgs([]string{"images", "list", "--context=local", "/some/path.yaml"})
	err := cmd.Execute()
	if err == nil {
		t.Fatal("expected error for positional + --context combination")
	}
	if !strings.Contains(err.Error(), "mutually exclusive") {
		t.Errorf("error should mention mutex: %v", err)
	}
}

// TestImagesListCmd_ContextUnknownPropagates: a --context that
// the kubeconfig doesn't know about should surface the cluster
// lookup error rather than swallowing it.
func TestImagesListCmd_ContextUnknownPropagates(t *testing.T) {
	cmd := rootCmd()
	cmd.SetArgs([]string{"images", "list", "--context=does-not-exist"})
	if err := cmd.Execute(); err == nil {
		t.Fatal("expected cluster-lookup error for unknown --context")
	}
}

// TestImagesListCmd_BadFormat / _BadSort pin the validation
// of the cluster-mode formatting knobs. Errors should fire on
// the flag value, NOT on the unreachable cluster -- but a
// non-existent context happens to error first; we assert that
// the flag values themselves are at least accepted without a
// flag-parse error (cobra would error before our RunE runs).
func TestImagesListCmd_FlagsAccepted(t *testing.T) {
	for _, args := range [][]string{
		{"images", "list", "--context=does-not-exist", "--format=table"},
		{"images", "list", "--context=does-not-exist", "--format=json"},
		{"images", "list", "--context=does-not-exist", "--sort=size"},
		{"images", "list", "--context=does-not-exist", "--sort=name"},
	} {
		cmd := rootCmd()
		cmd.SetArgs(args)
		// We expect a cluster-lookup error, not a flag-parse error.
		err := cmd.Execute()
		if err == nil {
			t.Errorf("%v: expected cluster-lookup error", args)
			continue
		}
		if strings.Contains(err.Error(), "unknown flag") {
			t.Errorf("%v: cobra rejected a flag we own: %v", args, err)
		}
	}
}

func TestImagesCacheCmd_RequiresRef(t *testing.T) {
	cmd := rootCmd()
	cmd.SetArgs([]string{"images", "cache"})
	if err := cmd.Execute(); err == nil {
		t.Fatal("expected error for missing ref")
	}
}

func TestImagesCacheCmd_ParseError(t *testing.T) {
	// Setting a sentinel cache dir keeps the test isolated from
	// the dev's real ~/.cache/y-cluster.
	t.Setenv("Y_CLUSTER_CACHE_DIR", t.TempDir())
	cmd := rootCmd()
	cmd.SetArgs([]string{"images", "cache", "::not a ref::"})
	if err := cmd.Execute(); err == nil {
		t.Fatal("expected parse error for malformed ref")
	}
}

func TestImagesLoadCmd_RequiresArchive(t *testing.T) {
	cmd := rootCmd()
	cmd.SetArgs([]string{"images", "load"})
	if err := cmd.Execute(); err == nil {
		t.Fatal("expected error for missing archive argument")
	}
}

func TestImagesLoadCmd_FileNotFound(t *testing.T) {
	cmd := rootCmd()
	cmd.SetArgs([]string{"images", "load", "--context=does-not-exist", "/nonexistent/archive.tar"})
	err := cmd.Execute()
	if err == nil {
		t.Fatal("expected error for missing archive file")
	}
}

// TestIsPathArg pins the prefix-driven dispatch rule -- the
// single source of truth for "is this a path or a remote ref?"
// the load cmd reads. Reference shape is documented in the
// load subcommand's --help, which the cases below mirror.
func TestIsPathArg(t *testing.T) {
	paths := []string{
		"./relative/path",
		"./relative/path.tar",
		"./", ".", "..",
		"/absolute/path",
		"/", "/tmp",
		"~/home/path",
	}
	refs := []string{
		"nginx",
		"nginx:1.27",
		"nginx@sha256:1111111111111111111111111111111111111111111111111111111111111111",
		"docker.io/library/nginx:1.27",
		"registry.k8s.io/pause:3.10",
		"localhost:5000/yolean/echo:v1",
		"builds-registry.default.svc.cluster.local/myrepo/myapp:local-dev",
	}
	for _, p := range paths {
		if !isPathArg(p) {
			t.Errorf("expected %q to dispatch as path", p)
		}
	}
	for _, r := range refs {
		if isPathArg(r) {
			t.Errorf("expected %q to dispatch as remote ref", r)
		}
	}
}

// TestImagesLoadCmd_CacheFalseRejectedForStdin: --cache=false
// is meaningful only for remote refs (where the alternative is
// "pull into a tempdir"). For stdin, the caller's already
// holding the bytes; --cache=false has no semantic and should
// fail loudly.
func TestImagesLoadCmd_CacheFalseRejectedForStdin(t *testing.T) {
	cmd := rootCmd()
	cmd.SetIn(strings.NewReader("noise"))
	cmd.SetArgs([]string{"images", "load", "--context=does-not-exist", "--cache=false", "-"})
	err := cmd.Execute()
	if err == nil {
		t.Fatal("expected error for --cache=false with stdin")
	}
	if !strings.Contains(err.Error(), "--cache=false") {
		t.Errorf("error should call out --cache=false: %v", err)
	}
}

// TestImagesLoadCmd_CacheFalseRejectedForPath: same shape for
// the path-input case -- caller owns the bytes; toggling the
// cache is meaningless.
func TestImagesLoadCmd_CacheFalseRejectedForPath(t *testing.T) {
	cmd := rootCmd()
	cmd.SetArgs([]string{"images", "load", "--context=does-not-exist", "--cache=false", "./some/path"})
	err := cmd.Execute()
	if err == nil {
		t.Fatal("expected error for --cache=false with path")
	}
	if !strings.Contains(err.Error(), "--cache=false") {
		t.Errorf("error should call out --cache=false: %v", err)
	}
}

// TestImagesLoadCmd_CacheFalseRejectedForURL: same shape for
// the url-input case -- the response body is streamed straight
// to the cluster; the cache is never involved, so toggling it
// is meaningless.
func TestImagesLoadCmd_CacheFalseRejectedForURL(t *testing.T) {
	cmd := rootCmd()
	cmd.SetArgs([]string{"images", "load", "--context=does-not-exist", "--cache=false", "https://example.invalid/some.tar"})
	err := cmd.Execute()
	if err == nil {
		t.Fatal("expected error for --cache=false with url")
	}
	if !strings.Contains(err.Error(), "--cache=false") {
		t.Errorf("error should call out --cache=false: %v", err)
	}
}

// TestIsURLArg pins the scheme-driven dispatch rule the load cmd
// consults before the remote-ref default: only http(s):// input
// streams over HTTP; refs and paths never carry a scheme.
func TestIsURLArg(t *testing.T) {
	urls := []string{
		"http://hel1.your-objectstorage.com/bucket/myapp.tar",
		"https://hel1.your-objectstorage.com/bucket/myapp.tar",
	}
	other := []string{
		"nginx:1.27",
		"registry.k8s.io/pause:3.10",
		"./relative/path.tar",
		"/absolute/path.tar",
		"-",
		"httpd:2.4",
	}
	for _, u := range urls {
		if !isURLArg(u) {
			t.Errorf("expected %q to dispatch as url", u)
		}
	}
	for _, o := range other {
		if isURLArg(o) {
			t.Errorf("expected %q NOT to dispatch as url", o)
		}
	}
}

// TestOpenInput_URL_Success exercises the http(s):// branch:
// returns the body verbatim, closer drains the response. Phase 4
// adds this path so a Hetzner S3 blob URL can be fed to images
// load / list / manifests add without an intermediate `curl ... |
// y-cluster ... -` pipe.
func TestOpenInput_URL_Success(t *testing.T) {
	const want = "fake oci archive bytes"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/octet-stream")
		_, _ = io.WriteString(w, want)
	}))
	defer srv.Close()

	r, closer, err := openInput(context.Background(), srv.URL+"/foo.tar", strings.NewReader(""))
	if err != nil {
		t.Fatalf("openInput: %v", err)
	}
	defer closer()
	got, err := io.ReadAll(r)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	if string(got) != want {
		t.Errorf("body: got %q, want %q", got, want)
	}
}

// TestOpenInput_URL_Non2xx surfaces server errors as a clean Go
// error rather than letting them slip through as the response
// body. Without this, `images load https://wherever/missing.tar`
// would feed a 404 HTML page to ctr image import.
func TestOpenInput_URL_Non2xx(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "no such object", http.StatusNotFound)
	}))
	defer srv.Close()

	_, _, err := openInput(context.Background(), srv.URL+"/missing.tar", strings.NewReader(""))
	if err == nil {
		t.Fatal("expected error for 404 response")
	}
	if !strings.Contains(err.Error(), "404") {
		t.Errorf("error should mention status: %v", err)
	}
}

// TestOpenInput_URL_DialFailure: an unreachable host surfaces
// an error rather than hanging or silently degrading. Uses a
// short ctx deadline because the default TCP connect timeout is
// long; the unit-test signal we need ("dial failure -> error") is
// independent of the wait.
func TestOpenInput_URL_DialFailure(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()
	// Reserved TEST-NET-1 (RFC 5737); guaranteed unroutable.
	_, _, err := openInput(ctx, "http://192.0.2.1:1/foo.tar", strings.NewReader(""))
	if err == nil {
		t.Fatal("expected error for unroutable host")
	}
}

// TestOpenInput_FileBranch confirms the file path is unchanged
// from the pre-phase-4 behaviour.
func TestOpenInput_FileBranch(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "data.bin")
	writeFile(t, path, "hello")

	r, closer, err := openInput(context.Background(), path, strings.NewReader("stdin should not be used"))
	if err != nil {
		t.Fatalf("openInput: %v", err)
	}
	defer closer()
	got, err := io.ReadAll(r)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if string(got) != "hello" {
		t.Errorf("got %q, want %q", got, "hello")
	}
}

// TestOpenInput_StdinBranch confirms "-" routes to the supplied
// stdin reader without consulting URL or file logic.
func TestOpenInput_StdinBranch(t *testing.T) {
	r, closer, err := openInput(context.Background(), "-", strings.NewReader("piped"))
	if err != nil {
		t.Fatalf("openInput: %v", err)
	}
	defer closer()
	got, err := io.ReadAll(r)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if string(got) != "piped" {
		t.Errorf("got %q, want %q", got, "piped")
	}
}
