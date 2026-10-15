package config

import (
	"strings"
	"testing"
)

func TestLoadProvision_Host(t *testing.T) {
	dir := writeProvision(t, `provider: host
context: edge
tlsSANs: [127.0.0.1, 192.0.2.10]
k3s:
  serverConfig:
    secrets-encryption: true
    write-kubeconfig-mode: "0600"
gateway:
  dnsHintIP: 192.0.2.10
certManager:
  skip: true
`)
	loaded, err := LoadProvision(dir)
	if err != nil {
		t.Fatal(err)
	}
	c, ok := loaded.(*HostConfig)
	if !ok {
		t.Fatalf("loaded %T", loaded)
	}
	if c.Name != "edge" || c.Context != "edge" {
		t.Errorf("name follows context: %q/%q", c.Name, c.Context)
	}
	if c.Arch != "amd64" || c.K3s.Install != "airgap" || c.K3s.Version != K3sDefaultVersion() {
		t.Errorf("defaults: arch %q install %q version %q", c.Arch, c.K3s.Install, c.K3s.Version)
	}
	if len(c.PortForwards) != 0 {
		t.Errorf("a host forwards nothing through this machine: %v", c.PortForwards)
	}
	if c.Gateway.DNSHintIP != "192.0.2.10" || c.Gateway.ClassName != "y-cluster" || !c.CertManager.Skip {
		t.Errorf("gateway %+v certManager %+v", c.Gateway, c.CertManager)
	}
	if c.K3s.ServerConfig["secrets-encryption"] != true || c.K3s.ServerConfig["write-kubeconfig-mode"] != "0600" {
		t.Errorf("serverConfig %v", c.K3s.ServerConfig)
	}
	if c.Storage.Path != "/data/yolean" {
		t.Errorf("storage defaults: %+v", c.Storage)
	}
}

func TestLoadProvision_HostRefuses(t *testing.T) {
	for name, c := range map[string]struct{ body, want string }{
		"no context":         {"provider: host\n", "context is required"},
		"memory":             {"provider: host\ncontext: edge\nmemory: \"4096\"\n", "memory"},
		"portForwards":       {"provider: host\ncontext: edge\nportForwards: [{host: \"6443\", guest: \"6443\"}]\n", "portForwards"},
		"lifetime":           {"provider: host\ncontext: edge\nlifetime: {maxRun: 8h}\n", "lifetime"},
		"arch":               {"provider: host\ncontext: edge\narch: riscv64\n", "arch"},
		"san with a comma":   {"provider: host\ncontext: edge\ntlsSANs: [\"a,b\"]\n", "tlsSANs[0]"},
		"dns hint not an IP": {"provider: host\ncontext: edge\ngateway: {dnsHintIP: node.example.test}\n", "dnsHintIP"},
		"cni setting":        {"provider: host\ncontext: edge\nk3s: {serverConfig: {flannel-backend: vxlan}}\n", "flannel-backend"},
		"unquoted octal":     {"provider: host\ncontext: edge\nk3s: {serverConfig: {write-kubeconfig-mode: 0600}}\n", "quote"},
		"env substitution":   {"provider: host\ncontext: edge\nk3s: {serverConfig: {token: \"${TOKEN}\"}}\n", "env substitution"},
		"unknown install":    {"provider: host\ncontext: edge\nk3s: {install: curl}\n", "k3s.install"},
	} {
		t.Run(name, func(t *testing.T) {
			_, err := LoadProvision(writeProvision(t, c.body))
			if err == nil || !strings.Contains(err.Error(), c.want) {
				t.Errorf("err %v, want it to mention %q", err, c.want)
			}
		})
	}
}

// k3s.serverConfig reaches k3s on qemu and host only, and a DNS hint
// is configured on host only; elsewhere they are refused rather than
// ignored.
func TestLoadProvision_HostOnlyAndQemuOnlyKeys(t *testing.T) {
	if _, err := LoadProvision(writeProvision(t, "provider: qemu\nk3s: {serverConfig: {secrets-encryption: true}}\n")); err != nil {
		t.Errorf("qemu takes k3s.serverConfig: %v", err)
	}
	for _, p := range []string{ProviderDocker, ProviderMultipass} {
		_, err := LoadProvision(writeProvision(t, "provider: "+p+"\nk3s: {serverConfig: {secrets-encryption: true}}\n"))
		if err == nil || !strings.Contains(err.Error(), "serverConfig") {
			t.Errorf("%s: k3s.serverConfig accepted: %v", p, err)
		}
	}
	for _, p := range []string{ProviderQEMU, ProviderDocker} {
		_, err := LoadProvision(writeProvision(t, "provider: "+p+"\ngateway: {dnsHintIP: 192.0.2.10}\n"))
		if err == nil || !strings.Contains(err.Error(), "dnsHintIP") {
			t.Errorf("%s: gateway.dnsHintIP accepted: %v", p, err)
		}
	}
	if !ExistingHost(ProviderHost) || ExistingHost(ProviderQEMU) {
		t.Error("ExistingHost")
	}
}
