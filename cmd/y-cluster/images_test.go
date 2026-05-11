package main

import (
	"path/filepath"
	"strings"
	"testing"
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
