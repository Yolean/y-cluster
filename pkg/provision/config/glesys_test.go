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

// TestGlesys_DefaultsAreTheSmallMachine pins the sizing defaults.
// They are deliberately below CommonConfig's 8192/4 because GleSYS
// bills hourly, and the mechanism that makes the smaller values win
// is subtle (pre-set before applyTagDefaults, which only fills empty
// strings), so a regression here would silently double the bill.
func TestGlesys_DefaultsAreTheSmallMachine(t *testing.T) {
	c := glesysMinimal()
	c.ApplyDefaults()

	if c.Memory != "4096" {
		t.Errorf("memory: got %q, want 4096 (CommonConfig's 8192 must not win)", c.Memory)
	}
	if c.CPUs != "2" {
		t.Errorf("cpus: got %q, want 2 (CommonConfig's 4 must not win)", c.CPUs)
	}
	if c.ServerDisk != "30G" {
		t.Errorf("serverDisk: got %q, want 30G", c.ServerDisk)
	}
	if c.Platform != "KVM" {
		t.Errorf("platform: got %q, want KVM", c.Platform)
	}
	// Where the server runs and what it boots are not details: a
	// default that drifts moves new clusters to another country or
	// another OS release without anyone having asked.
	if c.DataCenter != "Stockholm" {
		t.Errorf("dataCenter: got %q, want Stockholm", c.DataCenter)
	}
	if c.Template != "ubuntu-24-04" {
		t.Errorf("template: got %q, want ubuntu-24-04", c.Template)
	}
}

// TestGlesys_ExplicitSizingSurvivesDefaults is the other half of the
// pre-set mechanism: an operator who asks for a bigger machine must
// keep it.
func TestGlesys_ExplicitSizingSurvivesDefaults(t *testing.T) {
	c := glesysMinimal()
	c.Memory = "8192"
	c.CPUs = "4"
	c.ServerDisk = "80G"
	c.ApplyDefaults()

	if c.Memory != "8192" || c.CPUs != "4" || c.ServerDisk != "80G" {
		t.Fatalf("explicit sizing was overwritten: memory=%q cpus=%q serverDisk=%q", c.Memory, c.CPUs, c.ServerDisk)
	}
}

// TestGlesys_PlatformMustBeKVM: KVM is the only platform that takes a
// cloudconfig, and cloudconfig is how k3s gets installed. Failing at
// config load beats an unexplained SSH timeout ten minutes later.
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

// The type comment says hosting, not appliance. These are the two
// fields through which a config could ask for the appliance shape
// anyway, and a provisioner that ignored them would leave the
// operator waiting for something that never happens.
func TestGlesys_NoApplianceShape(t *testing.T) {
	c := glesysMinimal()
	c.K3s.Install = "airgap"
	c.ApplyDefaults()
	if err := c.Validate(); err == nil || !strings.Contains(err.Error(), "k3s.install") {
		t.Errorf("airgap install: want it refused, got %v", err)
	}

	c = glesysMinimal()
	c.PortForwards = []PortForward{{Host: "8443", Guest: "443"}}
	c.ApplyDefaults()
	if err := c.Validate(); err == nil || !strings.Contains(err.Error(), "portForwards") {
		t.Errorf("explicit portForwards: want them refused, got %v", err)
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
