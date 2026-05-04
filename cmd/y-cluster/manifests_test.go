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
