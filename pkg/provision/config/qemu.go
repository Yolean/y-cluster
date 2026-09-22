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

// Network modes.
//
// user is qemu user-mode (slirp) networking: no host setup and no
// privilege, every guest port reached through a host port forward.
// The guest sees all traffic as coming from 10.0.2.2.
//
// tap attaches the VM to a tap device the host operator created
// beforehand. The guest has an address of its own on a host-only
// subnet that the host routes, so workloads see real client
// addresses. y-cluster never creates or configures host network
// devices.
const (
	QEMUNetworkModeUser = "user"
	QEMUNetworkModeTap  = "tap"
)

// QEMUNetwork configures how the VM is attached to the host network.
type QEMUNetwork struct {
	Mode string `yaml:"mode,omitempty" json:"mode,omitempty" jsonschema:"default=user,enum=user,enum=tap,description=Network attachment. user: qemu user-mode networking with host port forwards; needs no host setup. tap: attach to a pre-created tap device; the guest gets its own address and workloads see real client addresses."`

	// BindAddress applies to every forward including SSH. The
	// default is loopback because a forward is otherwise reachable
	// from every network the host is on: on a host with a public
	// address that publishes the VM's SSH and the k3s API to the
	// internet. No tag default: it is only meaningful in user mode,
	// see ApplyDefaults.
	BindAddress string `yaml:"bindAddress,omitempty" json:"bindAddress,omitempty" jsonschema:"description=User mode only. IPv4 address the host port forwards (sshPort and portForwards) listen on. Default 127.0.0.1. 0.0.0.0 exposes them on every interface of the host."`

	Ifname       string   `yaml:"ifname,omitempty" json:"ifname,omitempty" jsonschema:"description=Tap mode only; required. Name of a tap device that exists and is owned by the invoking user. One per VM."`
	GuestAddress string   `yaml:"guestAddress,omitempty" json:"guestAddress,omitempty" jsonschema:"description=Tap mode only; required. Static IPv4 address of the guest in CIDR form such as 10.88.0.2/24."`
	Gateway      string   `yaml:"gateway,omitempty" json:"gateway,omitempty" jsonschema:"description=Tap mode only. The host's address on the tap device and the guest's default route. Default: first host address of the guestAddress subnet."`
	DNS          []string `yaml:"dns,omitempty" json:"dns,omitempty" jsonschema:"description=Tap mode only. Nameservers for the guest. Default 1.1.1.1 and 9.9.9.9; the host is not assumed to run a resolver."`
}

// QEMUNetworkDefaultBindAddress and QEMUNetworkDefaultDNS are applied
// in code because they depend on the mode.
const QEMUNetworkDefaultBindAddress = "127.0.0.1"

var QEMUNetworkDefaultDNS = []string{"1.1.1.1", "9.9.9.9"}

// GuestIP is the guest's address without the prefix length, or empty
// when GuestAddress is not a valid CIDR.
func (n QEMUNetwork) GuestIP() string {
	ip, _, err := net.ParseCIDR(n.GuestAddress)
	if err != nil {
		return ""
	}
	return ip.String()
}

// firstHostAddress is the lowest usable address of the subnet.
func firstHostAddress(subnet *net.IPNet) string {
	ip := subnet.IP.To4()
	if ip == nil {
		return ""
	}
	first := net.IPv4(ip[0], ip[1], ip[2], ip[3]+1)
	return first.String()
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
	// In tap mode there are no host port forwards. sshPort and
	// portForwards must not pick up their defaults, or Validate
	// could not tell a default from a value the operator wrote.
	tap := c.Network.Mode == QEMUNetworkModeTap
	sshPort, portForwards := c.SSHPort, c.PortForwards

	applyTagDefaults(c)
	c.applyCommonDefaults()

	if tap {
		c.SSHPort, c.PortForwards = sshPort, portForwards
		if c.Network.Gateway == "" {
			if _, subnet, err := net.ParseCIDR(c.Network.GuestAddress); err == nil {
				c.Network.Gateway = firstHostAddress(subnet)
			}
		}
		if len(c.Network.DNS) == 0 {
			c.Network.DNS = append([]string(nil), QEMUNetworkDefaultDNS...)
		}
		return
	}
	if c.Network.BindAddress == "" {
		c.Network.BindAddress = QEMUNetworkDefaultBindAddress
	}
}

