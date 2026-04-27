package qemu

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"go.uber.org/zap"

	"github.com/Yolean/y-cluster/pkg/cache"
)

// k3sReadyTimeout caps how long Provision waits for k3s to write
// /etc/rancher/k3s/k3s.yaml after the install script completes. k3s
// usually writes it within seconds; the longer ceiling guards
// against slow VMs and slow image pulls when running airgap.
const k3sReadyTimeout = 3 * time.Minute

// installK3s SSHes into the running VM and installs k3s using the
// strategy declared in c.cfg.K3s.Install. After this returns,
// /etc/rancher/k3s/k3s.yaml is present and the API server is up.
func (c *Cluster) installK3s(ctx context.Context) error {
	if c.cfg.K3s.Version == "" {
		return fmt.Errorf("k3s.version is empty; pkg/provision/config sets a pin-driven default")
	}
	switch c.cfg.K3s.Install {
	case "", "airgap":
		c.logger.Info("installing k3s (airgap)", zap.String("version", c.cfg.K3s.Version))
		if err := c.installK3sAirgap(ctx); err != nil {
			return err
		}
	case "script":
		c.logger.Info("installing k3s (script)", zap.String("version", c.cfg.K3s.Version))
		if err := c.installK3sScript(ctx); err != nil {
			return err
		}
	default:
		return fmt.Errorf("unknown k3s.install %q (want airgap or script)", c.cfg.K3s.Install)
	}
	return c.waitForK3sReady(ctx)
}

// installK3sScript runs the canonical curl|sh installer with
// INSTALL_K3S_VERSION pinned. The VM must have outbound HTTPS to
// get.k3s.io and github.com release URLs.
//
// `--disable=traefik` is added because y-cluster ships Envoy
// Gateway as the cluster ingress. Running both controllers
// would have two consumers fighting over the host:80/:443
// forwards.
func (c *Cluster) installK3sScript(ctx context.Context) error {
	cmd := fmt.Sprintf(
		"curl -sfL https://get.k3s.io | INSTALL_K3S_VERSION=%s INSTALL_K3S_EXEC=%s sudo -E sh -",
		shellQuote(c.cfg.K3s.Version),
		shellQuote("--write-kubeconfig-mode=644 --disable=traefik"),
	)
	out, err := c.SSH(ctx, cmd)
	if err != nil {
		return fmt.Errorf("k3s install script: %s: %w", out, err)
	}
	return nil
}

// installK3sAirgap pre-stages the k3s binary and image tarball on
// the VM (both downloaded and cached locally first), then runs the
// installer with INSTALL_K3S_SKIP_DOWNLOAD=true. Useful when
// outbound from the VM is rate-limited or unstable -- the host
// downloads once and re-uses the cache across provisions.
func (c *Cluster) installK3sAirgap(ctx context.Context) error {
	binPath, tarPath, err := c.cacheK3sAirgap(ctx, c.cfg.K3s.Version)
	if err != nil {
		return err
	}

	if err := c.SCP(ctx, binPath, "/tmp/k3s"); err != nil {
		return fmt.Errorf("scp k3s binary: %w", err)
	}
	if err := c.SCP(ctx, tarPath, "/tmp/k3s-airgap.tar.zst"); err != nil {
		return fmt.Errorf("scp airgap tarball: %w", err)
	}

	for _, step := range []string{
		"sudo install -m 755 /tmp/k3s /usr/local/bin/k3s",
		"sudo mkdir -p /var/lib/rancher/k3s/agent/images",
		"sudo mv /tmp/k3s-airgap.tar.zst /var/lib/rancher/k3s/agent/images/k3s-airgap-images-amd64.tar.zst",
		fmt.Sprintf(
			"curl -sfL https://get.k3s.io | INSTALL_K3S_VERSION=%s INSTALL_K3S_SKIP_DOWNLOAD=true INSTALL_K3S_EXEC=%s sudo -E sh -",
			shellQuote(c.cfg.K3s.Version),
			shellQuote("--write-kubeconfig-mode=644 --disable=traefik"),
		),
	} {
		out, err := c.SSH(ctx, step)
		if err != nil {
			return fmt.Errorf("airgap step %q: %s: %w", step, out, err)
		}
	}
	return nil
}

// cacheK3sAirgap downloads the k3s binary and airgap image tarball
// for the requested version into y-cluster's shared cache root
// under k3s/<version>/, or returns the cached paths if they
// already exist. Each download writes to a .tmp file and renames
// atomically so a partial download from a previous attempt isn't
// reused.
//
// Lives under cache.K3s (~/.cache/y-cluster/k3s/<version>) rather
// than the per-VM qemu cache so a developer with multiple qemu
// instances on the same k3s pin only downloads once, and
// `y-cluster cache purge --k3s` reaches it.
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

// waitForK3sReady polls /etc/rancher/k3s/k3s.yaml on the VM until it
// exists with non-empty contents. k3s's install script returns
// before the apiserver finishes starting; this is the gate before
// we can extract a working kubeconfig.
func (c *Cluster) waitForK3sReady(ctx context.Context) error {
	c.logger.Info("waiting for k3s.yaml")
	deadline := time.Now().Add(k3sReadyTimeout)
	for {
		out, err := c.SSH(ctx, "sudo test -s /etc/rancher/k3s/k3s.yaml && echo ok")
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

// extractKubeconfig reads the k3s-generated kubeconfig from the VM
// and rewrites the embedded server URL so the host's kubectl can
// reach it through the QEMU port forward.
//
// k3s writes `server: https://127.0.0.1:6443` (the loopback inside
// the VM). From the host, the API server is reachable at
// 127.0.0.1:<host-mapped-port> -- we look that up via
// Config.hostAPIPort and substitute. TLS still works because k3s
// puts 127.0.0.1 in the cert SANs by default.
func (c *Cluster) extractKubeconfig(ctx context.Context) ([]byte, error) {
	out, err := c.SSH(ctx, "sudo cat /etc/rancher/k3s/k3s.yaml")
	if err != nil {
		return nil, fmt.Errorf("read kubeconfig: %s: %w", out, err)
	}
	hostPort := c.cfg.hostAPIPort()
	if hostPort == "" {
		return nil, fmt.Errorf("portForwards has no guest:6443 entry; cannot reach k3s API")
	}
	rewritten := bytes.ReplaceAll(out, []byte("127.0.0.1:6443"), []byte("127.0.0.1:"+hostPort))
	return rewritten, nil
}

// urlEncodeK3sVersion percent-encodes the `+` separator in build
// metadata (e.g. v1.35.4-rc3-k3s1 doesn't need it; v1.35.3+k3s1
// does). The k3s GitHub release URLs require the `+` encoded.
func urlEncodeK3sVersion(v string) string {
	return strings.ReplaceAll(v, "+", "%2B")
}

// shellQuote wraps an argument in single quotes for safe inclusion
// in a remote shell command. Embedded single quotes are escaped
// using the standard `'\''` trick. Used for the values we pass via
// SSH: version strings (-rc3-k3s1) and INSTALL_K3S_EXEC arguments.
func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// downloadFile fetches a URL into dest atomically. Uses a `.tmp`
// suffix so an interrupted download doesn't leave a partial file
// that the cache check would falsely accept on retry.
func downloadFile(ctx context.Context, src, dest string) error {
	if _, err := url.Parse(src); err != nil {
		return fmt.Errorf("parse url: %w", err)
	}
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
