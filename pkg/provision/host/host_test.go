package host

import (
	"reflect"
	"testing"

	"go.uber.org/zap"

	"github.com/Yolean/y-cluster/pkg/provision/cilium"
	"github.com/Yolean/y-cluster/pkg/provision/config"
)

func hostConfig() *config.HostConfig {
	c := &config.HostConfig{CommonConfig: config.CommonConfig{Context: "edge"}, TLSSANs: []string{"127.0.0.1", "192.0.2.10"}}
	c.Registries = config.Registries{Mirrors: map[string]config.RegistryMirror{"example.test": {Endpoint: []string{"https://192.0.2.20"}}}}
	c.Gateway.DNSHintIP = "192.0.2.10"
	c.K3s.ServerConfig = map[string]any{"secrets-encryption": true, "write-kubeconfig-mode": "0600"}
	c.ApplyDefaults()
	return c
}

func TestBundleSpec(t *testing.T) {
	c := hostConfig()
	spec, err := BundleSpec(c, false)
	if err != nil {
		t.Fatal(err)
	}
	if spec.Arch != "amd64" || spec.Install != "airgap" || spec.Version != c.K3s.Version {
		t.Errorf("spec %+v", spec)
	}
	if !reflect.DeepEqual(spec.Server["tls-san"], []string{"127.0.0.1", "192.0.2.10"}) || spec.Server["secrets-encryption"] != true || spec.Server["write-kubeconfig-mode"] != "0600" {
		t.Errorf("server %v", spec.Server)
	}
	if spec.Registries == nil || !reflect.DeepEqual(spec.PostInstall, []string{cilium.K3sPortmapCommand}) {
		t.Errorf("registries %q post-install %v", spec.Registries, spec.PostInstall)
	}
	if spec, err = BundleSpec(c, true); err != nil || spec.Registries != nil {
		t.Errorf("without registries: %q %v", spec.Registries, err)
	}
}

func TestPlatformOptions(t *testing.T) {
	c := hostConfig()
	o := PlatformOptions(c, zap.NewNop())
	if o.Context != "edge" || o.DNSHintIP != "192.0.2.10" || o.Nodes != 1 || o.Gateway.ClassName != "y-cluster" || o.Storage.ReclaimPolicy != "Retain" {
		t.Errorf("options %+v", o)
	}
}
