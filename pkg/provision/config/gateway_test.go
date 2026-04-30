package config

import "testing"

// TestGateway_DefaultClassName: an empty GatewayConfig defaults
// to the well-known "y-cluster" GatewayClass name. Pinned because
// downstream consumers (ystack) reference this name verbatim.
func TestGateway_DefaultClassName(t *testing.T) {
	c := &CommonConfig{}
	c.applyCommonDefaults()
	if c.Gateway.ClassName != "y-cluster" {
		t.Fatalf("ClassName: got %q, want y-cluster", c.Gateway.ClassName)
	}
	if c.Gateway.Skip {
		t.Fatal("Skip should remain false by default")
	}
}

// TestGateway_PreservesExplicitClassName: a user pinning a
// non-default class name (e.g. "eg" for compat) survives
// defaulting.
func TestGateway_PreservesExplicitClassName(t *testing.T) {
	c := &CommonConfig{Gateway: GatewayConfig{ClassName: "eg"}}
	c.applyCommonDefaults()
	if c.Gateway.ClassName != "eg" {
		t.Fatalf("ClassName: got %q, want eg", c.Gateway.ClassName)
	}
}

// TestGateway_SkipLeavesClassNameAlone: when Skip is set, the
// defaulter doesn't fill ClassName -- the rendered config / debug
// logs make the operator's intent (no install at all) obvious.
func TestGateway_SkipLeavesClassNameAlone(t *testing.T) {
	c := &CommonConfig{Gateway: GatewayConfig{Skip: true}}
	c.applyCommonDefaults()
	if c.Gateway.ClassName != "" {
		t.Fatalf("Skip:true should leave ClassName empty, got %q", c.Gateway.ClassName)
	}
}

// TestEffectiveGatewayClassName covers the helper Provision uses
// to pick what (if anything) to pass to envoygateway.Install:
// empty when skipped, the configured name otherwise.
func TestEffectiveGatewayClassName(t *testing.T) {
	cases := []struct {
		name string
		gw   GatewayConfig
		want string
	}{
		{"default", GatewayConfig{ClassName: "y-cluster"}, "y-cluster"},
		{"custom name", GatewayConfig{ClassName: "eg"}, "eg"},
		{"skipped", GatewayConfig{Skip: true, ClassName: "y-cluster"}, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := CommonConfig{Gateway: tc.gw}
			if got := c.EffectiveGatewayClassName(); got != tc.want {
				t.Errorf("got %q, want %q", got, tc.want)
			}
		})
	}
}