// Validate checks the discriminator and qemu-specific invariants.
func (c *QEMUConfig) Validate() error {
	if err := c.validateCommon(ProviderQEMU); err != nil {
		return err
	}
	switch c.Network.Mode {
	case QEMUNetworkModeUser:
		if err := c.validateUserNetwork(); err != nil {
			return err
		}
	case QEMUNetworkModeTap:
		if err := c.validateTapNetwork(); err != nil {
			return err
		}
	default:
		return errInvalid("network.mode must be %q or %q, got %q", QEMUNetworkModeUser, QEMUNetworkModeTap, c.Network.Mode)
	}
	switch c.K3s.Install {
	case "", "airgap", "script":
	default:
		return errInvalid("k3s.install must be one of {airgap, script}, got %q", c.K3s.Install)
	}
	return nil
}

func isIPv4Literal(s string) bool {
	ip := net.ParseIP(s)
	return ip != nil && ip.To4() != nil && !strings.Contains(s, ":")
}

func (c *QEMUConfig) validateUserNetwork() error {
	if err := c.requireHostAPIPort(); err != nil {
		return err
	}
	if c.SSHPort == "" {
		return errInvalid("sshPort must not be empty after defaults")
	}
	if !isIPv4Literal(c.Network.BindAddress) {
		return errInvalid("network.bindAddress must be an IPv4 address such as 127.0.0.1 or 0.0.0.0, got %q", c.Network.BindAddress)
	}
	n := c.Network
	if n.Ifname != "" || n.GuestAddress != "" || n.Gateway != "" || len(n.DNS) > 0 {
		return errInvalid("network.ifname, guestAddress, gateway and dns only apply to network.mode %q", QEMUNetworkModeTap)
	}
	return nil
}

func (c *QEMUConfig) validateTapNetwork() error {
	n := c.Network
	if c.SSHPort != "" || len(c.PortForwards) > 0 {
		return errInvalid("sshPort and portForwards have no meaning in network.mode %q: the guest is reached directly at network.guestAddress", QEMUNetworkModeTap)
	}
	if n.BindAddress != "" {
		return errInvalid("network.bindAddress only applies to network.mode %q", QEMUNetworkModeUser)
	}
	if n.Ifname == "" {
		return errInvalid("network.ifname is required in network.mode %q", QEMUNetworkModeTap)
	}
	// IFNAMSIZ is 16 including the terminating NUL.
	if len(n.Ifname) > 15 || strings.ContainsAny(n.Ifname, "/ \t,=") {
		return errInvalid("network.ifname %q is not a valid interface name", n.Ifname)
	}
	guest, subnet, err := net.ParseCIDR(n.GuestAddress)
	if err != nil || guest.To4() == nil {
		return errInvalid("network.guestAddress must be an IPv4 address in CIDR form such as 10.88.0.2/24, got %q", n.GuestAddress)
	}
	if ones, _ := subnet.Mask.Size(); ones > 30 {
		return errInvalid("network.guestAddress %q leaves no room for a gateway; use a prefix of /30 or shorter", n.GuestAddress)
	}
	if !isIPv4Literal(n.Gateway) {
		return errInvalid("network.gateway must be an IPv4 address, got %q", n.Gateway)
	}
	gw := net.ParseIP(n.Gateway)
	if !subnet.Contains(gw) {
		return errInvalid("network.gateway %s is outside the guestAddress subnet %s", n.Gateway, subnet)
	}
	if gw.Equal(guest) {
		return errInvalid("network.gateway and network.guestAddress are both %s", n.Gateway)
	}
	for _, d := range n.DNS {
		if !isIPv4Literal(d) {
			return errInvalid("network.dns entries must be IPv4 addresses, got %q", d)
		}
	}
	return nil
}
