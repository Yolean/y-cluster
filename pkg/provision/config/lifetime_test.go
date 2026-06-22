package config

import (
	"strings"
	"testing"
	"time"
)

func TestLifetime_DisabledByDefault(t *testing.T) {
	c := &QEMUConfig{CommonConfig: CommonConfig{Provider: ProviderQEMU}}
	c.ApplyDefaults()
	if c.Lifetime.Enabled() {
		t.Fatalf("lifetime should be disabled when maxRun is unset, got %+v", c.Lifetime)
	}
	if err := c.Validate(); err != nil {
		t.Fatalf("disabled lifetime must validate, got %v", err)
	}
}

func TestLifetime_DefaultsOnExpiryStop(t *testing.T) {
	c := &QEMUConfig{CommonConfig: CommonConfig{
		Provider: ProviderQEMU,
		Lifetime: LifetimeConfig{MaxRun: "8h"},
	}}
	c.ApplyDefaults()
	if !c.Lifetime.Enabled() {
		t.Fatal("lifetime should be enabled when maxRun set")
	}
	if c.Lifetime.OnExpiry != OnExpiryStop {
		t.Fatalf("OnExpiry default: got %q want %q", c.Lifetime.OnExpiry, OnExpiryStop)
	}
	if err := c.Validate(); err != nil {
		t.Fatalf("valid lifetime rejected: %v", err)
	}
}

func TestLifetime_RespectsExplicitOnExpiry(t *testing.T) {
	c := &QEMUConfig{CommonConfig: CommonConfig{
		Provider: ProviderQEMU,
		Lifetime: LifetimeConfig{MaxRun: "1h", OnExpiry: OnExpiryTeardown},
	}}
	c.ApplyDefaults()
	if c.Lifetime.OnExpiry != OnExpiryTeardown {
		t.Fatalf("explicit OnExpiry overridden: %q", c.Lifetime.OnExpiry)
	}
	if err := c.Validate(); err != nil {
		t.Fatalf("valid lifetime rejected: %v", err)
	}
}

func TestLifetime_MaxRunDuration(t *testing.T) {
	tests := []struct {
		in   string
		want time.Duration
		ok   bool
	}{
		{"", 0, true},
		{"0", 0, true},
		{"8h", 8 * time.Hour, true},
		{"90m", 90 * time.Minute, true},
		{"banana", 0, false},
	}
	for _, tt := range tests {
		d, err := LifetimeConfig{MaxRun: tt.in}.MaxRunDuration()
		if tt.ok && err != nil {
			t.Errorf("MaxRunDuration(%q): unexpected error %v", tt.in, err)
			continue
		}
		if !tt.ok && err == nil {
			t.Errorf("MaxRunDuration(%q): expected error, got nil", tt.in)
			continue
		}
		if tt.ok && d != tt.want {
			t.Errorf("MaxRunDuration(%q) = %v, want %v", tt.in, d, tt.want)
		}
	}
}

func TestLifetime_Validate_BadDuration(t *testing.T) {
	c := &QEMUConfig{CommonConfig: CommonConfig{
		Provider: ProviderQEMU,
		Lifetime: LifetimeConfig{MaxRun: "lol"},
	}}
	c.ApplyDefaults()
	err := c.Validate()
	if err == nil || !strings.Contains(err.Error(), "maxRun") {
		t.Fatalf("want maxRun parse error, got %v", err)
	}
}

func TestLifetime_Validate_NegativeDuration(t *testing.T) {
	c := &QEMUConfig{CommonConfig: CommonConfig{
		Provider: ProviderQEMU,
		Lifetime: LifetimeConfig{MaxRun: "-5m"},
	}}
	c.ApplyDefaults()
	err := c.Validate()
	if err == nil || !strings.Contains(err.Error(), "positive") {
		t.Fatalf("want positive-duration error, got %v", err)
	}
}

func TestLifetime_Validate_BadOnExpiry(t *testing.T) {
	c := &QEMUConfig{CommonConfig: CommonConfig{
		Provider: ProviderQEMU,
		Lifetime: LifetimeConfig{MaxRun: "8h", OnExpiry: "explode"},
	}}
	c.ApplyDefaults()
	err := c.Validate()
	if err == nil || !strings.Contains(err.Error(), "onExpiry") {
		t.Fatalf("want onExpiry enum error, got %v", err)
	}
}
