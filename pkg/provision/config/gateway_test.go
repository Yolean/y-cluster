package config

import (
	"strings"
	"testing"
)

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
//
// Exercised through each provider's ApplyDefaults, which is what a
// loaded config goes through. applyCommonDefaults alone honours Skip;
// the tag-driven pass that runs before it does not, and used to fill
// ClassName from a tag default regardless.
func TestGateway_SkipLeavesClassNameAlone(t *testing.T) {
	skipped := CommonConfig{Gateway: GatewayConfig{Skip: true}}
	for name, defaulted := range map[string]func() GatewayConfig{
		"qemu": func() GatewayConfig {
			c := &QEMUConfig{CommonConfig: skipped}
			c.ApplyDefaults()
			return c.Gateway
		},
		"docker": func() GatewayConfig {
			c := &DockerConfig{CommonConfig: skipped}
			c.ApplyDefaults()
			return c.Gateway
		},
		"multipass": func() GatewayConfig {
			c := &MultipassConfig{CommonConfig: skipped}
			c.ApplyDefaults()
			return c.Gateway
		},
	} {
		got := defaulted()
		if got.ClassName != "" {
			t.Errorf("%s: Skip:true should leave ClassName empty, got %q", name, got.ClassName)
		}
		// Skip also keeps Resources empty so a downstream consumer
		// reading the rendered config can tell "operator didn't ask
		// for an install" from "operator asked, defaults applied".
		if got.Resources != (GatewayResources{}) {
			t.Errorf("%s: Skip:true should leave Resources zero, got %+v", name, got.Resources)
		}
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

// gateway.version, installSha256 and proxyImage are checked at load,
// each message naming its field. None of them has a default.
func TestGateway_ReleaseValidation(t *testing.T) {
	const (
		digest = "0412a72907e57ff9b73c56a7bf6df5190bf0f6e4f8bb4bba34e38630bbab5778"
		image  = "docker.io/envoyproxy/envoy:distroless-v1.39.5@sha256:" + digest
	)
	for _, tc := range []struct {
		name    string
		gateway GatewayConfig
		wantErr string
	}{
		{"none set", GatewayConfig{}, ""},
		{"version with its digest", GatewayConfig{Version: "v1.9.2", InstallSHA256: digest}, ""},
		{"uppercase digest", GatewayConfig{Version: "v1.9.2", InstallSHA256: strings.ToUpper(digest)}, ""},
		{"a pre-release is left to the cluster check", GatewayConfig{Version: "v1.10.0-rc.1", InstallSHA256: digest}, ""},
		{"proxy image alone", GatewayConfig{ProxyImage: image}, ""},
		{"proxy image without a tag", GatewayConfig{ProxyImage: "envoyproxy/envoy@sha256:" + digest}, ""},
		{"proxy image on a registry port", GatewayConfig{ProxyImage: "registry.example:5000/envoyproxy/envoy:v1.39.5@sha256:" + digest}, ""},
		{"all three", GatewayConfig{Version: "v1.9.2", InstallSHA256: digest, ProxyImage: image}, ""},
		{"version without a digest", GatewayConfig{Version: "v1.9.2"}, "gateway.version v1.9.2 requires gateway.installSha256"},
		{"digest without a version", GatewayConfig{InstallSHA256: digest}, "gateway.installSha256 is set without gateway.version"},
		{"short digest", GatewayConfig{Version: "v1.9.2", InstallSHA256: digest[:63]}, "gateway.installSha256"},
		{"digest with a prefix", GatewayConfig{Version: "v1.9.2", InstallSHA256: "sha256:" + digest}, "gateway.installSha256"},
		{"digest not hex", GatewayConfig{Version: "v1.9.2", InstallSHA256: strings.Repeat("g", 64)}, "gateway.installSha256"},
		{"version without v", GatewayConfig{Version: "1.9.2", InstallSHA256: digest}, "gateway.version"},
		{"version as a path", GatewayConfig{Version: "v1.9.2/../x", InstallSHA256: digest}, "gateway.version"},
		{"proxy image by tag only", GatewayConfig{ProxyImage: "docker.io/envoyproxy/envoy:distroless-v1.39.5"}, "gateway.proxyImage"},
		{"proxy image short digest", GatewayConfig{ProxyImage: "envoyproxy/envoy:v1.39.5@sha256:abc"}, "gateway.proxyImage"},
		{"proxy image uppercase digest", GatewayConfig{ProxyImage: "envoyproxy/envoy:v1.39.5@sha256:" + strings.ToUpper(digest)}, "gateway.proxyImage"},
		{"proxy image with a space", GatewayConfig{ProxyImage: "envoyproxy/envoy:v1 @sha256:" + digest}, "gateway.proxyImage"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			for _, c := range []interface {
				ApplyDefaults()
				Validate() error
				Common() *CommonConfig
			}{&QEMUConfig{}, &HostConfig{CommonConfig: CommonConfig{Context: "edge"}}, &DockerConfig{}} {
				c.ApplyDefaults()
				g := tc.gateway
				g.ClassName = c.Common().Gateway.ClassName
				g.Resources = c.Common().Gateway.Resources
				c.Common().Gateway = g
				err := c.Validate()
				if tc.wantErr == "" {
					if err != nil {
						t.Fatalf("%T: unexpected error: %v", c, err)
					}
					continue
				}
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("%T: want error containing %q, got %v", c, tc.wantErr, err)
				}
			}
		})
	}
}

// The new fields stay empty through defaulting: an empty version is
// y-cluster's pin, decided by the envoygateway package at install.
func TestGateway_ReleaseHasNoDefault(t *testing.T) {
	c := &QEMUConfig{}
	c.ApplyDefaults()
	if g := c.Gateway; g.Version != "" || g.InstallSHA256 != "" || g.ProxyImage != "" {
		t.Fatalf("defaults filled %+v", g)
	}
}
