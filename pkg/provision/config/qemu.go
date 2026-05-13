package config

// QEMUConfig is the on-disk shape of `y-cluster-provision.yaml` when
// `provider: qemu`. CommonConfig carries the portable fields shared
// with other providers; the fields below are qemu-specific.
type QEMUConfig struct {
	CommonConfig `yaml:",inline" json:",inline"`

	DiskSize string `yaml:"diskSize,omitempty"     json:"diskSize,omitempty"     jsonschema:"default=20G,description=qcow2 disk size as a [num][KMGT] string."`
	SSHPort  string `yaml:"sshPort,omitempty"      json:"sshPort,omitempty"      jsonschema:"default=2222,description=Host port forwarded to the VM's SSH server. Added on top of CommonConfig.PortForwards."`
	CacheDir string `yaml:"cacheDir,omitempty"     json:"cacheDir,omitempty"     jsonschema:"description=Directory for VM disk and cloud image cache. Empty: $HOME/.cache/y-cluster-qemu."`

	// DataDisk, when non-empty, points at an external qcow2 that y-cluster
	// attaches as a labeled `y-cluster-data` ext4 volume. Provision
	// creates the file with `qemu-img create` + `virt-format` if it
	// doesn't exist; teardown leaves the file in place (operator-owned
	// state, NOT cache-managed). The appliance image's pre-baked
	// `LABEL=y-cluster-data /data/yolean ext4` fstab entry mounts it
	// automatically at boot. Use this to test disk-reuse flows
	// (provision -> workload writes /data/yolean -> teardown ->
	// re-provision -> same data still there) locally without going
	// through prepare-export + cloud import.
	DataDisk string `yaml:"dataDisk,omitempty"     json:"dataDisk,omitempty"     jsonschema:"description=External qcow2 to attach as the labeled /data/yolean volume. Created if missing; preserved on teardown. Use absolute path; relative paths resolve against the config-file's directory."`

	// DataDiskSize sizes a freshly-created DataDisk. Ignored when the
	// DataDisk file already exists. Default keeps the same shape as
	// DiskSize so the schema reads consistently.
	DataDiskSize string `yaml:"dataDiskSize,omitempty" json:"dataDiskSize,omitempty" jsonschema:"description=Size for a freshly-created DataDisk ([num][KMGT]). Default 10G; ignored when the DataDisk file already exists or when DataDisk itself is empty."`

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
	if err := c.requireHostAPIPort(); err != nil {
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
