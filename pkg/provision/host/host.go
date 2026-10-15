// Package host turns a `provider: host` config into what the k3s
// bundle and platform install steps need. y-cluster has no access to
// such a host: the bundle is applied by the host's own tooling and the
// platform goes on through a kubeconfig context.
package host

import (
	"go.uber.org/zap"

	"github.com/Yolean/y-cluster/pkg/provision/cilium"
	"github.com/Yolean/y-cluster/pkg/provision/config"
	"github.com/Yolean/y-cluster/pkg/provision/k3s"
	"github.com/Yolean/y-cluster/pkg/provision/platform"
	"github.com/Yolean/y-cluster/pkg/provision/registries"
)

// BundleSpec is the host's k3s node bundle: the server settings for
// Cilium with tlsSANs and k3s.serverConfig, registries.yaml when
// registries are configured and withoutRegistries is false, and the
// portmap link Cilium needs. Leaving registries.yaml out is for a host
// where other tooling writes that file: applying the bundle leaves the
// host's file alone.
func BundleSpec(c *config.HostConfig, withoutRegistries bool) (k3s.BundleSpec, error) {
	server, err := k3s.CiliumServerSettings(c.TLSSANs, c.K3s.ServerConfig)
	if err != nil {
		return k3s.BundleSpec{}, err
	}
	var reg []byte
	if !withoutRegistries {
		if reg, err = registries.Marshal(c.Registries); err != nil {
			return k3s.BundleSpec{}, err
		}
	}
	return k3s.BundleSpec{
		Version:     c.K3s.Version,
		Install:     c.K3s.Install,
		Arch:        c.Arch,
		Server:      server,
		Registries:  reg,
		PostInstall: []string{cilium.K3sPortmapCommand},
	}, nil
}

// PlatformOptions is what platform install applies to the host's
// context: the config's storage, gateway and cert-manager sections,
// and gateway.dnsHintIP as the GatewayClass's DNS hint.
func PlatformOptions(c *config.HostConfig, logger *zap.Logger) platform.Options {
	return platform.Options{
		Context:     c.Context,
		Nodes:       1,
		Storage:     c.Storage,
		Gateway:     c.Gateway,
		CertManager: c.CertManager,
		DNSHintIP:   c.Gateway.DNSHintIP,
		Logger:      logger,
	}
}
