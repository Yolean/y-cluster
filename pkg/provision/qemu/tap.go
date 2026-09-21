package qemu

import (
	"crypto/sha256"
	"fmt"
	"net"
	"strings"

	"github.com/Yolean/y-cluster/pkg/provision/config"
)

// TapNetwork is the launch-relevant state of network.mode tap: the
// VM is attached to a tap device the host operator created, and has a
// static address on a host-only subnet the host routes.
type TapNetwork struct {
	Ifname string `json:"ifname"`
	// GuestAddress is the guest's address in CIDR form.
	GuestAddress string   `json:"guestAddress"`
	Gateway      string   `json:"gateway"`
	DNS          []string `json:"dns"`
	// MAC is recorded rather than re-derived on start: the guest's
	// netplan matches the NIC by it, so a change in the derivation
	// must not reach a VM that already booted.
	MAC string `json:"mac"`
}

func tapFromConfig(c *config.QEMUConfig) *TapNetwork {
	if c.Network.Mode != config.QEMUNetworkModeTap {
		return nil
	}
	return &TapNetwork{
		Ifname:       c.Network.Ifname,
		GuestAddress: c.Network.GuestAddress,
		Gateway:      c.Network.Gateway,
		DNS:          append([]string(nil), c.Network.DNS...),
		MAC:          macForName(c.Name),
	}
}

// guestIP is GuestAddress without the prefix length.
func (t TapNetwork) guestIP() string {
	ip, _, err := net.ParseCIDR(t.GuestAddress)
	if err != nil {
		return ""
	}
	return ip.String()
}

// macForName derives the NIC's MAC address from the cluster name.
// qemu's default MAC is the same for every VM, which breaks as soon
// as two VMs share a host. 52:54:00 is the locally administered
// prefix qemu/KVM tooling conventionally uses.
func macForName(name string) string {
	sum := sha256.Sum256([]byte(name))
	return fmt.Sprintf("52:54:00:%02x:%02x:%02x", sum[0], sum[1], sum[2])
}

// renderNetworkConfig builds the NoCloud network-config (version 2)
// that gives the guest its static address. The NIC is matched by MAC
// because its name inside the guest is not ours to choose, and is not
// renamed: prepare-export replaces this netplan with one matching
// kernel-named e* interfaces.
func renderNetworkConfig(t TapNetwork) string {
	var b strings.Builder
	b.WriteString("version: 2\n")
	b.WriteString("ethernets:\n")
	b.WriteString("  guest-nic:\n")
	b.WriteString("    match:\n")
	fmt.Fprintf(&b, "      macaddress: %q\n", t.MAC)
	b.WriteString("    dhcp4: false\n")
	b.WriteString("    addresses:\n")
	fmt.Fprintf(&b, "      - %s\n", t.GuestAddress)
	b.WriteString("    routes:\n")
	b.WriteString("      - to: default\n")
	fmt.Fprintf(&b, "        via: %s\n", t.Gateway)
	b.WriteString("    nameservers:\n")
	b.WriteString("      addresses:\n")
	for _, d := range t.DNS {
		fmt.Fprintf(&b, "        - %s\n", d)
	}
	return b.String()
}

// HostSetupCommands are the commands a host operator runs once to
// prepare the tap device. y-cluster prints them and never runs them:
// it stays unprivileged and does not touch host network devices.
func (t TapNetwork) HostSetupCommands(user string) []string {
	prefix := "24"
	if _, subnet, err := net.ParseCIDR(t.GuestAddress); err == nil {
		ones, _ := subnet.Mask.Size()
		prefix = fmt.Sprint(ones)
	}
	return []string{
		fmt.Sprintf("sudo ip tuntap add dev %s mode tap user %s", t.Ifname, user),
		fmt.Sprintf("sudo ip addr add %s/%s dev %s", t.Gateway, prefix, t.Ifname),
		fmt.Sprintf("sudo ip link set %s up", t.Ifname),
		"sudo sysctl -w net.ipv4.ip_forward=1",
	}
}
