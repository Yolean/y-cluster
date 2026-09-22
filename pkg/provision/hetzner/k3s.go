package hetzner

import (
	"context"
	"fmt"
	"time"

	"go.uber.org/zap"

	"github.com/Yolean/y-cluster/pkg/kubeconfig"
	"github.com/Yolean/y-cluster/pkg/provision/k3s"
)

// k3sReadyTimeout caps the wait for the apiserver after the install.
// Longer than the local provisioners': the script install pulls the
// k3s system images from upstream registries on first boot.
const k3sReadyTimeout = 5 * time.Minute

// installK3s runs the upstream installer over SSH and returns once
// the apiserver is ready. On top of the flags every provisioner
// passes:
//
//   - --tls-san=<public-ipv4>: the operator's kubectl dials the public
//     address, which k3s would not put in its serving cert by itself.
//   - --node-external-ip=<public-ipv4>: cluster-side consumers (Envoy
//     Gateway, Services with an external IP) advertise the public
//     address rather than the private one the node sees on its NIC.
//
// The script install is the only one this provider has; config
// validation refuses k3s.install: airgap. The server has outbound
// internet, which is what qemu's airgap path exists to avoid
// depending on.
func (c *Cluster) installK3s(ctx context.Context) error {
	c.logger.Info("installing k3s (script)",
		zap.String("version", c.cfg.K3s.Version),
		zap.String("ipv4", c.state.IPv4),
	)
	flags := k3s.ServerFlags("--tls-san="+c.state.IPv4, "--node-external-ip="+c.state.IPv4)
	if err := k3s.InstallScript(ctx, c.NodeExec, c.cfg.K3s.Version, flags); err != nil {
		return err
	}
	c.logger.Info("waiting for the k3s apiserver", zap.Duration("timeout", k3sReadyTimeout))
	return k3s.WaitReady(ctx, c.NodeExec, k3sReadyTimeout)
}

// extractKubeconfig reads the k3s kubeconfig off the node, pointed at
// the server's public IPv4.
//
// Note on security: that kubeconfig dials 6443 directly, so the
// server's API port has to be reachable from the operator's host.
// Hetzner Cloud servers are open to the public internet by default,
// and y-cluster configures no Hetzner Cloud Firewall. k3s's
// bearer-token auth keeps an open 6443 from being a free API for
// anyone who finds the IP; pinning 6443 to known source IPs is left
// to the operator.
func (c *Cluster) extractKubeconfig(ctx context.Context) ([]byte, error) {
	return k3s.ReadKubeconfig(ctx, c.NodeExec, c.state.IPv4+":6443")
}

// MergeKubeconfig writes the cluster's kubeconfig into the
// operator's merged file under the configured context. Wraps
// extractKubeconfig + kubeconfig.Manager so Provision callers get
// a single entry point.
func (c *Cluster) MergeKubeconfig(ctx context.Context) error {
	raw, err := c.extractKubeconfig(ctx)
	if err != nil {
		return err
	}
	mgr, err := kubeconfig.FromEnv(c.cfg.Context, c.cfg.Context, c.logger)
	if err != nil {
		return fmt.Errorf("kubeconfig manager: %w", err)
	}
	if err := mgr.Import(raw); err != nil {
		return fmt.Errorf("merge kubeconfig: %w", err)
	}
	c.logger.Info("kubeconfig merged",
		zap.String("context", c.cfg.Context),
		zap.String("server", "https://"+c.state.IPv4+":6443"),
	)
	return nil
}
