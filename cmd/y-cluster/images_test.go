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
