package config

import (
	"strings"
	"testing"
)

// TestHetzner_ApplyDefaults_Empty pins what an unfilled
// HetznerConfig defaults to after ApplyDefaults. The dev-cluster
// shape (HETZNER_PROVISIONER.md) drives the choices here.
func TestHetzner_ApplyDefaults_Empty(t *testing.T) {
	c := &HetznerConfig{CommonConfig: CommonConfig{Provider: ProviderHetzner}}
	c.ApplyDefaults()

	if c.ServerType != "cx23" {
		t.Errorf("ServerType: %q (want cx23)", c.ServerType)
	}
	if c.Location != "hel1" {
		t.Errorf("Location: %q (want hel1)", c.Location)
	}
	if c.OSImage != "ubuntu-24.04" {
		t.Errorf("OSImage: %q (want ubuntu-24.04)", c.OSImage)
	}
	if c.SSHUser != "ystack" {
		t.Errorf("SSHUser: %q (want ystack)", c.SSHUser)
	}
	if c.FQDNDomain != "local.test" {
		t.Errorf("FQDNDomain: %q (want local.test, RFC 6761 reserved)", c.FQDNDomain)
	}
	// LBGroup is host-dependent ($USER); we don't pin a specific
	// value, just that a non-empty USER produces a non-empty
	// LBGroup. ApplyDefaults reads os.Getenv("USER"); CI envs
	// universally set USER, and the test runs on dev/CI hosts only.
	if c.LBGroup == "" {
		t.Errorf("LBGroup is empty; expected $USER fallback")
	}
}

// TestHetzner_ApplyDefaults_RespectsExplicitValues confirms the
// caller's explicit values aren't clobbered by tag defaults.
func TestHetzner_ApplyDefaults_RespectsExplicitValues(t *testing.T) {
	c := &HetznerConfig{
		CommonConfig: CommonConfig{
			Provider: ProviderHetzner,
			Context:  "alice-dev",
			Name:     "alice-dev",
		},
		ServerType: "cx32",
		LBGroup:    "team-eu",
		FQDNDomain: "dev.yolean.se",
	}
	c.ApplyDefaults()
	if c.ServerType != "cx32" {
		t.Errorf("ServerType clobbered: %q", c.ServerType)
	}
	if c.LBGroup != "team-eu" {
		t.Errorf("LBGroup clobbered (USER fallback fired): %q", c.LBGroup)
	}
	if c.FQDNDomain != "dev.yolean.se" {
		t.Errorf("FQDNDomain clobbered: %q", c.FQDNDomain)
	}
}

// TestHetzner_Validate_LifetimeAccepted: the standard lifetime
// config drives hetzner expiry (the in-cluster reaper Job). A set
// maxRun with onExpiry stop / teardown (or empty, which
// ApplyDefaults resolves to stop) must validate.
func TestHetzner_Validate_LifetimeAccepted(t *testing.T) {
	for _, onExpiry := range []string{"", OnExpiryStop, OnExpiryTeardown} {
		c := &HetznerConfig{CommonConfig: CommonConfig{
			Provider: ProviderHetzner,
			Context:  "alice-dev",
			Lifetime: LifetimeConfig{MaxRun: "8h", OnExpiry: onExpiry},
		}}
		c.ApplyDefaults()
		if err := c.Validate(); err != nil {
			t.Errorf("onExpiry %q should validate on hetzner: %v", onExpiry, err)
		}
	}
}

// TestHetzner_Validate_HappyPath confirms a config that satisfies
// every rule passes Validate.
func TestHetzner_Validate_HappyPath(t *testing.T) {
	c := &HetznerConfig{CommonConfig: CommonConfig{
		Provider: ProviderHetzner,
		Context:  "alice-dev",
	}}
	c.ApplyDefaults()
	if err := c.Validate(); err != nil {
		t.Fatalf("happy path should pass: %v", err)
	}
}

// TestHetzner_ImageCache_DisabledByDefault: an unconfigured
// HetznerConfig must keep the cache disabled (Bucket empty) AND
// must not auto-populate the region / index defaults. Defaults
// are deliberately silent on a disabled cache so an operator who
// `cat`s the loaded config sees zero-values, not vestigial
// surface area.
func TestHetzner_ImageCache_DisabledByDefault(t *testing.T) {
	c := &HetznerConfig{CommonConfig: CommonConfig{
		Provider: ProviderHetzner, Context: "alice-dev",
	}}
	c.ApplyDefaults()
	if c.ImageCache.Enabled() {
		t.Errorf("ImageCache.Enabled() = true on default config; want false")
	}
	if c.ImageCache.Region != "" || c.ImageCache.IndexKey != "" {
		t.Errorf("ImageCache regional defaults applied to a disabled cache: %+v", c.ImageCache)
	}
	if err := c.Validate(); err != nil {
		t.Errorf("disabled-cache config should validate: %v", err)
	}
}

