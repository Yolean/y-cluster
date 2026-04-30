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

	// Dir is filled at load time. Not part of the schema.
	Dir string `yaml:"-" json:"-" jsonschema:"-"`
}

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
}

// Validate checks the discriminator and docker-specific invariants.
// docker tunnels the API server through a host port-forward, so a
// guest:6443 entry in PortForwards is required for kubectl to reach
// the cluster.
func (c *DockerConfig) Validate() error {
	if err := c.validateCommon(ProviderDocker); err != nil {
		return err
	}
	return c.requireHostAPIPort()
}
