package qemu

import (
	"bytes"
	"fmt"
	"path/filepath"

	"github.com/Yolean/y-cluster/pkg/provision/config"
	"github.com/Yolean/y-cluster/pkg/sshexec"
)

// guestUser is the login cloud-init creates in the guest.
const guestUser = "ystack"

// endpoints is the single answer to "how does the host reach the
// guest". Everything that dials the guest (ssh, the kubeconfig
// server URL, the DNS hint for ingress) derives from it, so a
// network mode with different reachability changes one function.
type endpoints struct {
	SSHHost string
	SSHPort string
	// APIHost / APIPort address the k3s apiserver. APIPort is empty
	// when no forward to guest 6443 is configured.
	APIHost string
	APIPort string
	// IngressIP is the host-side address of the guest's port 80, or
	// empty when ingress is not reachable from the host.
	IngressIP string
}

// endpoints derives the host-side addresses. A user-mode (slirp)
// guest is only reachable through the host port forwards, so every
// host is the address those forwards can be dialed on. A tap guest is
// reached on its own address.
func (c Config) endpoints() endpoints {
	if c.Tap != nil {
		// The guest has an address of its own; every port is reached
		// on it directly.
		ip := c.Tap.guestIP()
		return endpoints{SSHHost: ip, SSHPort: "22", APIHost: ip, APIPort: "6443", IngressIP: ip}
	}
	host := config.HostDialAddress(c.BindAddress)
	e := endpoints{SSHHost: host, SSHPort: c.SSHPort, APIHost: host}
	for _, pf := range c.PortForwards {
		switch pf.Guest {
		case "6443":
			if e.APIPort == "" {
				e.APIPort = pf.Host
			}
		case "80":
			e.IngressIP = host
		}
	}
	return e
}

// sshTarget is the guest's ssh endpoint for the given private key.
func (e endpoints) sshTarget(keyPath string) sshexec.Target {
	return sshexec.Target{
		Host:    e.SSHHost,
		Port:    e.SSHPort,
		User:    guestUser,
		KeyPath: keyPath,
	}
}

// SSHCommand is the ssh invocation an operator uses to log in to the
// guest.
func (c Config) SSHCommand() string {
	e := c.endpoints()
	return fmt.Sprintf("ssh -p %s -i %s %s@%s", e.SSHPort, filepath.Join(c.CacheDir, c.Name+"-ssh"), guestUser, e.SSHHost)
}

// guestAPIServer is the server address k3s writes into its own
// kubeconfig: the apiserver as seen from inside the guest.
const guestAPIServer = "127.0.0.1:6443"

// rewriteKubeconfigServer points a kubeconfig read from the guest at
// the host-side apiserver address. TLS validates against that
// address because k3s lists 127.0.0.1 among its serving cert SANs by
// default and k3sServerFlags adds any other APIHost.
func rewriteKubeconfigServer(raw []byte, e endpoints) ([]byte, error) {
	if e.APIPort == "" {
		return nil, fmt.Errorf("portForwards has no guest:6443 entry; cannot reach k3s API")
	}
	return bytes.ReplaceAll(raw, []byte(guestAPIServer), []byte(e.APIHost+":"+e.APIPort)), nil
}

// netdevArg renders qemu's -netdev value. User mode: one hostfwd for
// ssh and one per configured port forward, all bound to
// cfg.BindAddress; slirp reads an empty host address as 0.0.0.0. Tap
// mode: the pre-created device.
func netdevArg(cfg Config) string {
	if cfg.Tap != nil {
		// script=no,downscript=no: the device is already configured
		// by the host operator, and qemu's default scripts would
		// need root.
		return fmt.Sprintf("tap,id=%s,ifname=%s,script=no,downscript=no", netdevID, cfg.Tap.Ifname)
	}
	netdev := fmt.Sprintf("user,id=%s,hostfwd=tcp:%s:%s-:22", netdevID, cfg.BindAddress, cfg.SSHPort)
	for _, pf := range cfg.PortForwards {
		netdev += fmt.Sprintf(",hostfwd=tcp:%s:%s-:%s", cfg.BindAddress, pf.Host, pf.Guest)
	}
	return netdev
}

// k3sServerFlags is the INSTALL_K3S_EXEC value.
//
// traefik is disabled because y-cluster ships Envoy Gateway as the
// cluster ingress; two controllers would fight over the :80/:443
// forwards. local-storage is disabled because y-cluster ships its own
// local-path-provisioner (pkg/provision/localstorage) and k3s's deploy
// controller would reconcile that config back to upstream defaults on
// every restart.
//
// The kubeconfig written on the host names the apiserver by APIHost.
// k3s only puts 127.0.0.1 and the node's own addresses in its serving
// cert, so any other host address has to be added as a SAN.
func k3sServerFlags(e endpoints) string {
	flags := "--write-kubeconfig-mode=644 --disable=traefik --disable=local-storage"
	if e.APIHost != "127.0.0.1" {
		flags += " --tls-san=" + e.APIHost
	}
	return flags
}

const netdevID = "net0"

// nicArg renders the -device value for the guest NIC. The MAC is set
// in tap mode only, which keeps user-mode launches identical to what
// they were before tap mode existed.
func nicArg(cfg Config) string {
	nic := "virtio-net-pci,netdev=" + netdevID
	if cfg.Tap != nil {
		nic += ",mac=" + cfg.Tap.MAC
	}
	return nic
}

// vmDisks names the drives of one VM launch, in attach order. The
// order is load-bearing: the guest sees them as vda, vdb, ... and the
// boot disk has to come first.
type vmDisks struct {
	Boot string
	// Seed is the cloud-init NoCloud image; empty on a start after
	// the first boot, when cloud-init has nothing left to do.
	Seed  string
	Extra []string
}

// vmArgs renders the qemu-system-x86_64 argument list.
func vmArgs(cfg Config, disks vmDisks, consolePath, pidFile string) []string {
	args := []string{
		"-name", cfg.Name,
		"-machine", "accel=kvm",
		"-cpu", "host",
		"-smp", cfg.CPUs,
		"-m", cfg.Memory,
		"-drive", fmt.Sprintf("file=%s,format=qcow2,if=virtio", disks.Boot),
	}
	if disks.Seed != "" {
		args = append(args, "-drive", fmt.Sprintf("file=%s,format=raw,if=virtio", disks.Seed))
	}
	for _, d := range disks.Extra {
		args = append(args, "-drive", fmt.Sprintf("file=%s,format=qcow2,if=virtio", d))
	}
	return append(args,
		"-netdev", netdevArg(cfg),
		"-device", nicArg(cfg),
		"-serial", "file:"+consolePath,
		"-display", "none",
		"-daemonize",
		"-pidfile", pidFile,
	)
}
