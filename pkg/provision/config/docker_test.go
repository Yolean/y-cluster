package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Yolean/y-cluster/pkg/configfile"
)

func TestDocker_ApplyDefaults_Empty(t *testing.T) {
	c := &DockerConfig{CommonConfig: CommonConfig{Provider: ProviderDocker}}
	c.ApplyDefaults()
	if c.Name != "y-cluster" {
		t.Fatalf("Name: %q", c.Name)
	}
	if c.HostAPIPort() != "6443" {
		t.Fatalf("HostAPIPort: %q (default forwards should map host 6443 -> guest 6443)", c.HostAPIPort())
	}
	if len(c.PortForwards) != 3 {
		t.Fatalf("PortForwards: %v (expected 3 default entries)", c.PortForwards)
	}
	if c.Context != "local" {
		t.Fatalf("Context: %q", c.Context)
	}
	if c.Memory != "8192" {
		t.Fatalf("Memory: %q", c.Memory)
	}
	if c.CPUs != "4" {
		t.Fatalf("CPUs: %q", c.CPUs)
	}
	if c.K3s.Version == "" {
		t.Fatal("K3s.Version was not defaulted from pin file")
	}
	// Image is no longer modelled in the config; the docker
	// provisioner derives it from K3s.Version via ResolveImage.
	mirror := MirrorImage(c.K3s.Version)
	if !strings.Contains(mirror, "ghcr.io/yolean/k3s") {
		t.Fatalf("MirrorImage: %q (expected mirror)", mirror)
	}
	if strings.Contains(mirror, "+") {
		t.Fatalf("MirrorImage should use docker tag form (no '+'): %q", mirror)
	}
	upstream := UpstreamImage(c.K3s.Version)
	if !strings.Contains(upstream, "rancher/k3s") {
		t.Fatalf("UpstreamImage: %q (expected upstream)", upstream)
	}
}

func TestDocker_Validate_Provider(t *testing.T) {
	c := &DockerConfig{CommonConfig: CommonConfig{Provider: "qemu"}}
	c.ApplyDefaults()
	err := c.Validate()
	if err == nil || !strings.Contains(err.Error(), "docker") {
		t.Fatalf("want provider error, got %v", err)
	}
}

func TestDocker_Load_HappyPath(t *testing.T) {
	dir := t.TempDir()
	yaml := "provider: docker\nname: my-k3s\nportForwards:\n" +
		"- {host: '36443', guest: '6443'}\n" +
		"- {host: '8080', guest: '80'}\n"
	if err := os.WriteFile(filepath.Join(dir, "y-cluster-provision.yaml"),
		[]byte(yaml), 0o644); err != nil {
		t.Fatal(err)
	}
	var c DockerConfig
	if err := configfile.Load(dir, "y-cluster-provision.yaml", &c); err != nil {
		t.Fatal(err)
	}
	if c.Name != "my-k3s" {
		t.Fatalf("Name: %q", c.Name)
	}
	if c.HostAPIPort() != "36443" {
		t.Fatalf("HostAPIPort: %q", c.HostAPIPort())
	}
	if len(c.PortForwards) != 2 {
		t.Fatalf("PortForwards: %v (explicit list should be preserved, not merged with defaults)", c.PortForwards)
	}
}

// TestDocker_Validate_Requires6443 covers the regression: when a
// user spells out portForwards without a 6443 entry, validation
// must reject the config rather than silently producing a cluster
// kubectl can't reach.
func TestDocker_Validate_Requires6443(t *testing.T) {
	c := &DockerConfig{
		CommonConfig: CommonConfig{
			Provider:     ProviderDocker,
			PortForwards: []PortForward{{Host: "8080", Guest: "80"}},
		},
	}
	c.ApplyDefaults()
	err := c.Validate()
	if err == nil || !strings.Contains(err.Error(), "6443") {
		t.Fatalf("want 6443 validation error, got %v", err)
	}
}

func TestLoadProvision_K3sInDocker(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "y-cluster-provision.yaml"),
		[]byte("provider: docker\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	got, err := LoadProvision(dir)
	if err != nil {
		t.Fatal(err)
	}
	c, ok := got.(*DockerConfig)
	if !ok {
		t.Fatalf("type %T", got)
	}
	if c.Provider != ProviderDocker {
		t.Fatalf("Provider: %q", c.Provider)
	}
}
