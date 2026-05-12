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
	// Skip also keeps Resources empty so a downstream consumer
	// reading the rendered config can tell "operator didn't ask
	// for an install" from "operator asked, defaults applied".
	if c.Gateway.Resources != (GatewayResources{}) {
		t.Fatalf("Skip:true should leave Resources zero, got %+v", c.Gateway.Resources)
	}
}

// TestGateway_DefaultResources pins the lower-than-upstream
// defaults that single-user dev clusters benefit from. Upstream
// EG ships 100m/256Mi controller and 100m/512Mi proxy, which
// oversubscribe a 2GB appliance node. Changing these values is
// a contract change for anyone running on those budgets.
func TestGateway_DefaultResources(t *testing.T) {
	c := &CommonConfig{}
	c.applyCommonDefaults()
	if c.Gateway.Resources.Controller.CPU != "10m" {
		t.Errorf("Controller.CPU: got %q, want 10m", c.Gateway.Resources.Controller.CPU)
	}
	if c.Gateway.Resources.Controller.Memory != "64Mi" {
		t.Errorf("Controller.Memory: got %q, want 64Mi", c.Gateway.Resources.Controller.Memory)
	}
	if c.Gateway.Resources.Proxy.CPU != "10m" {
		t.Errorf("Proxy.CPU: got %q, want 10m", c.Gateway.Resources.Proxy.CPU)
	}
	if c.Gateway.Resources.Proxy.Memory != "128Mi" {
		t.Errorf("Proxy.Memory: got %q, want 128Mi", c.Gateway.Resources.Proxy.Memory)
	}
}

// TestGateway_PreservesExplicitResources: an operator setting
// any subset of fields keeps their explicit values; only
// unset fields default.
func TestGateway_PreservesExplicitResources(t *testing.T) {
	c := &CommonConfig{Gateway: GatewayConfig{
		Resources: GatewayResources{
			Controller: ResourceRequests{CPU: "200m"},
			Proxy:      ResourceRequests{Memory: "1Gi"},
		},
	}}
	c.applyCommonDefaults()
	if c.Gateway.Resources.Controller.CPU != "200m" {
		t.Errorf("explicit Controller.CPU lost: %q", c.Gateway.Resources.Controller.CPU)
	}
	if c.Gateway.Resources.Controller.Memory != "64Mi" {
		t.Errorf("unset Controller.Memory should default, got %q", c.Gateway.Resources.Controller.Memory)
	}
	if c.Gateway.Resources.Proxy.CPU != "10m" {
		t.Errorf("unset Proxy.CPU should default, got %q", c.Gateway.Resources.Proxy.CPU)
	}
	if c.Gateway.Resources.Proxy.Memory != "1Gi" {
		t.Errorf("explicit Proxy.Memory lost: %q", c.Gateway.Resources.Proxy.Memory)
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
