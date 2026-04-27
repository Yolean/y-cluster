package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestCacheInfo_PathFlag(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("Y_CLUSTER_CACHE_DIR", dir)
	cmd := rootCmd()
	var out strings.Builder
	cmd.SetOut(&out)
	cmd.SetArgs([]string{"cache", "info", "-p"})
	if err := cmd.Execute(); err != nil {
		t.Fatal(err)
	}
	if got := strings.TrimSpace(out.String()); got != dir {
		t.Fatalf("got %q want %q", got, dir)
	}
}

func TestCacheInfo_DefaultPrintsBothSubtrees(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("Y_CLUSTER_CACHE_DIR", dir)
	// Drop a known-size file under images/ so size logic is exercised.
	imgs := filepath.Join(dir, "images")
	if err := os.MkdirAll(imgs, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(imgs, "blob"), []byte("12345"), 0o644); err != nil {
		t.Fatal(err)
	}

	cmd := rootCmd()
	var out strings.Builder
	cmd.SetOut(&out)
	cmd.SetArgs([]string{"cache", "info"})
	if err := cmd.Execute(); err != nil {
		t.Fatal(err)
	}
	body := out.String()
	if !strings.Contains(body, "root:   "+dir) {
		t.Fatalf("body missing root: %q", body)
	}
	if !strings.Contains(body, "images: 5 B") {
		t.Fatalf("body missing images bytes: %q", body)
	}
	if !strings.Contains(body, "k3s:    0 B") {
		t.Fatalf("body missing k3s bytes: %q", body)
	}
}

func TestCachePurge_BareErrors(t *testing.T) {
	t.Setenv("Y_CLUSTER_CACHE_DIR", t.TempDir())
	cmd := rootCmd()
	var out strings.Builder
	cmd.SetOut(&out)
	cmd.SetErr(&out)
	cmd.SetArgs([]string{"cache", "purge"})
	err := cmd.Execute()
	if err == nil {
		t.Fatal("bare cache purge should error")
	}
	if !strings.Contains(err.Error(), "--images") {
		t.Fatalf("error should mention --images: %v", err)
	}
}

func TestCachePurge_Images(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("Y_CLUSTER_CACHE_DIR", dir)
	imgs := filepath.Join(dir, "images")
	k3s := filepath.Join(dir, "k3s")
	for _, p := range []string{imgs, k3s} {
		if err := os.MkdirAll(p, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(p, "blob"), []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	cmd := rootCmd()
	var out strings.Builder
	cmd.SetOut(&out)
	cmd.SetArgs([]string{"cache", "purge", "--images"})
	if err := cmd.Execute(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(imgs); !os.IsNotExist(err) {
		t.Fatalf("images dir still present: %v", err)
	}
	if _, err := os.Stat(k3s); err != nil {
		t.Fatalf("k3s dir should remain: %v", err)
	}
	if !strings.Contains(out.String(), "removed "+imgs) {
		t.Fatalf("output missing removal log: %q", out.String())
	}
}

func TestCachePurge_All_Idempotent(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("Y_CLUSTER_CACHE_DIR", dir)
	if err := os.MkdirAll(filepath.Join(dir, "images"), 0o755); err != nil {
		t.Fatal(err)
	}
	// Don't pre-create k3s so we exercise the "skip (not present)" path.

	cmd := rootCmd()
	var out strings.Builder
	cmd.SetOut(&out)
	cmd.SetArgs([]string{"cache", "purge", "--all"})
	if err := cmd.Execute(); err != nil {
		t.Fatal(err)
	}
	body := out.String()
	if !strings.Contains(body, "removed "+filepath.Join(dir, "images")) {
		t.Fatalf("missing removed images line: %q", body)
	}
	if !strings.Contains(body, "skip "+filepath.Join(dir, "k3s")) {
		t.Fatalf("missing skip k3s line: %q", body)
	}

	// Second invocation: both should be skipped, no error.
	cmd2 := rootCmd()
	var out2 strings.Builder
	cmd2.SetOut(&out2)
	cmd2.SetArgs([]string{"cache", "purge", "--all"})
	if err := cmd2.Execute(); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out2.String(), "skip "+filepath.Join(dir, "images")) {
		t.Fatalf("second run should skip images: %q", out2.String())
	}
}
