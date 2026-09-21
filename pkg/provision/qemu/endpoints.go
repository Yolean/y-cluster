package qemu

import (
	"bytes"
	"fmt"

	"github.com/Yolean/y-cluster/pkg/sshexec"
)

// guestUser is the login cloud-init creates in the guest.
const guestUser = "ystack"

// loopback is where the host reaches a user-mode (slirp) guest: every
// guest port is only available through a hostfwd on the host.
const loopback = "127.0.0.1"

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

// endpoints derives the host-side addresses from the port forwards.
func (c Config) endpoints() endpoints {
	e := endpoints{SSHHost: loopback, SSHPort: c.SSHPort, APIHost: loopback}
	for _, pf := range c.PortForwards {
		switch pf.Guest {
		case "6443":
			if e.APIPort == "" {
				e.APIPort = pf.Host
			}
		case "80":
			e.IngressIP = loopback
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

// guestAPIServer is the server address k3s writes into its own
// kubeconfig: the apiserver as seen from inside the guest.
const guestAPIServer = "127.0.0.1:6443"

// rewriteKubeconfigServer points a kubeconfig read from the guest at
// the host-side apiserver address. TLS keeps validating because k3s
// lists 127.0.0.1 among its serving cert SANs.
func rewriteKubeconfigServer(raw []byte, e endpoints) ([]byte, error) {
	if e.APIPort == "" {
		return nil, fmt.Errorf("portForwards has no guest:6443 entry; cannot reach k3s API")
	}
	return bytes.ReplaceAll(raw, []byte(guestAPIServer), []byte(e.APIHost+":"+e.APIPort)), nil
}

// netdevArg renders qemu's -netdev value: user-mode networking with
// one hostfwd for ssh and one per configured port forward.
func netdevArg(cfg Config) string {
	netdev := fmt.Sprintf("user,id=%s,hostfwd=tcp::%s-:22", netdevID, cfg.SSHPort)
	for _, pf := range cfg.PortForwards {
		netdev += fmt.Sprintf(",hostfwd=tcp::%s-:%s", pf.Host, pf.Guest)
	}
	return netdev
}

const netdevID = "net0"

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
		"-device", "virtio-net-pci,netdev="+netdevID,
		"-serial", "file:"+consolePath,
		"-display", "none",
		"-daemonize",
		"-pidfile", pidFile,
	)
}
