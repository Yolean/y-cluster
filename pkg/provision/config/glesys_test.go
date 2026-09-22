package config

import (
	"strings"
	"testing"
)

// glesysMinimal is the least an operator can write: a provider and a
// context. Everything else has to come from ApplyDefaults, so these
// tests double as the record of what a bare config expands into.
func glesysMinimal() *GlesysConfig {
	c := &GlesysConfig{}
	c.Provider = ProviderGlesys
	c.Context = "hosting-test"
	return c
}

// TestGlesys_Defaults pins what a bare config expands into. Sizing
// is CommonConfig's regular single-site machine; where the server
// runs and what it boots are not details either: a default that
// drifts moves new clusters to another country or another Talos
// release without anyone having asked.
func TestGlesys_Defaults(t *testing.T) {
	c := glesysMinimal()
	c.ApplyDefaults()

	if c.Memory != "8192" {
		t.Errorf("memory: got %q, want 8192 (the regular single-site machine)", c.Memory)
	}
	if c.CPUs != "4" {
		t.Errorf("cpus: got %q, want 4", c.CPUs)
	}
	if c.ServerDisk != "30G" {
		t.Errorf("serverDisk: got %q, want 30G", c.ServerDisk)
	}
	if c.Platform != "KVM" {
		t.Errorf("platform: got %q, want KVM", c.Platform)
	}
	if c.DataCenter != "Falkenberg" {
		t.Errorf("dataCenter: got %q, want Falkenberg", c.DataCenter)
	}
	if c.Template != "Talos 1.14" {
		t.Errorf("template: got %q, want Talos 1.14", c.Template)
	}
	if err := c.Validate(); err != nil {
		t.Errorf("the defaults must validate: %v", err)
	}
}

// TestGlesys_ExplicitSizingSurvivesDefaults: an operator who asks for
// another machine keeps it.
func TestGlesys_ExplicitSizingSurvivesDefaults(t *testing.T) {
	c := glesysMinimal()
	c.Memory = "4096"
	c.CPUs = "2"
	c.ServerDisk = "80G"
	c.Template = "Talos 1.15"
	c.ApplyDefaults()

	if c.Memory != "4096" || c.CPUs != "2" || c.ServerDisk != "80G" || c.Template != "Talos 1.15" {
		t.Fatalf("explicit values were overwritten: memory=%q cpus=%q serverDisk=%q template=%q", c.Memory, c.CPUs, c.ServerDisk, c.Template)
	}
}

// TestGlesys_PlatformMustBeKVM: KVM is the only platform that takes a
// cloudconfig, and cloudconfig is how the machine config reaches the
// node. Failing at config load beats an unexplained API timeout ten
// minutes later.
func TestGlesys_PlatformMustBeKVM(t *testing.T) {
	c := glesysMinimal()
	c.ApplyDefaults()
	c.Platform = "VMware"
	err := c.Validate()
	if err == nil {
		t.Fatal("non-KVM platform should be rejected")
	}
	if !strings.Contains(err.Error(), "cloudconfig") {
		t.Fatalf("error should explain why KVM is required; got %v", err)
	}
}

// TestGlesys_SizingMustBeNumeric: Memory and CPUs are strings for the
// local providers' sake but reach the GleSYS API as integers.
func TestGlesys_SizingMustBeNumeric(t *testing.T) {
	for _, tc := range []struct{ name, mem, cpus, disk string }{
		{"memory not a number", "4G", "2", "30G"},
		{"memory zero", "0", "2", "30G"},
		{"cpus not a number", "4096", "two", "30G"},
		{"disk too small a unit", "4096", "2", "500M"},
		{"disk unknown unit", "4096", "2", "30X"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := glesysMinimal()
			c.ApplyDefaults()
			c.Memory, c.CPUs, c.ServerDisk = tc.mem, tc.cpus, tc.disk
			if err := c.Validate(); err == nil {
				t.Fatalf("memory=%q cpus=%q serverDisk=%q should be rejected", tc.mem, tc.cpus, tc.disk)
			}
		})
	}
}

// The node runs Talos, and the provisioner installs nothing on top.
// Each of these stanzas asks for something the provider does not do,
// and an operator who wrote one would otherwise look for its effect.
func TestGlesys_StanzasThatDoNotApply(t *testing.T) {
	for _, tc := range []struct {
		name string
		set  func(*GlesysConfig)
		want string
	}{
		{"k3s airgap install", func(c *GlesysConfig) { c.K3s.Install = "airgap" }, "k3s.install"},
		{"portForwards", func(c *GlesysConfig) { c.PortForwards = []PortForward{{Host: "8443", Guest: "443"}} }, "portForwards"},
		{"gateway skip", func(c *GlesysConfig) { c.Gateway.Skip = true }, "gateway"},
		{"gateway className", func(c *GlesysConfig) { c.Gateway.ClassName = "eg" }, "gateway"},
		{"lifetime", func(c *GlesysConfig) { c.Lifetime.MaxRun = "8h" }, "lifetime"},
		{"registries", func(c *GlesysConfig) {
			c.Registries.Mirrors = map[string]RegistryMirror{"docker.io": {Endpoint: []string{"https://mirror.example"}}}
		}, "registries"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := glesysMinimal()
			tc.set(c)
			c.ApplyDefaults()
			err := c.Validate()
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Errorf("want a refusal naming %q, got %v", tc.want, err)
			}
		})
	}
}

func TestDiskSizeGB(t *testing.T) {
	for _, tc := range []struct {
		in      string
		want    int
		wantErr bool
	}{
		{"30G", 30, false},
		{"30g", 30, false},
		{"30GB", 30, false},
		{"30", 30, false}, // bare number reads as GB
		{"1T", 1024, false},
		{" 30G ", 30, false},
		{"500M", 0, true}, // below the 1G granularity
		{"10K", 0, true},
		{"30X", 0, true},
		{"0G", 0, true},
		{"-5G", 0, true},
		{"", 0, true},
		{"G", 0, true},
	} {
		got, err := DiskSizeGB(tc.in)
		if tc.wantErr {
			if err == nil {
				t.Errorf("DiskSizeGB(%q) should error, got %d", tc.in, got)
			}
			continue
		}
		if err != nil {
			t.Errorf("DiskSizeGB(%q): unexpected error %v", tc.in, err)
			continue
		}
		if got != tc.want {
			t.Errorf("DiskSizeGB(%q) = %d, want %d", tc.in, got, tc.want)
		}
	}
}
