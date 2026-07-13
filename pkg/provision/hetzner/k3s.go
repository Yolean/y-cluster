package hetzner

import (
	"bytes"
	"context"
	"fmt"
	"strings"
	"time"

	"go.uber.org/zap"

	"github.com/Yolean/y-cluster/pkg/kubeconfig"
)

// k3sReadyTimeout caps how long Provision waits for k3s to write
// /etc/rancher/k3s/k3s.yaml after the install script completes.
// Same shape as the qemu provisioner; on Hetzner the script-mode
// install can be slower than airgap because it pulls k3s + the
// system images from upstream registries on first boot.
const k3sReadyTimeout = 5 * time.Minute

// installK3s runs the canonical curl|sh installer over SSH, with
// hetzner-specific flags so the kubeconfig is reachable from the
// operator's host:
//
//   - --tls-san=<public-ipv4>         lets kubectl accept the
//                                     certificate when the operator
//                                     dials the public IP.
//   - --node-external-ip=<public-ipv4> pins the node's reported
//                                     external IP to the Hetzner
//                                     public IPv4 so cluster-side
//                                     consumers (envoy gateway,
//                                     services with externalIP)
//                                     advertise the right address.
//   - --disable=traefik               y-cluster ships envoy-gateway
//                                     instead.
//   - --disable=local-storage         y-cluster ships its own
//                                     local-path provisioner.
//
// Phase 1 uses script-mode (curl | sh). Airgap mode -- mirroring
// pkg/provision/qemu's installK3sAirgap, which downloads the k3s
// binary + image tarball locally and SCPs them in -- lands later
// once the dev-cluster experience needs offline-capable installs.
func (c *Cluster) installK3s(ctx context.Context) error {
	if c.cfg.K3s.Version == "" {
		return fmt.Errorf("k3s.version is empty; pkg/provision/config sets a pin-driven default")
	}
	c.logger.Info("installing k3s (script)",
		zap.String("version", c.cfg.K3s.Version),
		zap.String("ipv4", c.state.IPv4),
	)
	cmd := fmt.Sprintf(
		"curl -sfL https://get.k3s.io | INSTALL_K3S_VERSION=%s INSTALL_K3S_EXEC=%s sudo -E sh -",
		shellQuote(c.cfg.K3s.Version),
		shellQuote(strings.Join([]string{
			"--write-kubeconfig-mode=644",
			"--disable=traefik",
			"--disable=local-storage",
			"--tls-san=" + c.state.IPv4,
			"--node-external-ip=" + c.state.IPv4,
		}, " ")),
	)
	out, err := c.SSH(ctx, cmd)
	if err != nil {
		return fmt.Errorf("k3s install: %s: %w", out, err)
	}
	return c.waitForK3sReady(ctx)
}

// waitForK3sReady polls the kubeconfig path on the node until k3s
// writes it. The install script returns as soon as systemd reports
// k3s.service active, but the API isn't actually reachable until
// the kubeconfig file is present; polling the file is the cheapest
// readiness probe.
func (c *Cluster) waitForK3sReady(ctx context.Context) error {
	c.logger.Info("waiting for k3s to write /etc/rancher/k3s/k3s.yaml",
		zap.Duration("timeout", k3sReadyTimeout))
	deadline := time.Now().Add(k3sReadyTimeout)
	for {
		out, err := c.SSH(ctx, "sudo test -f /etc/rancher/k3s/k3s.yaml && echo ready")
		if err == nil && bytes.Contains(out, []byte("ready")) {
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("k3s.yaml never appeared on the node within %s", k3sReadyTimeout)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(5 * time.Second):
		}
	}
}

// extractKubeconfig pulls /etc/rancher/k3s/k3s.yaml off the node
// (as root via sudo) and rewrites the server URL from k3s's local
// 127.0.0.1:6443 default to the Hetzner server's public IPv4. The
// kubeconfig.Manager handles merging into the operator's
// $KUBECONFIG with the configured context name.
//
// Note on security: the rewritten kubeconfig dials 6443 directly,
// which means the Hetzner server's API port has to be reachable
// from the operator's host. Hetzner Cloud servers are open to the
// public internet by default; phase 5 polish should add a Hetzner
// Cloud Firewall pinning 6443 to operator-supplied source IPs.
// k3s's bearer-token auth keeps an open 6443 from being a free
// API for anyone who finds the IP, but a tighter firewall is the
// right belt-and-braces.
func (c *Cluster) extractKubeconfig(ctx context.Context) ([]byte, error) {
	raw, err := c.SSH(ctx, "sudo cat /etc/rancher/k3s/k3s.yaml")
	if err != nil {
		return nil, fmt.Errorf("read k3s.yaml: %s: %w", raw, err)
	}
	rewritten := strings.ReplaceAll(string(raw),
		"server: https://127.0.0.1:6443",
		"server: https://"+c.state.IPv4+":6443")
	return []byte(rewritten), nil
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
	mgr, err := kubeconfig.New(c.cfg.Context, c.cfg.Context, c.logger)
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

// shellQuote wraps a string for safe inclusion in a remote sh -c
// command. Dupe of qemu/exec.go's helper -- breaking the cross-
// package import out into pkg/cluster/shellquote is bigger than
// the duplication is worth right now (4 lines vs an exported
// helper somebody would import).
func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}
