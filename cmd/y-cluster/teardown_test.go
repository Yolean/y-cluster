package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Yolean/y-cluster/pkg/inventory"
)

// TestTeardownCmd_NoConfigListsCandidates: `teardown` without -c
// prints every recorded cluster as a ready-to-paste command, then
// errors so a script that forgot -c can't read the listing as a
// successful teardown.
func TestTeardownCmd_NoConfigListsCandidates(t *testing.T) {
	t.Setenv("Y_CLUSTER_INVENTORY_DIR", t.TempDir())
	// One record whose config dir still holds a provision yaml,
	// one whose dir is gone -- the listing marks the latter.
	liveDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(liveDir, "y-cluster-provision.yaml"), []byte("provider: docker\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	for _, rec := range []inventory.Record{
		{Context: "node-agent", Name: "node-agent", Provider: "docker", ConfigDir: liveDir, HostPorts: []string{"26443"}},
		{Context: "local", Name: "y-cluster", Provider: "qemu", ConfigDir: "/gone/cluster/dir", HostPorts: []string{"6443"}},
	} {
		if err := inventory.Save(rec); err != nil {
			t.Fatal(err)
		}
	}

	cmd := rootCmd()
	var out, errOut bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&errOut)
	cmd.SetArgs([]string{"teardown"})
	err := cmd.Execute()
	if err == nil {
		t.Fatal("bare teardown must exit non-zero")
	}
	if !strings.Contains(err.Error(), "--config") {
		t.Fatalf("error should point at --config: %v", err)
	}
	got := out.String()
	for _, want := range []string{
		"teardown -c " + liveDir,
		"teardown -c /gone/cluster/dir",
		`context "node-agent"`,
		"config no longer at this path",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("listing should contain %q; got:\n%s", want, got)
		}
	}
	if strings.Contains(got, liveDir+"\n      #") && strings.Contains(got, liveDir+" -- config no longer") {
		t.Fatalf("live config dir must not be marked stale:\n%s", got)
	}
}

// TestTeardownCmd_NoConfigEmptyInventory: with nothing recorded,
// bare teardown keeps the old "--config is required" contract.
func TestTeardownCmd_NoConfigEmptyInventory(t *testing.T) {
	t.Setenv("Y_CLUSTER_INVENTORY_DIR", t.TempDir())
	cmd := rootCmd()
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&out)
	cmd.SetArgs([]string{"teardown"})
	err := cmd.Execute()
	if err == nil {
		t.Fatal("bare teardown must exit non-zero")
	}
	if !strings.Contains(err.Error(), "--config (-c) is required") {
		t.Fatalf("expected the required-flag error, got: %v", err)
	}
	if strings.Contains(out.String(), "teardown -c") {
		t.Fatalf("no candidate listing expected when inventory is empty:\n%s", out.String())
	}
}
