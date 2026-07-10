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

// TestHetzner_Validate_ContextRequired pins the rule that
// HetznerConfig refuses to load without a context. The qemu /
// docker / multipass providers default context to "local"; for
// hetzner we explicitly forbid that fallback because two devs
// sharing kubeconfig would clobber each other on every provision.
func TestHetzner_Validate_ContextRequired(t *testing.T) {
	c := &HetznerConfig{CommonConfig: CommonConfig{Provider: ProviderHetzner}}
	c.ApplyDefaults()
	c.Context = "" // ApplyDefaults from CommonConfig may set it; force-empty
	err := c.Validate()
	if err == nil {
		t.Fatal("expected error for empty context")
	}
	if !strings.Contains(err.Error(), "context is required") {
		t.Errorf("error should mention context is required: %v", err)
	}
}

// TestHetzner_Validate_RejectsLocal pins the rule that "local"
// is reserved for the operator's local cluster. Stamping it onto
// a Hetzner Cloud cluster would mean every kubectl-context merge
// from this provisioner overwrites the operator's locally-running
// cluster's context entry.
func TestHetzner_Validate_RejectsLocal(t *testing.T) {
	c := &HetznerConfig{CommonConfig: CommonConfig{Provider: ProviderHetzner, Context: "local"}}
	c.ApplyDefaults()
	err := c.Validate()
	if err == nil || !strings.Contains(err.Error(), "reserved") {
		t.Fatalf("want 'reserved' error for context=local, got %v", err)
	}
}

// TestHetzner_Validate_ContextLength pins the >= 4 chars rule.
// Three-letter names (`dev`, `qa`, `foo`) are too generic and
// likely to collide between developers sharing a project.
func TestHetzner_Validate_ContextLength(t *testing.T) {
	for _, ctx := range []string{"a", "ab", "dev"} {
		c := &HetznerConfig{CommonConfig: CommonConfig{Provider: ProviderHetzner, Context: ctx}}
		c.ApplyDefaults()
		err := c.Validate()
		if err == nil || !strings.Contains(err.Error(), "too short") {
			t.Errorf("context %q: expected too-short error, got %v", ctx, err)
		}
	}
}

// TestHetzner_Validate_ContextDNSLabel pins the DNS-label-safe
// rule. Hetzner Cloud rejects names with uppercase / underscores
// / leading-digit anyway; we surface the failure in our own
// validate-step instead so the operator sees a clear message
// before the API call fires.
func TestHetzner_Validate_ContextDNSLabel(t *testing.T) {
	cases := []string{
		"Alice-Dev",  // uppercase
		"alice_dev",  // underscore
		"-alice-dev", // leading dash
		"1alice",     // leading digit
		"alice-dev-", // trailing dash (regex requires alphanumeric end)
		"alice dev",  // space
	}
	for _, ctx := range cases {
		c := &HetznerConfig{CommonConfig: CommonConfig{Provider: ProviderHetzner, Context: ctx}}
		c.ApplyDefaults()
		err := c.Validate()
		if err == nil {
			t.Errorf("context %q should fail DNS-label check", ctx)
		}
	}
}

// TestHetzner_Validate_NameMustEqualContext pins the rule that
// the Hetzner server name IS the cluster identifier. Allowing a
// Name that differs from Context would mean cluster.Lookup would
// have to know which one to match against; this branch keeps
// it simple: there's one identifier, and it's Context.
func TestHetzner_Validate_NameMustEqualContext(t *testing.T) {
	c := &HetznerConfig{CommonConfig: CommonConfig{
		Provider: ProviderHetzner,
		Context:  "alice-dev",
		Name:     "different-name",
	}}
	c.ApplyDefaults()
	err := c.Validate()
	if err == nil || !strings.Contains(err.Error(), "must equal context") {
		t.Fatalf("want name-mismatch error, got %v", err)
	}
}

// TestHetzner_Validate_NameDefaultsToContext: leaving Name empty
// is fine; ApplyDefaults' tag-driven handling fills CommonConfig.
// We just have to not REJECT the empty case. The provisioner
// actually uses Context as the server name regardless.
func TestHetzner_Validate_NameDefaultsToContext(t *testing.T) {
	c := &HetznerConfig{CommonConfig: CommonConfig{Provider: ProviderHetzner, Context: "alice-dev"}}
	c.ApplyDefaults()
	if err := c.Validate(); err != nil {
		t.Errorf("empty Name should be fine when Context is set: %v", err)
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

// TestHetzner_Validate_LifetimePauseRejected: Hetzner Cloud has no
// pause/resume primitive (no SIGSTOP analog for a cloud server),
// so an armed budget with onExpiry pause must fail loudly instead
// of silently downgrading to stop.
func TestHetzner_Validate_LifetimePauseRejected(t *testing.T) {
	c := &HetznerConfig{CommonConfig: CommonConfig{
		Provider: ProviderHetzner,
		Context:  "alice-dev",
		Lifetime: LifetimeConfig{MaxRun: "8h", OnExpiry: OnExpiryPause},
	}}
	c.ApplyDefaults()
	err := c.Validate()
	if err == nil {
		t.Fatal("want error for lifetime.onExpiry pause on hetzner, got nil")
	}
	if !strings.Contains(err.Error(), "no pause/resume primitive") {
		t.Errorf("error should explain the missing primitive: %v", err)
	}
	if !strings.Contains(err.Error(), "stop") || !strings.Contains(err.Error(), "teardown") {
		t.Errorf("error should name stop and teardown as the options: %v", err)
	}
}

// TestHetzner_Validate_LifetimeDisabledValid: empty maxRun keeps
// the whole feature off and must stay valid even though
// ApplyDefaults fills lifetime.onExpiry's tag default (stop) on a
// disabled lifetime -- Enabled() is the emptiness test, not
// onExpiry presence.
func TestHetzner_Validate_LifetimeDisabledValid(t *testing.T) {
	c := &HetznerConfig{CommonConfig: CommonConfig{
		Provider: ProviderHetzner,
		Context:  "alice-dev",
	}}
	c.ApplyDefaults()
	if c.Lifetime.Enabled() {
		t.Fatalf("lifetime unexpectedly enabled by defaults: %+v", c.Lifetime)
	}
	if c.Lifetime.OnExpiry != OnExpiryStop {
		t.Fatalf("expected tag default onExpiry stop even when disabled, got %q", c.Lifetime.OnExpiry)
	}
	if err := c.Validate(); err != nil {
		t.Fatalf("disabled lifetime should pass: %v", err)
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
