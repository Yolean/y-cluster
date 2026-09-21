package config

// GleSYS sizing defaults. Deliberately below CommonConfig's 8192/4:
// GleSYS bills by the hour, so the default should be the smallest
// machine that runs the stack rather than the roomiest. The
// platform components are all tuned small (redpanda requests
// 200Mi, the Envoy Gateway controller 64Mi); the variable is
// buildkit, which sizes against the image it is building.
const (
	glesysDefaultMemory = "4096"
	glesysDefaultCPUs   = "2"
)

// GlesysSSHUser is the unprivileged user the cloudconfig creates and
// the y-cluster CLI authenticates as.
//
// Fixed rather than configurable, following qemu (which pins the same
// name in its cloud-init template) rather than hetzner (which exposes
// a sshUser field). Two reasons: this provisioner creates the user
// itself, so nothing outside the cloudconfig constrains the name; and
// hetzner already claims the sshUser yaml key, so exposing it here
// would need either a second spelling for one concept or a move to
// CommonConfig - which would hand docker and qemu a field they would
// silently ignore. Promoting it later, together with wiring qemu to
// read it, is the clean version of that change.
const GlesysSSHUser = "ystack"

// GlesysConfig is the on-disk shape of `y-cluster-provision.yaml`
// when `provider: glesys`. CommonConfig carries portable fields --
// including Memory and CPUs, which this provisioner reads rather
// than redeclaring; the fields below are GleSYS-specific.
//
// # Hosting, not appliance
//
// This provisioner targets HOSTING only: the stack is installed in
// place on a VM that GleSYS provisions, and every image is pulled
// by the k3s setup and by the cluster itself. There is deliberately
// no image cache, no OCI preload, no upstream-pull lockdown and no
// export/import - the appliance machinery the hetzner provisioner
// grew (imageCache, preload, rejectUpstream, prepare-export) has no
// counterpart here. A GleSYS cluster that cannot reach a registry
// is broken, not degraded.
//
// # Ingress
//
// A GleSYS KVM server carries a public IPv4 of its own, so ingress
// is the node address plus k3s ServiceLB. There is no shared load
// balancer and no per-context FQDN, which is the other reason this
// provisioner is smaller than hetzner's.
type GlesysConfig struct {
	CommonConfig `yaml:",inline" json:",inline"`

	// DataCenter is the GleSYS datacenter the server is created
	// in. Not validated against an enumeration here: the API
	// rejects an unknown value with a better message than a
	// hardcoded list that goes stale, and server/allowedarguments
	// is the authority.
	DataCenter string `yaml:"dataCenter,omitempty" json:"dataCenter,omitempty" jsonschema:"default=Stockholm,description=GleSYS datacenter for the server. server/allowedarguments lists the current values."`

	// Platform is the GleSYS virtualization platform. KVM is the
	// only supported value: it is the one that takes a cloudconfig
	// argument, which is how k3s gets installed at first boot.
	// Validate rejects anything else rather than letting a server
	// come up with no cloud-init and fail later as an unexplained
	// SSH timeout.
	Platform string `yaml:"platform,omitempty" json:"platform,omitempty" jsonschema:"default=KVM,description=GleSYS virtualization platform. KVM only - it is the platform that accepts a cloudconfig."`

	// Template is the GleSYS OS template name. An Ubuntu cloud
	// image, matching the qemu and hetzner provisioners, so the
	// cloud-init that installs k3s keeps one shape everywhere.
	Template string `yaml:"template,omitempty" json:"template,omitempty" jsonschema:"default=ubuntu-24-04,description=GleSYS OS template. An Ubuntu cloud image; k3s is installed at first boot via cloudconfig."`

	// ServerDisk is the server disk, written in qemu's [num][KMGT]
	// form so the spelling is familiar across VM provisioners.
	// Holds k3s state, every pulled image, the registry's blobs
	// and buildkit's cache. Billed per GB per month, so it is the
	// cheap dimension to be generous with.
	//
	// Named ServerDisk rather than the natural DiskSize because
	// qemu already claims that yaml key and schemagen's collision
	// check forces disambiguation - the same reason hetzner has
	// OSImage where multipass has Image. Moving it to CommonConfig
	// was the other option the check offers, but docker and
	// multipass have no disk to size, so it is not portable.
	ServerDisk string `yaml:"serverDisk,omitempty" json:"serverDisk,omitempty" jsonschema:"default=30G,description=Server disk as a [num][KMGT] string. Holds k3s state, pulled images, registry blobs and the buildkit cache."`

	// Dir is filled at load time from the absolute path of the
	// directory the config came from. Not part of the schema.
	Dir string `yaml:"-" json:"-" jsonschema:"-"`
}

// SetDir records the directory the config was loaded from.
func (c *GlesysConfig) SetDir(dir string) { c.Dir = dir }

// ApplyDefaults pre-sets the GleSYS sizing and then runs the cloud
// defaults (applyCloudDefaults). Memory and CPUs live on CommonConfig,
// whose tags say 8192/4 for the local providers; the tag defaults
// only fill EMPTY strings, so setting them first is what makes the
// smaller cloud defaults win without touching the shared struct.
func (c *GlesysConfig) ApplyDefaults() {
	if c.Provider == "" {
		c.Provider = ProviderGlesys
	}
	if c.Memory == "" {
		c.Memory = glesysDefaultMemory
	}
	if c.CPUs == "" {
		c.CPUs = glesysDefaultCPUs
	}
	applyCloudDefaults(c, &c.CommonConfig)
}

// Validate checks the cloud rules (validateCloud) and the
// GleSYS-specific invariants.
func (c *GlesysConfig) Validate() error {
	if err := c.validateCloud(ProviderGlesys); err != nil {
		return err
	}
	// KVM is the only platform that accepts a cloudconfig, and
	// cloudconfig is how k3s gets installed. Anything else boots a
	// server this provisioner cannot finish provisioning.
	if c.Platform != "KVM" {
		return errInvalid("platform %q is not supported; glesys provisioning requires KVM (the platform that accepts a cloudconfig)", c.Platform)
	}
	// Memory and CPUs are strings on CommonConfig because qemu and
	// docker pass them through as strings. The GleSYS API takes
	// integers, so a non-numeric value has to fail here rather
	// than at server/create.
	if _, err := positiveInt(c.Memory); err != nil {
		return errInvalid("memory %q must be a positive whole number of MB", c.Memory)
	}
	if _, err := positiveInt(c.CPUs); err != nil {
		return errInvalid("cpus %q must be a positive whole number of cores", c.CPUs)
	}
	if _, err := DiskSizeGB(c.ServerDisk); err != nil {
		return errInvalid("serverDisk %q: %v", c.ServerDisk, err)
	}
	switch c.K3s.Install {
	case "", "airgap", "script":
	default:
		return errInvalid("k3s.install must be one of {airgap, script}, got %q", c.K3s.Install)
	}
	return nil
}
