package main

import (
	"bytes"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Yolean/y-cluster/pkg/inventory"
)

func runRoot(t *testing.T, args ...string) (string, error) {
	t.Helper()
	cmd := rootCmd()
	var out, errOut bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&errOut)
	cmd.SetArgs(args)
	err := cmd.Execute()
	return out.String(), err
}

// On a machine without a dockerhost, env prints nothing at all, so
// `eval "$(y-cluster dockerhost env)"` changes nothing for host Docker.
func TestDockerhostEnv_NoGuestPrintsNothing(t *testing.T) {
	t.Setenv("Y_CLUSTER_DOCKERHOST_DIR", filepath.Join(t.TempDir(), "none"))
	t.Setenv("DOCKER_HOST", "tcp://dockerd:2375")
	out, err := runRoot(t, "dockerhost", "env")
	if err != nil {
		t.Fatal(err)
	}
	if out != "" {
		t.Fatalf("want no output, got %q", out)
	}
}

func TestDockerhostStatus_NoGuest(t *testing.T) {
	t.Setenv("Y_CLUSTER_DOCKERHOST_DIR", filepath.Join(t.TempDir(), "none"))
	out, err := runRoot(t, "dockerhost", "status")
	if err == nil {
		t.Fatal("status must exit non-zero without a healthy guest")
	}
	if !strings.Contains(out, "dockerhost: none") {
		t.Fatalf("output %q", out)
	}
}

func TestDockerhostProvision_NoConfig(t *testing.T) {
	t.Setenv("Y_CLUSTER_DOCKERHOST_DIR", filepath.Join(t.TempDir(), "state"))
	_, err := runRoot(t, "dockerhost", "provision", "--config", filepath.Join(t.TempDir(), "missing.yaml"))
	if err == nil {
		t.Fatal("provision without a configuration must fail")
	}
	for _, want := range []string{"does not exist", "One-time root setup", "ifname:", "guestAddress:"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error lacks %q: %v", want, err)
		}
	}
}

func TestTeardownCmd_ListsDockerhost(t *testing.T) {
	t.Setenv("Y_CLUSTER_INVENTORY_DIR", t.TempDir())
	if err := inventory.Save(inventory.Record{
		Context: "y-cluster-dockerhost", Name: "dockerhost", Provider: "dockerhost",
		ConfigDir: "/home/u/.cache/y-cluster-dockerhost", Teardown: "y-cluster dockerhost teardown",
	}); err != nil {
		t.Fatal(err)
	}
	out, err := runRoot(t, "teardown")
	if err == nil {
		t.Fatal("bare teardown must exit non-zero")
	}
	if !strings.Contains(out, "  y-cluster dockerhost teardown\n") {
		t.Fatalf("listing must give the dockerhost's own teardown:\n%s", out)
	}
	if strings.Contains(out, "teardown -c /home/u/.cache/y-cluster-dockerhost") || strings.Contains(out, "config no longer") {
		t.Fatalf("the dockerhost has no provision config dir:\n%s", out)
	}
}
