package multipass

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"go.uber.org/zap"

	"github.com/Yolean/y-cluster/pkg/cache"
)

// k3sReadyTimeout caps how long Provision waits for k3s to write
// /etc/rancher/k3s/k3s.yaml after the install script completes.
const k3sReadyTimeout = 3 * time.Minute

// installK3s installs k3s inside the VM using the strategy named in
// c.cfg.K3s.Install. After this returns, /etc/rancher/k3s/k3s.yaml
// is on disk and the apiserver is up. Default is "script": multipass
// VMs have outbound HTTPS through the host, so the curl|sh path is
// faster than the airgap dance.
func (c *Cluster) installK3s(ctx context.Context) error {
	if c.cfg.K3s.Version == "" {
		return fmt.Errorf("k3s.version is empty; pkg/provision/config sets a pin-driven default")
	}
	if c.vmIP == "" {
		return fmt.Errorf("VM IP not resolved before k3s install")
	}
	switch c.cfg.K3s.Install {
	case "", "script":
		c.logger.Info("installing k3s (script)", zap.String("version", c.cfg.K3s.Version))
		if err := c.installK3sScript(ctx); err != nil {
			return err
		}
	case "airgap":
		c.logger.Info("installing k3s (airgap)", zap.String("version", c.cfg.K3s.Version))
		if err := c.installK3sAirgap(ctx); err != nil {
			return err
		}
	default:
		return fmt.Errorf("unknown k3s.install %q (want script or airgap)", c.cfg.K3s.Install)
	}
	return c.waitForK3sReady(ctx)
}

// installK3sScript pipes the canonical get.k3s.io installer into
// `multipass exec`. INSTALL_K3S_EXEC carries:
//
//   - --tls-san=<vm-ip>: the apiserver cert needs the VM's IP in its
//     SAN list because the host dials https://<vm-ip>:6443 directly.
//   - --write-kubeconfig-mode=644: extract requires a non-root
//     reader; k3s defaults to 0600 owned by root.
//   - --disable=traefik: y-cluster ships envoy-gateway as the
//     ingress controller. Traefik would otherwise compete on
//     :80/:443 inside the VM.
func (c *Cluster) installK3sScript(ctx context.Context) error {
	cmd := fmt.Sprintf(
		"curl -sfL https://get.k3s.io | INSTALL_K3S_VERSION=%s INSTALL_K3S_EXEC=%s sudo -E sh -",
		shellQuote(c.cfg.K3s.Version),
		shellQuote(c.k3sExecArgs()),
	)
	out, err := c.NodeExec(ctx, cmd, nil)
	if err != nil {
		return fmt.Errorf("k3s install script: %s: %w", out, err)
	}
	return nil
}

// installK3sAirgap pre-stages the k3s binary and image tarball in
// the VM, then runs the installer with INSTALL_K3S_SKIP_DOWNLOAD=true.
// Re-uses pkg/cache.K3s so a developer running multiple provisioners
// only downloads once.
func (c *Cluster) installK3sAirgap(ctx context.Context) error {
	binPath, tarPath, err := c.cacheK3sAirgap(ctx, c.cfg.K3s.Version)
	if err != nil {
		return err
	}
	if err := multipassTransfer(ctx, c.cfg.Name, binPath, "/tmp/k3s"); err != nil {
		return fmt.Errorf("transfer k3s binary: %w", err)
	}
	if err := multipassTransfer(ctx, c.cfg.Name, tarPath, "/tmp/k3s-airgap.tar.zst"); err != nil {
		return fmt.Errorf("transfer airgap tarball: %w", err)
	}
	for _, step := range []string{
		"sudo install -m 755 /tmp/k3s /usr/local/bin/k3s",
		"sudo mkdir -p /var/lib/rancher/k3s/agent/images",
		"sudo mv /tmp/k3s-airgap.tar.zst /var/lib/rancher/k3s/agent/images/k3s-airgap-images-amd64.tar.zst",
		fmt.Sprintf(
			"curl -sfL https://get.k3s.io | INSTALL_K3S_VERSION=%s INSTALL_K3S_SKIP_DOWNLOAD=true INSTALL_K3S_EXEC=%s sudo -E sh -",
			shellQuote(c.cfg.K3s.Version),
			shellQuote(c.k3sExecArgs()),
		),
	} {
		out, err := c.NodeExec(ctx, step, nil)
		if err != nil {
			return fmt.Errorf("airgap step %q: %s: %w", step, out, err)
		}
	}
	return nil
}

