package config

import "net"

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

	// Workers is the number of worker servers beside the control
	// plane. Zero is a single server that is the control plane and
	// runs the workloads, the dev shape. One or more gives a
	// dedicated control plane that schedules nothing, sized by
	// ControlPlane, and workers sized by Memory and CPUs that carry
	// the workloads and the gateway. Envoy then runs on every
	// worker, and every worker's public address serves 80 and 443.
	Workers int `yaml:"workers,omitempty" json:"workers,omitempty" jsonschema:"description=Worker servers beside the control plane. 0 (default) is one server doing both; 1 or more gives a dedicated control plane sized by controlPlane and workers sized by memory and cpus."`

	// ControlPlane sizes the dedicated control-plane server when
	// Workers > 0. Two cores and four gigabytes: Talos logs a
	// recommended memory size of 3946 MiB at boot, and on a
	// two-gigabyte server it removed the kube-apiserver static pod
	// for a minute each time a large CRD set was applied (seen with
	// the Envoy Gateway install, 2026-09-23). Nothing else runs
	// there, so two cores are enough.
	ControlPlane GlesysControlPlane `yaml:"controlPlane,omitempty" json:"controlPlane,omitempty" jsonschema:"description=Sizing of the dedicated control-plane server, used when workers > 0. Defaults 4096 MB (Talos recommends 3946 MiB) and 2 cores."`

	// CNI selects the pod network. cilium (default) encrypts pod
	// traffic between nodes with WireGuard and enforces
	// NetworkPolicy, which flannel, Talos's built-in, does neither
	// of. flannel is the escape hatch for a single node where the
	// install time matters more.
	CNI string `yaml:"cni,omitempty" json:"cni,omitempty" jsonschema:"enum=cilium,enum=flannel,default=cilium,description=Pod network. cilium (default) gives WireGuard-encrypted node-to-node pod traffic and NetworkPolicy enforcement; flannel is Talos's built-in with neither."`

	// APIAllowedCIDRs turns on the Talos ingress firewall. Every
	// node then blocks what is not explicitly allowed: the Talos
	// and Kubernetes API ports (50000, 50001, 6443) accept from the
	// cluster's own addresses and from these CIDRs only, the
	// cluster's internal ports from the cluster's addresses only,
	// and 80 and 443 on the nodes that serve ingress from anywhere.
	// Empty leaves the firewall off, the dev default; the APIs are
	// still mTLS-only, but open to the internet. Put the operator's
	// address here, as a /32, for a cluster that holds anything.
	APIAllowedCIDRs []string `yaml:"apiAllowedCIDRs,omitempty" json:"apiAllowedCIDRs,omitempty" jsonschema:"description=Source CIDRs allowed to reach the Talos and Kubernetes APIs (50000, 50001, 6443). Setting it enables the Talos ingress firewall in default-block mode on every node; empty (default) leaves the firewall off."`

	// Dir is filled at load time from the absolute path of the
	// directory the config came from. Not part of the schema.
	Dir string `yaml:"-" json:"-" jsonschema:"-"`
}

// GlesysControlPlane sizes the dedicated control-plane server.
type GlesysControlPlane struct {
	Memory string `yaml:"memory,omitempty" json:"memory,omitempty" jsonschema:"default=4096,description=Memory in MB for the dedicated control plane. Talos recommends 3946 MiB; 2048 loses the apiserver under load."`
	CPUs   string `yaml:"cpus,omitempty" json:"cpus,omitempty" jsonschema:"default=2,description=vCPU count for the dedicated control plane."`
}

// Nodes is the number of servers the cluster has.
func (c *GlesysConfig) Nodes() int { return 1 + c.Workers }

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
	if c.Workers < 0 {
		return errInvalid("workers %d must be zero or more", c.Workers)
	}
	if c.Workers > 0 {
		if _, err := positiveInt(c.ControlPlane.Memory); err != nil {
			return errInvalid("controlPlane.memory %q must be a positive whole number of MB", c.ControlPlane.Memory)
		}
		if _, err := positiveInt(c.ControlPlane.CPUs); err != nil {
			return errInvalid("controlPlane.cpus %q must be a positive whole number of cores", c.ControlPlane.CPUs)
		}
	}
	if c.CNI != "cilium" && c.CNI != "flannel" {
		return errInvalid("cni %q is not supported on glesys; cilium or flannel", c.CNI)
	}
	for _, cidr := range c.APIAllowedCIDRs {
		if _, _, err := net.ParseCIDR(cidr); err != nil {
			return errInvalid("apiAllowedCIDRs entry %q is not a CIDR (write a single address as a /32)", cidr)
		}
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
