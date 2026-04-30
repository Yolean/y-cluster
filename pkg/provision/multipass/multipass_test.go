package multipass

import (
	"testing"

	"github.com/Yolean/y-cluster/pkg/provision/config"
)

func defaultedRuntimeConfig(t *testing.T) Config {
	t.Helper()
	c := &config.MultipassConfig{CommonConfig: config.CommonConfig{Provider: config.ProviderMultipass}}
	c.ApplyDefaults()
	return FromConfig(c)
}

func TestFromConfig_AppliesDefaults(t *testing.T) {
	cfg := defaultedRuntimeConfig(t)
	if cfg.Name != "y-cluster" {
		t.Fatalf("Name: %q", cfg.Name)
	}
	if cfg.Image != "24.04" {
		t.Fatalf("Image: %q", cfg.Image)
	}
	if cfg.Memory != "8192" {
		t.Fatalf("Memory: %q", cfg.Memory)
	}
	if cfg.CPUs != "4" {
		t.Fatalf("CPUs: %q", cfg.CPUs)
	}
	if cfg.Context != "local" {
		t.Fatalf("Context: %q", cfg.Context)
	}
	if cfg.K3s.Version == "" {
		t.Fatal("K3s.Version was not defaulted from pin file")
	}
}

func TestVMIP_EmptyBeforeProvision(t *testing.T) {
	c := &Cluster{}
	if got := c.VMIP(); got != "" {
		t.Fatalf("VMIP before provision: %q", got)
	}
}

func TestContext_PassesThroughCfg(t *testing.T) {
	c := &Cluster{cfg: Config{Context: "ctx-x"}}
	if got := c.Context(); got != "ctx-x" {
		t.Fatalf("Context: %q", got)
	}
}
