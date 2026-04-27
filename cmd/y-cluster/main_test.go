package main

import (
	"os"
	"path/filepath"
	"runtime/debug"
	"strings"
	"testing"

	"github.com/Yolean/y-cluster/pkg/provision/config"
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

// TestRootCmd_Version locks in that --version still names the
// release tag. The git-suffix piece relies on debug.BuildInfo
// having vcs.* settings, which `go test` doesn't always stamp;
// formatVersion's unit tests below cover the suffix logic with
// synthetic settings.
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

func TestFormatVersion_NoVCSInfo(t *testing.T) {
	got := formatVersion("v0.4.0", nil)
	if got != "v0.4.0" {
		t.Fatalf("got %q, want bare release without VCS info", got)
	}
}

func TestFormatVersion_TaggedClean(t *testing.T) {
	got := formatVersion("v0.4.0", []debug.BuildSetting{
		{Key: "vcs.revision", Value: "abcdef1234567890"},
		{Key: "vcs.modified", Value: "false"},
	})
	if got != "v0.4.0 (abcdef1)" {
		t.Fatalf("got %q", got)
	}
}

func TestFormatVersion_TaggedDirty(t *testing.T) {
	got := formatVersion("v0.4.0", []debug.BuildSetting{
		{Key: "vcs.revision", Value: "abcdef1234567890"},
		{Key: "vcs.modified", Value: "true"},
	})
	if got != "v0.4.0 (abcdef1-dirty)" {
		t.Fatalf("got %q", got)
	}
}

func TestFormatVersion_DevDirty(t *testing.T) {
	got := formatVersion("dev", []debug.BuildSetting{
		{Key: "vcs.revision", Value: "abcdef1234567890"},
		{Key: "vcs.modified", Value: "true"},
	})
	if got != "dev (abcdef1-dirty)" {
		t.Fatalf("got %q", got)
	}
}

// TestFormatVersion_ShortRevisionPreserved -- if the toolchain
// ever gives us a revision shorter than 7 chars (unlikely for
// git, but the contract is "first N or full"), don't slice past
// the end.
func TestFormatVersion_ShortRevisionPreserved(t *testing.T) {
	got := formatVersion("dev", []debug.BuildSetting{
		{Key: "vcs.revision", Value: "abcd"},
		{Key: "vcs.modified", Value: "false"},
	})
	if got != "dev (abcd)" {
		t.Fatalf("got %q", got)
	}
}

// TestProvisionCmd_RequiresConfig confirms the hard cut from flags:
// `y-cluster provision` without -c errors out before touching qemu
// state. Same constraint on teardown/export/import.
func TestProvisionCmd_RequiresConfig(t *testing.T) {
	for _, name := range []string{"provision", "teardown", "export", "import"} {
		t.Run(name, func(t *testing.T) {
			cmd := rootCmd()
			args := []string{name}
			if name == "export" || name == "import" {
				args = append(args, "out.vmdk")
			}
			cmd.SetArgs(args)
			var buf strings.Builder
			cmd.SetOut(&buf)
			cmd.SetErr(&buf)
			err := cmd.Execute()
			if err == nil || !strings.Contains(err.Error(), "required flag") {
				t.Fatalf("%s: want required-flag error, got %v", name, err)
			}
		})
	}
}

// TestProvisionCmd_UnknownProviderError exercises the dispatch loader
// surfacing a useful error to the operator, not a panic or silent
// fallback.
func TestProvisionCmd_UnknownProviderError(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "y-cluster-provision.yaml"), "provider: martian\n")

	cmd := rootCmd()
	cmd.SetArgs([]string{"provision", "-c", dir})
	var buf strings.Builder
	cmd.SetOut(&buf)
	cmd.SetErr(&buf)
	err := cmd.Execute()
	if err == nil || !strings.Contains(err.Error(), "unknown provider") {
		t.Fatalf("want unknown-provider error, got %v", err)
	}
}

// TestProvisionCmd_MissingProviderError covers the empty-discovery
// path: when the YAML omits provider: and DiscoverProvider also
// returns nothing, the CLI surfaces an actionable error pointing
// at the discovery probes. We override DiscoverProviderFn so the
// test outcome doesn't depend on whether the test host has KVM
// or a docker daemon.
func TestProvisionCmd_MissingProviderError(t *testing.T) {
	prev := config.DiscoverProviderFn
	config.DiscoverProviderFn = func() string { return "" }
	t.Cleanup(func() { config.DiscoverProviderFn = prev })

	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "y-cluster-provision.yaml"), "name: foo\n")

	cmd := rootCmd()
	cmd.SetArgs([]string{"provision", "-c", dir})
	var buf strings.Builder
	cmd.SetOut(&buf)
	cmd.SetErr(&buf)
	err := cmd.Execute()
	if err == nil {
		t.Fatal("want error when discovery returns empty")
	}
	for _, want := range []string{"discovery", "qemu", "docker"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("error should mention %q, got %v", want, err)
		}
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
