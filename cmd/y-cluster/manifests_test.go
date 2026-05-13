package main

import (
	"strings"
	"testing"
)

// TestManifestNameRE_Valid pins the names that the validation
// regex accepts. Anything in this list MUST round-trip from
// `y-cluster manifests add <name> ...` to a file at
// /var/lib/y-cluster/manifests-staging/<name>.yaml on the cluster
// node. The shape of the regex deliberately matches what kubectl
// accepts as a resource name so the manifest's filename and
// metadata.name typically line up.
func TestManifestNameRE_Valid(t *testing.T) {
	cases := []string{
		"a",
		"abc",
		"migrate-v0.5.0-userdb",
		"migrate-v0.5.0-userdb-add-tenants",
		"01-bootstrap",
		"x.y.z",
		"a_b",
	}
	for _, in := range cases {
		if !manifestNameRE.MatchString(in) {
			t.Errorf("manifestNameRE rejected valid name %q", in)
		}
	}
}

// TestManifestNameRE_Invalid pins the rejection set. The regex is
// the only safety belt before we write to the cluster node's
// filesystem -- a slip here turns into a path-traversal that
// writes outside the staging dir.
func TestManifestNameRE_Invalid(t *testing.T) {
	cases := []string{
		"",                // empty
		".hidden",         // leading dot
		"-leading-dash",   // leading dash
		"path/with/slash", // path separator
		"..",              // path traversal
		"../escape",       // path traversal
		"name with space", // whitespace
		"name\twith\ttab", // whitespace
		"name;rm -rf /",   // shell metacharacter
		"name`whoami`",    // shell metacharacter
		"name$HOME",       // shell metacharacter
		"name*",           // glob
		"name?",           // glob
	}
	for _, in := range cases {
		if manifestNameRE.MatchString(in) {
			t.Errorf("manifestNameRE accepted invalid name %q", in)
		}
	}
}

// TestManifestsAddCmd_Wired smokes that the cobra subcommand
// graph rejects the obvious bad-input paths before any cluster
// I/O happens. Doesn't hit a real cluster -- the important shape
// here is that bad <name> trips the regex check, so a typo
// can't reach RunShell.
func TestManifestsAddCmd_RejectsInvalidName(t *testing.T) {
	cmd := manifestsAddCmd()
	cmd.SetArgs([]string{"../escape", "/dev/null"})
	cmd.SetOut(&strings.Builder{})
	cmd.SetErr(&strings.Builder{})
	err := cmd.Execute()
	if err == nil {
		t.Fatal("expected error for path-traversal name, got nil")
	}
	if !strings.Contains(err.Error(), "invalid manifest name") {
		t.Errorf("expected 'invalid manifest name' in error, got: %v", err)
	}
}

// TestManifestsReplaceCmd_RejectsInvalidName mirrors the add
// guard for the replace verb -- same regex, same fail-fast.
func TestManifestsReplaceCmd_RejectsInvalidName(t *testing.T) {
	cmd := manifestsReplaceCmd()
	cmd.SetArgs([]string{"../escape", "/dev/null"})
	cmd.SetOut(&strings.Builder{})
	cmd.SetErr(&strings.Builder{})
	err := cmd.Execute()
	if err == nil {
		t.Fatal("expected error for path-traversal name, got nil")
	}
	if !strings.Contains(err.Error(), "invalid manifest name") {
		t.Errorf("expected 'invalid manifest name' in error, got: %v", err)
	}
}

// TestManifestsRmCmd_RejectsInvalidName guards the rm path too --
// rm takes <name> only (no file input) and we want the same
// regex check to fire before any RunShell.
func TestManifestsRmCmd_RejectsInvalidName(t *testing.T) {
	cmd := manifestsRmCmd()
	cmd.SetArgs([]string{"../escape"})
	cmd.SetOut(&strings.Builder{})
	cmd.SetErr(&strings.Builder{})
	err := cmd.Execute()
	if err == nil {
		t.Fatal("expected error for path-traversal name, got nil")
	}
	if !strings.Contains(err.Error(), "invalid manifest name") {
		t.Errorf("expected 'invalid manifest name' in error, got: %v", err)
	}
}

// TestStagedManifestPath pins the on-cluster path the three
// verbs compute. The path is the single touchpoint between the
// build-side commands and the prepare-export step that moves
// these files onto the appliance disk; changing it accidentally
// would silently break the appliance flow.
func TestStagedManifestPath(t *testing.T) {
	got := stagedManifestPath("migrate-v0.5.0-userdb")
	want := "/var/lib/y-cluster/manifests-staging/migrate-v0.5.0-userdb.yaml"
	if got != want {
		t.Errorf("stagedManifestPath: got %q, want %q", got, want)
	}
}

// TestManifestsCmd_Subcommands pins the three-verb surface so a
// future refactor can't accidentally drop one. The strict-in-
// both-directions contract (add: must-not-exist, replace: must-
// exist, rm: must-exist) only works if all three exist; deleting
// any of them turns either a "create" or an "overwrite" into a
// silent --force.
func TestManifestsCmd_Subcommands(t *testing.T) {
	cmd := manifestsCmd()
	have := map[string]bool{}
	for _, sub := range cmd.Commands() {
		have[sub.Name()] = true
	}
	for _, want := range []string{"add", "replace", "rm"} {
		if !have[want] {
			t.Errorf("manifests subcommand missing: %q (have %v)", want, have)
		}
	}
}
