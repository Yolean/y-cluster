package multipass

import (
	"context"
	"fmt"
	"time"

	"go.uber.org/zap"

	"github.com/Yolean/y-cluster/pkg/multipassexec"
	"github.com/Yolean/y-cluster/pkg/provision/k3s"
)

// k3sReadyTimeout caps the wait for the apiserver after the install.
const k3sReadyTimeout = 3 * time.Minute

// installK3s installs k3s in the VM with the strategy in
// c.cfg.K3s.Install and returns once the apiserver is ready. Config
// defaults to "script" here: multipass VMs have outbound HTTPS
// through the host, which makes the installer's own download faster
// than copying the airgap artifacts in.
func (c *Cluster) installK3s(ctx context.Context) error {
	if c.vmIP == "" {
		return fmt.Errorf("VM IP not resolved before k3s install")
	}
	switch c.cfg.K3s.Install {
	case "", "script":
		c.logger.Info("installing k3s (script)", zap.String("version", c.cfg.K3s.Version))
		if err := k3s.InstallScript(ctx, c.NodeExec, c.cfg.K3s.Version, c.k3sServerFlags()); err != nil {
			return err
		}
	case "airgap":
		c.logger.Info("installing k3s (airgap)", zap.String("version", c.cfg.K3s.Version))
		if err := k3s.InstallAirgap(ctx, c.NodeExec, c.transfer, c.cfg.K3s.Version, c.k3sServerFlags(), c.logger); err != nil {
			return err
		}
	default:
		return fmt.Errorf("unknown k3s.install %q (want script or airgap)", c.cfg.K3s.Install)
	}
	c.logger.Info("waiting for the k3s apiserver")
	return k3s.WaitReady(ctx, c.NodeExec, k3sReadyTimeout)
}

// k3sServerFlags adds the VM's address as a SAN: the host dials
// https://<vm-ip>:6443 directly, and k3s would not put an address
// the hypervisor assigned after first boot in its serving cert.
func (c *Cluster) k3sServerFlags() string {
	return k3s.ServerFlags("--tls-san=" + c.vmIP)
}

func (c *Cluster) transfer(ctx context.Context, hostPath, nodePath string) error {
	return multipassexec.Transfer(ctx, c.cfg.Name, hostPath, nodePath)
}