// k3sExecArgs is the INSTALL_K3S_EXEC value shared by both install
// strategies. tls-san pinning is what makes the host's kubectl
// trust the cert when it dials https://<vm-ip>:6443 instead of the
// in-VM 127.0.0.1.
func (c *Cluster) k3sExecArgs() string {
	return "--write-kubeconfig-mode=644 --disable=traefik --tls-san=" + c.vmIP
}

// cacheK3sAirgap downloads (or reuses cached) k3s binary + airgap
// tarball under <cache.K3s>/<version>/. Same shape as the qemu
// provisioner's cache.
func (c *Cluster) cacheK3sAirgap(ctx context.Context, version string) (string, string, error) {
	root, err := cache.K3s("")
	if err != nil {
		return "", "", fmt.Errorf("resolve k3s cache: %w", err)
	}
	dir := filepath.Join(root, version)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", "", fmt.Errorf("create airgap cache: %w", err)
	}
	binPath := filepath.Join(dir, "k3s")
	tarPath := filepath.Join(dir, "k3s-airgap-images-amd64.tar.zst")
	base := fmt.Sprintf("https://github.com/k3s-io/k3s/releases/download/%s", urlEncodeK3sVersion(version))

	if _, err := os.Stat(binPath); err != nil {
		c.logger.Info("downloading k3s binary", zap.String("version", version))
		if err := downloadFile(ctx, base+"/k3s", binPath); err != nil {
			return "", "", fmt.Errorf("download k3s binary: %w", err)
		}
	}
	if _, err := os.Stat(tarPath); err != nil {
		c.logger.Info("downloading k3s airgap images", zap.String("version", version))
		if err := downloadFile(ctx, base+"/k3s-airgap-images-amd64.tar.zst", tarPath); err != nil {
			return "", "", fmt.Errorf("download airgap tarball: %w", err)
		}
	}
	return binPath, tarPath, nil
}

// waitForK3sReady polls /etc/rancher/k3s/k3s.yaml until it exists
// with non-empty contents. k3s's install script returns before the
// apiserver finishes starting; we gate kubeconfig extraction here.
func (c *Cluster) waitForK3sReady(ctx context.Context) error {
	c.logger.Info("waiting for k3s.yaml")
	deadline := time.Now().Add(k3sReadyTimeout)
	for {
		out, err := c.NodeExec(ctx, "sudo test -s /etc/rancher/k3s/k3s.yaml && echo ok", nil)
		if err == nil && strings.Contains(string(out), "ok") {
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("/etc/rancher/k3s/k3s.yaml never appeared within %s", k3sReadyTimeout)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(2 * time.Second):
		}
	}
}

// urlEncodeK3sVersion percent-encodes the `+` separator so the k3s
// GitHub release URL works for v1.X.Y+k3sN strings.
func urlEncodeK3sVersion(v string) string {
	return strings.ReplaceAll(v, "+", "%2B")
}

// shellQuote wraps an argument in single quotes for safe inclusion
// in a remote shell command. Same trick as qemu.shellQuote.
func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// downloadFile fetches src into dest atomically via a `.tmp` rename.
// Same shape as qemu.downloadFile (kept private here so multipass
// doesn't import the qemu package just for one helper).
func downloadFile(ctx context.Context, src, dest string) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, src, nil)
	if err != nil {
		return err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("GET %s: status %d", src, resp.StatusCode)
	}
	tmp := dest + ".tmp"
	f, err := os.Create(tmp)
	if err != nil {
		return err
	}
	if _, err := io.Copy(f, resp.Body); err != nil {
		_ = f.Close()
		_ = os.Remove(tmp)
		return err
	}
	if err := f.Close(); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	return os.Rename(tmp, dest)
}
