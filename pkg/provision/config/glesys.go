package config

// GlesysConfig is the on-disk shape of `y-cluster-provision.yaml`
// when `provider: glesys`. CommonConfig carries the portable fields,
// Memory and CPUs among them; the fields below are GleSYS-specific.
//
// # Talos, not k3s
//
// The server boots GleSYS's Talos Linux template, and the cloudconfig
// argument to server/create carries the Talos machine config the
// provisioner generates: a single control-plane node that schedules
// workloads. Talos ships Kubernetes, so the k3s settings on
// CommonConfig are not used, and neither are the bundled Envoy
// Gateway and local-path-provisioner installs: what this provider
// delivers is the node and a kubeconfig context for it. The
// registries, gateway and storage stanzas are accepted for
// portability between providers and ignored, which Validate says
// when one of them is set.
//
// # Ingress
//
// A GleSYS KVM server carries a public IPv4 of its own. The
// provisioner reserves the address before the server exists so the
// Talos cluster endpoint in the machine config can name it.
//
// # Access
//
// Talos has no SSH. The provisioner writes a talosconfig next to its
// state sidecar (~/.cache/y-cluster-glesys/<context>-talosconfig),
// which is what `talosctl` takes.
type GlesysConfig struct {
	CommonConfig `yaml:",inline" json:",inline"`

	// DataCenter is the GleSYS datacenter the server is created
	// in. Not validated against an enumeration here: the API
	// rejects an unknown value with a better message than a
	// hardcoded list that goes stale, and server/allowedarguments
	// is the authority.
	DataCenter string `yaml:"dataCenter,omitempty" json:"dataCenter,omitempty" jsonschema:"default=Falkenberg,description=GleSYS datacenter for the server. server/allowedarguments lists the current values."`

	// Platform is the GleSYS virtualization platform. KVM is the
	// only supported value: it is the one that takes a cloudconfig
	// argument, which is how the Talos machine config reaches the
	// node. Validate rejects anything else rather than letting a
	// server come up unconfigured and fail later as a Talos API
	// timeout.
	Platform string `yaml:"platform,omitempty" json:"platform,omitempty" jsonschema:"default=KVM,description=GleSYS virtualization platform. KVM only - it is the platform that accepts a cloudconfig."`

	// Template is the GleSYS OS template name: the Talos Linux
	// release the node boots. The provisioner checks the name
	// against server/templates before creating anything, and its
	// error lists the Talos templates the project can see, so a
	// wrong guess costs one API call and no server.
	Template string `yaml:"template,omitempty" json:"template,omitempty" jsonschema:"default=Talos 1.14,description=GleSYS OS template. A Talos Linux release; the Talos machine config is delivered as the server's cloudconfig."`

	// ServerDisk is the server disk, written in qemu's [num][KMGT]
	// form so the spelling is familiar across VM provisioners.
	// Holds Talos's state and every pulled image. Billed per GB
	// per month, so it is the cheap dimension to be generous with.
	//
	// ServerDisk and not DiskSize: qemu owns the `diskSize` yaml
	// key, and schemagen refuses one key on two providers. It is
	// not on CommonConfig because docker and multipass have no disk
	// to size.
	ServerDisk string `yaml:"serverDisk,omitempty" json:"serverDisk,omitempty" jsonschema:"default=30G,description=Server disk as a [num][KMGT] string. Holds Talos state and pulled images."`

	// Dir is filled at load time from the absolute path of the
	// directory the config came from. Not part of the schema.
	Dir string `yaml:"-" json:"-" jsonschema:"-"`
}

// SetDir records the directory the config was loaded from.
func (c *GlesysConfig) SetDir(dir string) { c.Dir = dir }

// ApplyDefaults runs the cloud defaults (applyCloudDefaults).
// Memory and CPUs keep CommonConfig's 8192/4: the regular
// single-site machine, the same on GleSYS as anywhere else.
func (c *GlesysConfig) ApplyDefaults() {
	if c.Provider == "" {
		c.Provider = ProviderGlesys
	}
	applyCloudDefaults(c, &c.CommonConfig)
}

// Validate checks the GleSYS-specific fields on top of the cloud
// identity rules (validateCloud).
func (c *GlesysConfig) Validate() error {
	if err := c.validateCloud(ProviderGlesys); err != nil {
		return err
	}
	if c.Platform != "KVM" {
		return errInvalid("platform %q is not supported on glesys; KVM is the platform that takes a cloudconfig, which is how the Talos machine config reaches the node", c.Platform)
	}
	if c.Template == "" {
		return errInvalid("template is required on glesys; the Talos Linux template name from server/templates")
	}
	// Sizing reaches the API as integers. Checked here so a typo
	// fails at config load rather than at server/create.
	if _, err := positiveInt(c.Memory); err != nil {
		return errInvalid("memory %q must be a positive whole number of MB", c.Memory)
	}
	if _, err := positiveInt(c.CPUs); err != nil {
		return errInvalid("cpus %q must be a positive whole number of cores", c.CPUs)
	}
	if _, err := DiskSizeGB(c.ServerDisk); err != nil {
		return errInvalid("serverDisk %q: %v", c.ServerDisk, err)
	}
	// The server has a public address of its own, and ingress is
	// that address. An operator who lists forwards expects them to
	// do something.
	if len(c.PortForwards) > 0 {
		return errInvalid("portForwards do not apply on glesys: the server has its own public address, nothing is forwarded through this host")
	}
	// Talos ships Kubernetes. A k3s setting that differs from the
	// defaults is a config written for another provider, and the
	// operator should hear that it does nothing here rather than
	// look for its effect.
	if c.K3s.Install != "script" {
		return errInvalid("k3s.install %q does not apply on glesys: the node runs Talos Linux, which ships Kubernetes; drop the k3s stanza", c.K3s.Install)
	}
	if len(c.Registries.Mirrors) > 0 || len(c.Registries.Configs) > 0 {
		return errInvalid("registries do not apply on glesys: the node runs Talos Linux, whose registry mirrors are part of the machine config; drop the registries stanza")
	}
	// Nothing enforces a budget on this provider yet: hetzner's
	// in-cluster reaper Job is not installed here. A budget that is
	// accepted and then not enforced is the one outcome the
	// lifetime feature exists to prevent.
	if c.Lifetime.Enabled() {
		return errInvalid("lifetime is not supported on glesys yet (no expiry reaper is installed on the node); drop lifetime.maxRun and run `y-cluster teardown` to stop billing")
	}
	return nil
}
