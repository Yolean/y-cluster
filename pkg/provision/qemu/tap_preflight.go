package qemu

import (
	"fmt"
	"net"
	"os"
	"os/user"
	"path/filepath"
	"strconv"
	"strings"
)

// iffTap is IFF_TAP from <linux/if_tun.h>: the device carries
// ethernet frames (a tun device, IFF_TUN, carries IP packets and
// cannot back a VM NIC).
const iffTap = 0x0002

// tapHost is what checkTap asks the host. The real implementation
// reads /sys and the interface table; tests substitute a fake.
type tapHost interface {
	// Addrs returns the addresses configured on the interface, or an
	// error when the interface does not exist.
	Addrs(ifname string) ([]net.IP, error)
	// TunFlags returns the tun/tap flags, or an error when the
	// interface is not a tun/tap device at all.
	TunFlags(ifname string) (uint64, error)
	// Owner returns the uid allowed to attach to the device, or -1
	// when it has no owner (root only).
	Owner(ifname string) (int, error)
	UID() int
	Username() string
}

// checkTap verifies the host is prepared for network.mode tap:
// the device exists, is a tap device, may be opened by this user
// (qemu fails otherwise, and as a daemonized process its error is
// easy to miss), and carries the gateway address the guest will
// route through. y-cluster never creates or configures the device;
// every failure ends with the commands that do.
func checkTap(t TapNetwork, h tapHost) error {
	fail := func(format string, a ...any) error {
		return fmt.Errorf("network.mode tap: "+format+"\nPrepare the host once (y-cluster never changes host network devices):\n  %s",
			append(a, strings.Join(t.HostSetupCommands(h.Username()), "\n  "))...)
	}
	addrs, err := h.Addrs(t.Ifname)
	if err != nil {
		return fail("interface %s does not exist", t.Ifname)
	}
	flags, err := h.TunFlags(t.Ifname)
	if err != nil {
		return fail("interface %s is not a tun/tap device", t.Ifname)
	}
	if flags&iffTap == 0 {
		return fail("interface %s is a tun device; a VM NIC needs mode tap", t.Ifname)
	}
	owner, err := h.Owner(t.Ifname)
	if err != nil {
		return fail("cannot read the owner of %s: %v", t.Ifname, err)
	}
	if owner != h.UID() && h.UID() != 0 {
		return fail("interface %s is owned by uid %d, not by you (uid %d), so qemu cannot attach to it", t.Ifname, owner, h.UID())
	}
	gw := net.ParseIP(t.Gateway)
	for _, a := range addrs {
		if a.Equal(gw) {
			return nil
		}
	}
	return fail("interface %s does not have the gateway address %s (it has %v)", t.Ifname, t.Gateway, addrs)
}

// sysTapHost is the real tapHost.
type sysTapHost struct{}

func (sysTapHost) Addrs(ifname string) ([]net.IP, error) {
	iface, err := net.InterfaceByName(ifname)
	if err != nil {
		return nil, err
	}
	addrs, err := iface.Addrs()
	if err != nil {
		return nil, err
	}
	var ips []net.IP
	for _, a := range addrs {
		if ipnet, ok := a.(*net.IPNet); ok {
			ips = append(ips, ipnet.IP)
		}
	}
	return ips, nil
}

func sysNetValue(ifname, file string) (string, error) {
	data, err := os.ReadFile(filepath.Join("/sys/class/net", ifname, file))
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(data)), nil
}

func (sysTapHost) TunFlags(ifname string) (uint64, error) {
	v, err := sysNetValue(ifname, "tun_flags")
	if err != nil {
		return 0, err
	}
	return strconv.ParseUint(v, 0, 64)
}

func (sysTapHost) Owner(ifname string) (int, error) {
	v, err := sysNetValue(ifname, "owner")
	if err != nil {
		return 0, err
	}
	return strconv.Atoi(v)
}

func (sysTapHost) UID() int { return os.Getuid() }

func (sysTapHost) Username() string {
	if u, err := user.Current(); err == nil {
		return u.Username
	}
	return "$USER"
}
