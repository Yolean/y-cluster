package config

// QEMUConfig is the on-disk shape of `y-cluster-provision.yaml` when
// `provider: qemu`. CommonConfig carries the portable fields shared
// with other providers; the fields below are qemu-specific.
type QEMUConfig struct {
	CommonConfig `yaml:",inline" json:",inline"`

	DiskSize string `yaml:"diskSize,omitempty"     json:"diskSize,omitempty"     jsonschema:"default=40G,description=qcow2 disk size as a [num][KMGT] string."`
	SSHPort  string `yaml:"sshPort,omitempty"      json:"sshPort,omitempty"      jsonschema:"default=2222,description=Host port forwarded to the VM's SSH server. Added on top of CommonConfig.PortForwards."`
	CacheDir string `yaml:"cacheDir,omitempty"     json:"cacheDir,omitempty"     jsonschema:"description=Directory for VM disk and cloud image cache. Empty: $HOME/.cache/y-cluster-qemu."`

	// Dir is filled at load time from the absolute path of the
	// directory the config came from. Not part of the schema.
	Dir string `yaml:"-" json:"-" jsonschema:"-"`
}

// SetDir satisfies configfile.DirAware so relative paths in the
// YAML can resolve against the directory the file came from.
func (c *QEMUConfig) SetDir(dir string) { c.Dir = dir }

// ApplyDefaults satisfies configfile.Defaulter. Tag-driven defaults
// run via reflection (covering both common and qemu-specific
// fields); pin-driven defaults run via the helpers in defaults.go.
//
// Provider defaulting handles the LoadProvision discovery path:
// when the YAML omits `provider:` and the dispatcher has already
// decided this is the qemu config (because DiscoverProvider said
// so), the field is empty after unmarshal -- we fill it so
// Validate sees a coherent state.
func (c *QEMUConfig) ApplyDefaults() {
	if c.Provider == "" {
		c.Provider = ProviderQEMU
	}
	applyTagDefaults(c)
	c.applyCommonDefaults()
}

// Validate checks the discriminator and qemu-specific invariants.
func (c *QEMUConfig) Validate() error {
	if err := c.validateCommon(ProviderQEMU); err != nil {
		return err
	}
	if c.SSHPort == "" {
		return errInvalid("sshPort must not be empty after defaults")
	}
	switch c.K3s.Install {
	case "", "airgap", "script":
	default:
		return errInvalid("k3s.install must be one of {airgap, script}, got %q", c.K3s.Install)
	}
	return nil
}
