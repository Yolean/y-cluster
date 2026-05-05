package hetzner

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestState_RoundTrip pins the sidecar JSON shape Teardown reads
// back. Specifically: ServerID is the authoritative identifier
// (Hetzner Cloud IDs are int64 and don't shadow on rename), and
// SSHKeyName is captured so a future operator-driven rename
// doesn't strand the resource.
func TestState_RoundTrip(t *testing.T) {
	dir := t.TempDir()
	want := state{
		Context:    "alice-dev",
		ServerID:   12345678,
		ServerName: "alice-dev",
		IPv4:       "203.0.113.1",
		SSHUser:    "ystack",
		SSHKeyName: "alice-dev",
	}
	if err := saveState(dir, want); err != nil {
		t.Fatal(err)
	}
	got, err := loadState(dir, "alice-dev")
	if err != nil {
		t.Fatal(err)
	}
	if got != want {
		t.Errorf("round-trip mismatch:\n got: %+v\nwant: %+v", got, want)
	}

	// File written at the canonical path.
	if _, err := os.Stat(filepath.Join(dir, "alice-dev.json")); err != nil {
		t.Errorf("state file at canonical path: %v", err)
	}
}

// TestState_DeleteIdempotent: removing a missing sidecar is a
// no-op (we want Teardown to not error on a half-finished previous
// teardown).
func TestState_DeleteIdempotent(t *testing.T) {
	dir := t.TempDir()
	if err := deleteState(dir, "missing"); err != nil {
		t.Fatalf("delete missing state should be no-op: %v", err)
	}
}

// TestRenderCloudInitUserData_Shape pins the user_data Hetzner
// receives. Two pieces matter:
//
//  1. The unprivileged user (ystack) gets the operator's pubkey;
//     anything else and Provision's waitForSSH loops forever.
//  2. The datasource_list pin lands as a write_files entry under
//     /etc/cloud/cloud.cfg.d/. A re-imaged or snapshot-restored
//     server inherits the pin and doesn't stall on EC2 IMDS /
//     GCE metadata probes -- same fix as the qemu provisioner's
//     renderCloudInitUserData.
func TestRenderCloudInitUserData_Shape(t *testing.T) {
	body := renderCloudInitUserData("alice-dev", "ystack", "ssh-ed25519 AAAA test@host\n")
	for _, want := range []string{
		"hostname: alice-dev",
		"name: ystack",
		"sudo: ALL=(ALL) NOPASSWD:ALL",
		"ssh-ed25519 AAAA test@host",
		"/etc/cloud/cloud.cfg.d/99-y-cluster-pin.cfg",
		"datasource_list: [NoCloud, None]",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("user-data missing %q:\n%s", want, body)
		}
	}
	// Trailing newline on the pubkey must be stripped (would
	// otherwise produce a malformed YAML list item).
	if strings.Contains(body, "test@host\n      - ") {
		t.Errorf("trailing newline on ssh key not trimmed:\n%s", body)
	}
}

// TestLabelSelectorForGroup pins the label vocabulary that phase 3
// will use to enumerate LB-group members. Defined in phase 1 so
// every Provision tags the server consistently.
func TestLabelSelectorForGroup(t *testing.T) {
	got := labelSelectorForGroup("alice")
	want := "managed-by=y-cluster,lb-group=alice"
	if got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

// TestCacheDir_DefaultUnderHome documents the resolution order
// without requiring the user-home-dir actually exist (we only
// confirm the suffix). Tests that need an isolated cache use
// CacheDirEnv.
func TestCacheDir_DefaultUnderHome(t *testing.T) {
	t.Setenv(CacheDirEnv, "")
	got := CacheDir()
	if !strings.HasSuffix(got, "/y-cluster-hetzner") {
		t.Errorf("CacheDir should end in /y-cluster-hetzner; got %q", got)
	}
}

// TestCacheDir_EnvOverride: tests + multi-tenant CI use the env
// var to keep state out of $HOME.
func TestCacheDir_EnvOverride(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv(CacheDirEnv, tmp)
	if got := CacheDir(); got != tmp {
		t.Errorf("CacheDir env override: got %q, want %q", got, tmp)
	}
}
