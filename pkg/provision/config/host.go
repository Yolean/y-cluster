package config

import (
	"net"
	"strings"
)

// HostConfig is the on-disk shape of `y-cluster-provision.yaml` when
// `provider: host`: a k3s server on a machine that y-cluster neither
// creates nor removes, set up by whatever already manages the machine.
// y-cluster contributes three steps that need no access to the host
// themselves:
//
//   - `y-cluster k3s bundle` writes the files to put on the node
//     (k3s, its airgap images, installer, config.yaml and, when
//     configured, registries.yaml) with a manifest saying where
//     each goes; the host's own tooling applies them as root;
//   - `y-cluster kubeconfig import` merges the node's k3s.yaml,
//     however the operator read it, under the context below;
//   - `y-cluster platform install` installs Cilium, local-path
//     storage, Envoy Gateway and cert-manager through that context.
//
// provision, teardown and the other verbs that manage a machine refuse
// this provider.
type HostConfig struct {
	CommonConfig `yaml:",inline" json:",inline"`

	// TLSSANs are the addresses besides 127.0.0.1 and the node's own
	// that k3s puts in its serving certificate.
	TLSSANs []string `yaml:"tlsSANs,omitempty" json:"tlsSANs,omitempty" jsonschema:"description=Addresses (IPs or DNS names) besides 127.0.0.1 and the node's own interface addresses at which clients reach the k3s API. They go into the bundle's config.yaml as tls-san. 127.0.0.1 covers an ssh tunnel to the node."`

	// Arch picks the airgap binary and images of the bundle.
	Arch string `yaml:"arch,omitempty" json:"arch,omitempty" jsonschema:"enum=amd64,enum=arm64,default=amd64,description=CPU architecture of the node: which k3s binary and airgap images the bundle carries. Applying a bundle checks it against the node."`

	// Dir is filled at load time. Not part of the schema.
	Dir string `yaml:"-" json:"-" jsonschema:"-"`

	// unused lists the common keys the file set that describe a
	// machine y-cluster runs, for Validate to refuse.
	unused []string
}

// SetDir satisfies configfile.DirAware.
func (c *HostConfig) SetDir(dir string) { c.Dir = dir }

// ApplyDefaults satisfies configfile.Defaulter. Like the cloud
// providers, the context gets no default and name follows it: the
// common default context "local" would point platform install at
// whatever local cluster the operator has. portForwards stay empty.
func (c *HostConfig) ApplyDefaults() {
	if c.Provider == "" {
		c.Provider = ProviderHost
	}
	c.unused = nil
	if c.Memory != "" {
		c.unused = append(c.unused, "memory")
	}
	if c.CPUs != "" {
		c.unused = append(c.unused, "cpus")
	}
	if len(c.PortForwards) > 0 {
		c.unused = append(c.unused, "portForwards")
	}
	if c.Lifetime != (LifetimeConfig{}) {
		c.unused = append(c.unused, "lifetime")
	}
	explicitContext := c.Context != ""
	if c.Name == "" {
		c.Name = c.Context
	}
	applyTagDefaults(c)
	c.applyCommonDefaults()
	if !explicitContext {
		c.Context = ""
	}
	c.PortForwards = nil
}

// Validate checks the discriminator and the host-specific invariants.
func (c *HostConfig) Validate() error {
	if err := c.validateCommon(ProviderHost); err != nil {
		return err
	}
	if len(c.unused) > 0 {
		return errInvalid("%s: provider %q is a host y-cluster does not run; remove them", strings.Join(c.unused, ", "), ProviderHost)
	}
	if c.Context == "" {
		return errInvalid("context is required for provider %q: it names the kubeconfig context that kubeconfig import writes and platform install uses", ProviderHost)
	}
	switch c.K3s.Install {
	case "airgap", "script":
	default:
		return errInvalid("k3s.install must be one of {airgap, script}, got %q", c.K3s.Install)
	}
	switch c.Arch {
	case "amd64", "arm64":
	default:
		return errInvalid("arch must be amd64 or arm64, got %q", c.Arch)
	}
	for i, san := range c.TLSSANs {
		if san == "" || strings.ContainsAny(san, " \t,=/") {
			return errInvalid("tlsSANs[%d] %q must be an IP address or a DNS name", i, san)
		}
	}
	return nil
}

// validDNSHintIP reports whether s is an IP address, the only thing
// consumers of the annotation can dial.
func validDNSHintIP(s string) bool { return net.ParseIP(s) != nil }
