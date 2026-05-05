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
	if c.AutoTeardownHours != 8 {
		t.Errorf("AutoTeardownHours: %d (want 8)", c.AutoTeardownHours)
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
		ServerType:        "cx32",
		AutoTeardownHours: 24,
		LBGroup:           "team-eu",
		FQDNDomain:        "dev.yolean.se",
	}
	c.ApplyDefaults()
	if c.ServerType != "cx32" {
		t.Errorf("ServerType clobbered: %q", c.ServerType)
	}
	if c.AutoTeardownHours != 24 {
		t.Errorf("AutoTeardownHours clobbered: %d", c.AutoTeardownHours)
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

// TestHetzner_Validate_AutoTeardownHoursNonNegative: 0 means
// "use default" (ApplyDefaults rewrites to 8); negative is an
// operator typo we reject loudly. Auto-teardown is mandatory --
// permanent dev clusters defeat the point.
func TestHetzner_Validate_AutoTeardownHoursNonNegative(t *testing.T) {
	c := &HetznerConfig{
		CommonConfig:      CommonConfig{Provider: ProviderHetzner, Context: "alice-dev"},
		AutoTeardownHours: -1,
	}
	c.ApplyDefaults()
	// ApplyDefaults' explicit zero-handler doesn't override
	// negative values, so -1 survives.
	if c.AutoTeardownHours != -1 {
		t.Fatalf("ApplyDefaults clobbered explicit -1: %d", c.AutoTeardownHours)
	}
	err := c.Validate()
	if err == nil || !strings.Contains(err.Error(), "negative") {
		t.Fatalf("want negative-hours error, got %v", err)
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
