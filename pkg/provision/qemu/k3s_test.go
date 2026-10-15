package qemu

import (
	"reflect"
	"sort"
	"strings"
	"testing"

	"go.uber.org/zap"

	"github.com/Yolean/y-cluster/pkg/provision/cilium"
	"github.com/Yolean/y-cluster/pkg/provision/config"
)

// The flags qemu provision passed to the k3s installer before the
// server options moved into config.yaml. The bundle's config.yaml must
// say the same: k3s takes a key of its config file exactly as the flag
// of that name.
const flagsBeforeConfigFile = "--write-kubeconfig-mode=644 --disable=traefik --disable=local-storage --disable=gateway-api-crd --flannel-backend=none --disable-network-policy"

func sortedFields(s string) []string {
	f := strings.Fields(s)
	sort.Strings(f)
	return f
}

func TestK3sBundleSpec_ServerSettingsAreTheFormerFlags(t *testing.T) {
	for _, c := range []struct {
		name, bind, want string
	}{
		{"loopback", "127.0.0.1", flagsBeforeConfigFile},
		{"wildcard is dialed over loopback", "0.0.0.0", flagsBeforeConfigFile},
		// The host kubeconfig names the apiserver by APIHost, so an
		// APIHost k3s would not put in its serving cert by itself
		// needs a SAN.
		{"specific address", "192.168.1.10", flagsBeforeConfigFile + " --tls-san=192.168.1.10"},
	} {
		t.Run(c.name, func(t *testing.T) {
			spec, err := Config{BindAddress: c.bind, K3s: K3s{Version: "v1.37.0+k3s1"}}.K3sBundleSpec()
			if err != nil {
				t.Fatal(err)
			}
			got := spec.Server.Flags()
			sort.Strings(got)
			if want := sortedFields(c.want); !reflect.DeepEqual(got, want) {
				t.Errorf("config.yaml as flags:\n got %v\nwant %v", got, want)
			}
		})
	}
}

func TestK3sBundleSpec(t *testing.T) {
	c := &config.QEMUConfig{CommonConfig: config.CommonConfig{Provider: config.ProviderQEMU}}
	c.ApplyDefaults()
	c.Registries = config.Registries{Mirrors: map[string]config.RegistryMirror{"example.test": {Endpoint: []string{"http://127.0.0.1:5000"}}}}
	c.K3s.ServerConfig = map[string]any{"secrets-encryption": true, "write-kubeconfig-mode": "0600"}
	spec, err := FromConfig(c).K3sBundleSpec()
	if err != nil {
		t.Fatal(err)
	}
	if spec.Version != c.K3s.Version || spec.Install != "airgap" || spec.Arch != "amd64" {
		t.Errorf("spec %+v", spec)
	}
	if !strings.Contains(string(spec.Registries), "example.test") {
		t.Errorf("registries.yaml: %s", spec.Registries)
	}
	if !reflect.DeepEqual(spec.PostInstall, []string{cilium.K3sPortmapCommand}) {
		t.Errorf("post-install %v", spec.PostInstall)
	}
	if spec.Server["secrets-encryption"] != true || spec.Server["write-kubeconfig-mode"] != "0600" || spec.Server["flannel-backend"] != "none" {
		t.Errorf("serverConfig not merged over the defaults: %v", spec.Server)
	}

	c.Registries = config.Registries{}
	if spec, err = FromConfig(c).K3sBundleSpec(); err != nil || spec.Registries != nil {
		t.Errorf("no registries configured, no registries.yaml: %q %v", spec.Registries, err)
	}
}

// What `y-cluster platform install -c <qemu config>` applies is what
// provision applied before the platform became a function of its own.
func TestPlatformOptions_AsProvisionInstalled(t *testing.T) {
	c := &config.QEMUConfig{CommonConfig: config.CommonConfig{Provider: config.ProviderQEMU, Context: "ctx"}}
	c.ApplyDefaults()
	c.CertManager.Skip = true
	rt := FromConfig(c)
	got := rt.PlatformOptions(zap.NewNop())
	if got.Context != "ctx" || got.Nodes != 1 || got.DNSHintIP != rt.endpoints().IngressIP || got.DNSHintIP != "127.0.0.1" {
		t.Errorf("options %+v", got)
	}
	if !reflect.DeepEqual(got.Storage, c.Storage) || !reflect.DeepEqual(got.Gateway, c.Gateway) || !got.CertManager.Skip {
		t.Errorf("config sections not passed as they are: %+v", got)
	}
}
