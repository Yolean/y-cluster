package config

import (
	"net"
	"strings"
)

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

	Network QEMUNetwork `yaml:"network,omitempty" json:"network,omitempty"`

	// Dir is filled at load time from the absolute path of the
	// directory the config came from. Not part of the schema.
	Dir string `yaml:"-" json:"-" jsonschema:"-"`
}

// QEMUNetworkModeUser is qemu user-mode (slirp) networking: no host
// setup and no privilege, every guest port reached through a host
// port forward. The guest sees all traffic as coming from 10.0.2.2.
const QEMUNetworkModeUser = "user"

// QEMUNetwork configures how the VM is attached to the host network.
type QEMUNetwork struct {
	Mode string `yaml:"mode,omitempty" json:"mode,omitempty" jsonschema:"default=user,enum=user,description=Network attachment. user is qemu user-mode networking with host port forwards."`

	// BindAddress applies to every forward including SSH. The
	// default is loopback because a forward is otherwise reachable
	// from every network the host is on: on a host with a public
	// address that publishes the VM's SSH and the k3s API to the
	// internet.
	BindAddress string `yaml:"bindAddress,omitempty" json:"bindAddress,omitempty" jsonschema:"default=127.0.0.1,description=IPv4 address the host port forwards (sshPort and portForwards) listen on. 0.0.0.0 exposes them on every interface of the host."`
}

// HostDialAddress is the address the host itself dials to reach a
// forward bound to bindAddress. The wildcard is not dialable and an
// empty value is a forward from before bindAddress existed, which
// qemu bound to the wildcard; both are reached over loopback.
func HostDialAddress(bindAddress string) string {
	if bindAddress == "" || bindAddress == "0.0.0.0" {
		return "127.0.0.1"
	}
	return bindAddress
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
	if c.Network.Mode != QEMUNetworkModeUser {
		return errInvalid("network.mode must be %q, got %q", QEMUNetworkModeUser, c.Network.Mode)
	}
	if ip := net.ParseIP(c.Network.BindAddress); ip == nil || ip.To4() == nil || strings.Contains(c.Network.BindAddress, ":") {
		return errInvalid("network.bindAddress must be an IPv4 address such as 127.0.0.1 or 0.0.0.0, got %q", c.Network.BindAddress)
	}
	switch c.K3s.Install {
	case "", "airgap", "script":
	default:
		return errInvalid("k3s.install must be one of {airgap, script}, got %q", c.K3s.Install)
	}
	return nil
}
