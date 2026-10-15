package config

// DockerConfig is the on-disk shape of `y-cluster-provision.yaml`
// when `provider: docker`. CommonConfig carries the portable
// fields (including PortForwards, which is how the API server and
// any ingress ports are exposed on the host). The container image
// is derived from CommonConfig.K3s.Version at provision time
// (see pkg/provision/docker.ResolveImage), with a manifest probe
// that falls back to the upstream rancher/k3s when the y-cluster
// mirror has not yet been built for the requested version.
type DockerConfig struct {
	CommonConfig `yaml:",inline" json:",inline"`

	// BindAddress is where docker publishes PortForwards on the host.
	// The default is loopback, as for qemu's user-mode forwards: a
	// published port is otherwise reachable from every network the
	// host is on, the k3s API included.
	BindAddress string `yaml:"bindAddress,omitempty" json:"bindAddress,omitempty" jsonschema:"description=IPv4 address the host port forwards (portForwards) listen on. Default 127.0.0.1. 0.0.0.0 exposes them on every interface of the host."`

	// Dir is filled at load time. Not part of the schema.
	Dir string `yaml:"-" json:"-" jsonschema:"-"`
}

// DockerDefaultBindAddress is the host address published ports listen
// on when bindAddress is not set.
const DockerDefaultBindAddress = "127.0.0.1"

// SetDir satisfies configfile.DirAware.
func (c *DockerConfig) SetDir(dir string) { c.Dir = dir }

// ApplyDefaults satisfies configfile.Defaulter. See QEMUConfig
// for the Provider-defaulting rationale -- it lets a config
// file omit `provider:` and have it filled from the discovery
// dispatcher's decision.
func (c *DockerConfig) ApplyDefaults() {
	if c.Provider == "" {
		c.Provider = ProviderDocker
	}
	applyTagDefaults(c)
	c.applyCommonDefaults()
	if c.BindAddress == "" {
		c.BindAddress = DockerDefaultBindAddress
	}
}

// Validate checks the discriminator and docker-specific invariants.
// docker tunnels the API server through a host port-forward, so a
// guest:6443 entry in PortForwards is required for kubectl to reach
// the cluster.
func (c *DockerConfig) Validate() error {
	if err := c.validateCommon(ProviderDocker); err != nil {
		return err
	}
	if !isIPv4Literal(c.BindAddress) {
		return errInvalid("bindAddress must be an IPv4 address such as 127.0.0.1 or 0.0.0.0, got %q", c.BindAddress)
	}
	return c.requireHostAPIPort()
}