// TestHetzner_ImageCache_Defaults: enabling the cache (Bucket set)
// makes Region + IndexKey fall back to hel1 / index.json.
func TestHetzner_ImageCache_Defaults(t *testing.T) {
	c := &HetznerConfig{
		CommonConfig: CommonConfig{Provider: ProviderHetzner, Context: "alice-dev"},
		ImageCache:   HetznerImageCache{Bucket: "y-cluster-examples"},
	}
	c.ApplyDefaults()
	if c.ImageCache.Region != "hel1" {
		t.Errorf("ImageCache.Region: %q (want hel1)", c.ImageCache.Region)
	}
	if c.ImageCache.IndexKey != "index.json" {
		t.Errorf("ImageCache.IndexKey: %q (want index.json)", c.ImageCache.IndexKey)
	}
	if err := c.Validate(); err != nil {
		t.Errorf("enabled-cache happy path should validate: %v", err)
	}
}

// TestHetzner_ImageCache_RejectsBareFields: an operator who sets
// rejectUpstream / region / indexKey but forgot the bucket gets a
// loud error at config-load time, not a silent no-op when the
// pre-load step skips because Bucket is empty.
func TestHetzner_ImageCache_RejectsBareFields(t *testing.T) {
	cases := []HetznerImageCache{
		{Region: "hel1"},
		{IndexKey: "alt-index.json"},
		{RejectUpstream: true},
	}
	for _, ic := range cases {
		c := &HetznerConfig{
			CommonConfig: CommonConfig{Provider: ProviderHetzner, Context: "alice-dev"},
			ImageCache:   ic,
		}
		c.ApplyDefaults()
		err := c.Validate()
		if err == nil || !strings.Contains(err.Error(), "bucket is empty") {
			t.Errorf("imageCache=%+v should fail with bucket-empty error, got %v", ic, err)
		}
	}
}

// TestHetzner_ImageCache_RejectsUnknownRegion: typos in the region
// (`hel-1`, `helsinki`) fail before any S3 endpoint is constructed.
func TestHetzner_ImageCache_RejectsUnknownRegion(t *testing.T) {
	c := &HetznerConfig{
		CommonConfig: CommonConfig{Provider: ProviderHetzner, Context: "alice-dev"},
		ImageCache:   HetznerImageCache{Bucket: "x", Region: "helsinki"},
	}
	c.ApplyDefaults()
	err := c.Validate()
	if err == nil || !strings.Contains(err.Error(), "not a known Hetzner") {
		t.Errorf("region typo should fail with known-region error, got %v", err)
	}
}

// lbGroup defaults to $USER, which containers and CI jobs often lack.
// An empty group used to pass: it produced hostnames with an empty
// label and switched off teardown's "delete the load balancer with
// its last server", so the LB kept billing.
func TestHetzner_Validate_LBGroup(t *testing.T) {
	for _, tc := range []struct {
		name, user, lbGroup, wantErr string
	}{
		{"defaults to $USER", "alice", "", ""},
		{"dotted user name is a valid group", "alice.smith", "", ""},
		{"explicit group", "", "team-qa", ""},
		{"no $USER and nothing configured", "", "", "lbGroup"},
		{"uppercase", "", "Alice", "lbGroup"},
		{"underscore", "", "team_qa", "lbGroup"},
		{"trailing dot", "", "alice.", "lbGroup"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("USER", tc.user)
			c := &HetznerConfig{CommonConfig: CommonConfig{Context: "alice-dev"}, LBGroup: tc.lbGroup}
			c.ApplyDefaults()
			err := c.Validate()
			if tc.wantErr == "" {
				if err != nil {
					t.Fatalf("unexpected error: %v", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("want error naming %s, got %v", tc.wantErr, err)
			}
		})
	}
}

// The provisioner always runs the install script on the server. The
// common default is airgap, which used to be accepted here and then
// ignored.
func TestHetzner_K3sInstall(t *testing.T) {
	c := &HetznerConfig{CommonConfig: CommonConfig{Context: "alice-dev"}}
	c.ApplyDefaults()
	if c.K3s.Install != "script" {
		t.Errorf("default install: got %q, want script (what the provisioner does)", c.K3s.Install)
	}
	if err := c.Validate(); err != nil {
		t.Fatalf("defaults should validate: %v", err)
	}

	asked := &HetznerConfig{CommonConfig: CommonConfig{Context: "alice-dev", K3s: K3sConfig{Install: "airgap"}}}
	asked.ApplyDefaults()
	if err := asked.Validate(); err == nil || !strings.Contains(err.Error(), "not supported on hetzner") {
		t.Errorf("an airgap install the provisioner would ignore must be refused, got %v", err)
	}
}
