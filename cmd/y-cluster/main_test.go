package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestYconvergeCmd_PrintDeps(t *testing.T) {
	root := t.TempDir()
	writeFile(t, filepath.Join(root, "cue.mod/module.cue"), `module: "yolean.se/ystack"`)
	writeFile(t, filepath.Join(root, "base/kustomization.yaml"), "")
	writeFile(t, filepath.Join(root, "base/yconverge.cue"), `
package base
step: checks: []
`)
	writeFile(t, filepath.Join(root, "target/kustomization.yaml"), "")
	writeFile(t, filepath.Join(root, "target/yconverge.cue"), `
package target
import "yolean.se/ystack/base:base"
_dep: base.step
step: checks: []
`)

	cmd := rootCmd()
	var out strings.Builder
	cmd.SetOut(&out)
	cmd.SetArgs([]string{
		"yconverge",
		"--context=test",
		"-k", filepath.Join(root, "target"),
		"--print-deps",
	})
	if err := cmd.Execute(); err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(out.String()), "\n")
	if len(lines) != 2 {
		t.Fatalf("expected 2 lines, got %d: %v", len(lines), lines)
	}
	if !strings.HasSuffix(lines[0], "/base") {
		t.Fatalf("expected base first, got %s", lines[0])
	}
	if !strings.HasSuffix(lines[1], "/target") {
		t.Fatalf("expected target last, got %s", lines[1])
	}
}

func TestYconvergeCmd_MissingContext(t *testing.T) {
	cmd := rootCmd()
	cmd.SetArgs([]string{"yconverge", "-k", "/tmp"})
	err := cmd.Execute()
	if err == nil {
		t.Fatal("expected error for missing --context")
	}
}

func TestYconvergeCmd_MissingK(t *testing.T) {
	cmd := rootCmd()
	cmd.SetArgs([]string{"yconverge", "--context=test"})
	err := cmd.Execute()
	if err == nil {
		t.Fatal("expected error for missing -k")
	}
}

func TestRootCmd_Version(t *testing.T) {
	cmd := rootCmd()
	var out strings.Builder
	cmd.SetOut(&out)
	cmd.SetArgs([]string{"--version"})
	if err := cmd.Execute(); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "dev") {
		t.Fatalf("expected version 'dev', got %s", out.String())
	}
}

func TestBinaryName_Default(t *testing.T) {
	// Save and restore os.Args[0]
	orig := os.Args[0]
	defer func() { os.Args[0] = orig }()

	os.Args[0] = "/usr/local/bin/y-cluster"
	if got := binaryName(); got != "y-cluster" {
		t.Fatalf("expected y-cluster, got %s", got)
	}

	os.Args[0] = "/usr/local/bin/kubectl-yconverge"
	if got := binaryName(); got != "kubectl-yconverge" {
		t.Fatalf("expected kubectl-yconverge, got %s", got)
	}
}
