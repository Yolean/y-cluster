package config

import (
	"strings"
	"testing"
)

// cloudProviders are the providers that rent a server. The rules
// below are one implementation; running them per provider is what
// shows each provider is wired to it.
var cloudProviders = []string{ProviderHetzner, ProviderGlesys}

// cloudConfig is what LoadProvision produces for a file that says
// only `provider:` and, unless empty, `context:`.
func cloudConfig(t *testing.T, provider, context string) ProviderConfig {
	t.Helper()
	// lbGroup's default is $USER, which CI and containers leave empty.
	t.Setenv("USER", "tester")
	c := NewProviderConfig(provider)
	c.Common().Provider = provider
	c.Common().Context = context
	c.ApplyDefaults()
	return c
}

func TestCloud_MinimalConfigIsValid(t *testing.T) {
	for _, provider := range cloudProviders {
		c := cloudConfig(t, provider, "qa-cluster")
		if err := c.Validate(); err != nil {
			t.Errorf("%s: %v", provider, err)
		}
		if c.Common().Name != "qa-cluster" {
			t.Errorf("%s: name %q should follow the context", provider, c.Common().Name)
		}
	}
}

// The local providers' defaults that a rented server has no use for.
func TestCloud_DefaultsThatDoNotApply(t *testing.T) {
	for _, provider := range cloudProviders {
		c := cloudConfig(t, provider, "qa-cluster").Common()
		if c.K3s.Install != "script" {
			t.Errorf("%s: k3s.install defaults to %q, want script (the common default is airgap)", provider, c.K3s.Install)
		}
		if len(c.PortForwards) != 0 {
			t.Errorf("%s: a server with a public address forwards nothing through this host, got %v", provider, c.PortForwards)
		}
	}
}

// An operator who leaves the context out is told so, and not that
// "local" (which they never wrote) is reserved.
func TestCloud_ContextRules(t *testing.T) {
	for _, provider := range cloudProviders {
		for _, tc := range []struct{ name, context, want string }{
			{"omitted", "", "context is required"},
			{"the local default", "local", "reserved for local clusters"},
			{"three letters", "dev", "too short"},
			{"uppercase", "QA-cluster", "must match"},
			{"underscore", "qa_cluster", "must match"},
			{"leading digit", "1qa-cluster", "must match"},
			{"trailing dash", "qa-cluster-", "must match"},
			{"longer than a DNS label", strings.Repeat("a", 64), "must match"},
		} {
			err := cloudConfig(t, provider, tc.context).Validate()
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Errorf("%s, %s: want an error with %q, got %v", provider, tc.name, tc.want, err)
			}
		}
		if err := cloudConfig(t, provider, strings.Repeat("a", 63)).Validate(); err != nil {
			t.Errorf("%s: a 63 character label is valid: %v", provider, err)
		}
	}
}

// The server is named after the context; a second name would be one
// the cluster cannot be found by.
func TestCloud_NameMustEqualContext(t *testing.T) {
	for _, provider := range cloudProviders {
		c := NewProviderConfig(provider)
		c.Common().Provider = provider
		c.Common().Context = "qa-cluster"
		c.Common().Name = "something-else"
		t.Setenv("USER", "tester")
		c.ApplyDefaults()
		if err := c.Validate(); err == nil || !strings.Contains(err.Error(), "must equal context") {
			t.Errorf("%s: want a name/context mismatch error, got %v", provider, err)
		}
	}
}

func TestCloud_PauseOnExpiryIsRefused(t *testing.T) {
	for _, provider := range cloudProviders {
		c := cloudConfig(t, provider, "qa-cluster")
		// Without a budget the lifetime is off and onExpiry is
		// whatever the tag default filled in.
		c.Common().Lifetime.OnExpiry = OnExpiryPause
		if err := c.Validate(); err != nil {
			t.Errorf("%s: onExpiry without maxRun is not in effect: %v", provider, err)
		}
		c.Common().Lifetime.MaxRun = "2h"
		err := c.Validate()
		if err == nil || !strings.Contains(err.Error(), "no pause/resume primitive") {
			t.Fatalf("%s: want pause refused, got %v", provider, err)
		}
		for _, alternative := range []string{OnExpiryStop, OnExpiryTeardown} {
			if !strings.Contains(err.Error(), alternative) {
				t.Errorf("%s: the refusal should offer %s: %v", provider, alternative, err)
			}
		}
	}
}
