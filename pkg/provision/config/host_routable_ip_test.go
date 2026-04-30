package config

import "testing"

// TestHostRoutableIP_NoForwards covers the cloud-shaped
// (no-host-tunneling) topology: empty PortForwards mean there's no
// host-side dial address to advertise, so the helper returns ""
// and the provisioner omits the dns-hint-ip annotation.
func TestHostRoutableIP_NoForwards(t *testing.T) {
	c := CommonConfig{}
	if got := c.HostRoutableIP(); got != "" {
		t.Fatalf("HostRoutableIP with no forwards: %q", got)
	}
}

// TestHostRoutableIP_NoIngressForward covers a config that has an
// API forward but no ingress (guest:80) forward. The cluster is
// reachable for kubectl but no host loopback maps to Envoy, so
// there's nothing to hint at.
func TestHostRoutableIP_NoIngressForward(t *testing.T) {
	c := CommonConfig{PortForwards: []PortForward{
		{Host: "26443", Guest: "6443"},
	}}
	if got := c.HostRoutableIP(); got != "" {
		t.Fatalf("HostRoutableIP without guest:80: %q", got)
	}
}

// TestHostRoutableIP_WithIngress covers the qemu/docker default
// shape: guest:80 is bound to the host loopback via PortForwards,
// so the helper returns 127.0.0.1.
func TestHostRoutableIP_WithIngress(t *testing.T) {
	c := CommonConfig{PortForwards: []PortForward{
		{Host: "26443", Guest: "6443"},
		{Host: "80", Guest: "80"},
		{Host: "443", Guest: "443"},
	}}
	if got := c.HostRoutableIP(); got != "127.0.0.1" {
		t.Fatalf("HostRoutableIP: %q (want 127.0.0.1)", got)
	}
}

// TestHostRoutableIP_DefaultedConfig pins the breaking-change
// contract: a defaulted config (any provider) gets the hint IP for
// free because the default port forwards include guest:80.
func TestHostRoutableIP_DefaultedConfig(t *testing.T) {
	c := &DockerConfig{CommonConfig: CommonConfig{Provider: ProviderDocker}}
	c.ApplyDefaults()
	if got := c.HostRoutableIP(); got != "127.0.0.1" {
		t.Fatalf("defaulted DockerConfig HostRoutableIP: %q", got)
	}
}
