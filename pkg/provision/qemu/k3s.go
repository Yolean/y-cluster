package qemu

import (
	"context"
	"fmt"
	"time"

	"go.uber.org/zap"

	"github.com/Yolean/y-cluster/pkg/provision/k3s"
)

// k3sReadyTimeout caps the wait for the apiserver after the install
// (or, on start, after boot). The ceiling is for slow hosts and for
// the airgap image import.
const k3sReadyTimeout = 3 * time.Minute

// installK3s installs k3s in the running VM with the strategy in
// c.cfg.K3s.Install and returns once the apiserver is ready.
func (c *Cluster) installK3s(ctx context.Context) error {
	flags := k3sServerFlags(c.cfg.endpoints())
	switch c.cfg.K3s.Install {
	case "", "airgap":
		c.logger.Info("installing k3s (airgap)", zap.String("version", c.cfg.K3s.Version))
		if err := k3s.InstallAirgap(ctx, c.NodeExec, c.SCP, c.cfg.K3s.Version, flags, c.logger); err != nil {
			return err
		}
	case "script":
		c.logger.Info("installing k3s (script)", zap.String("version", c.cfg.K3s.Version))
		if err := k3s.InstallScript(ctx, c.NodeExec, c.cfg.K3s.Version, flags); err != nil {
			return err
		}
	default:
		return fmt.Errorf("unknown k3s.install %q (want airgap or script)", c.cfg.K3s.Install)
	}
	return c.waitForK3sReady(ctx)
}

func (c *Cluster) waitForK3sReady(ctx context.Context) error {
	c.logger.Info("waiting for the k3s apiserver")
	return k3s.WaitReady(ctx, c.NodeExec, k3sReadyTimeout)
}

// extractKubeconfig reads the k3s-generated kubeconfig from the VM,
// rewritten so the host's kubectl reaches the apiserver.
func (c *Cluster) extractKubeconfig(ctx context.Context) ([]byte, error) {
	addr, err := c.cfg.endpoints().apiAddress()
	if err != nil {
		return nil, err
	}
	return k3s.ReadKubeconfig(ctx, c.NodeExec, addr)
}
