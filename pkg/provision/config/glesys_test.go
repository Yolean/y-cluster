package config

import "testing"

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
	if c.DataCenter == "" || c.Template == "" {
		t.Errorf("dataCenter/template should default, got %q/%q", c.DataCenter, c.Template)
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

// TestGlesys_NameFollowsContext: the GleSYS hostname is the cluster
// identifier, so the operator writes the context once.
func TestGlesys_NameFollowsContext(t *testing.T) {
	c := glesysMinimal()
	c.ApplyDefaults()
	if c.Name != c.Context {
		t.Fatalf("name %q should follow context %q", c.Name, c.Context)
	}
	if err := c.Validate(); err != nil {
		t.Fatalf("defaulted minimal config should validate: %v", err)
	}
}

// TestGlesys_ContextGuards keeps a cloud cluster from clobbering the
// local one, and keeps the hostname DNS-safe.
func TestGlesys_ContextGuards(t *testing.T) {
	for _, tc := range []struct{ name, ctx, want string }{
		{"empty", "", "context is required"},
		{"local reserved", "local", "reserved for local clusters"},
		{"too short", "abc", "too short"},
		{"uppercase", "Hosting-Test", "must match"},
		{"underscore", "hosting_test", "must match"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := glesysMinimal()
			c.Context = tc.ctx
			c.ApplyDefaults()
			err := c.Validate()
			if err == nil {
				t.Fatalf("context %q should be rejected", tc.ctx)
			}
			if !contains(err.Error(), tc.want) {
				t.Fatalf("error should mention %q; got %v", tc.want, err)
			}
		})
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
	if !contains(err.Error(), "cloudconfig") {
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

// TestGlesys_PauseIsRejected: GleSYS has no pause primitive, so a
// budget asking for it must fail rather than quietly become stop.
func TestGlesys_PauseIsRejected(t *testing.T) {
	c := glesysMinimal()
	c.ApplyDefaults()
	c.Lifetime.MaxRun = "2h"
	c.Lifetime.OnExpiry = OnExpiryPause
	err := c.Validate()
	if err == nil {
		t.Fatal("onExpiry pause should be rejected on glesys")
	}
	if !contains(err.Error(), OnExpiryStop) || !contains(err.Error(), OnExpiryTeardown) {
		t.Fatalf("error should name the supported actions; got %v", err)
	}
}

// TestGlesys_LifetimeStopIsAccepted guards the Enabled() subtlety:
// applyTagDefaults fills onExpiry even when no budget is set, so a
// disabled lifetime must stay valid.
func TestGlesys_LifetimeStopIsAccepted(t *testing.T) {
	c := glesysMinimal()
	c.ApplyDefaults()
	if err := c.Validate(); err != nil {
		t.Fatalf("no budget set should validate: %v", err)
	}
	c.Lifetime.MaxRun = "8h"
	c.Lifetime.OnExpiry = OnExpiryStop
	if err := c.Validate(); err != nil {
		t.Fatalf("stop-on-expiry should validate: %v", err)
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

func contains(haystack, needle string) bool {
	return len(needle) > 0 && len(haystack) >= len(needle) && indexOf(haystack, needle) >= 0
}

func indexOf(h, n string) int {
	for i := 0; i+len(n) <= len(h); i++ {
		if h[i:i+len(n)] == n {
			return i
		}
	}
	return -1
}
