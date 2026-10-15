package qemu

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"go.uber.org/zap"

	"github.com/Yolean/y-cluster/pkg/provision/cilium"
	"github.com/Yolean/y-cluster/pkg/provision/k3s"
	"github.com/Yolean/y-cluster/pkg/provision/platform"
	"github.com/Yolean/y-cluster/pkg/provision/registries"
)

// k3sReadyTimeout caps the wait for the apiserver after the install
// (or, on start, after boot). The ceiling is for slow hosts and for
// the airgap image import.
const k3sReadyTimeout = 3 * time.Minute

// guestArch is the architecture of every qemu guest: the VM runs on
// qemu-system-x86_64.
const guestArch = "amd64"

// K3sBundleSpec is the k3s node bundle of the cluster: the server
// settings for Cilium plus a tls-san for any apiserver address the
// host dials other than 127.0.0.1 (k3s only puts 127.0.0.1 and the
// node's own addresses in its serving cert), then k3s.serverConfig;
// registries.yaml when registries are configured; the portmap link
// Cilium needs as the post-install command.
func (c Config) K3sBundleSpec() (k3s.BundleSpec, error) {
	var sans []string
	if e := c.endpoints(); e.APIHost != "127.0.0.1" {
		sans = append(sans, e.APIHost)
	}
	server, err := k3s.CiliumServerSettings(sans, c.K3s.ServerConfig)
	if err != nil {
		return k3s.BundleSpec{}, err
	}
	reg, err := registries.Marshal(c.Registries)
	if err != nil {
		return k3s.BundleSpec{}, err
	}
	return k3s.BundleSpec{
		Version:     c.K3s.Version,
		Install:     c.K3s.Install,
		Arch:        guestArch,
		Server:      server,
		Registries:  reg,
		PostInstall: []string{cilium.K3sPortmapCommand},
	}, nil
}

// PlatformOptions is what provision hands platform.Install, and what
// `y-cluster platform install` hands it for a qemu config.
func (c Config) PlatformOptions(logger *zap.Logger) platform.Options {
	return platform.Options{
		Context:     c.Context,
		Nodes:       1,
		Storage:     c.Storage,
		Gateway:     c.Gateway,
		CertManager: c.CertManager,
		DNSHintIP:   c.endpoints().IngressIP,
		Logger:      logger,
	}
}

// bundleDir is where provision writes the node bundle. It is removed
// once applied: registries.yaml may carry credentials.
func (c Config) bundleDir() string {
	return filepath.Join(c.CacheDir, c.Name+"-k3s-bundle")
}

// installK3s writes the cluster's node bundle, applies it to the
// running VM and returns once the apiserver is ready.
func (c *Cluster) installK3s(ctx context.Context) error {
	spec, err := c.cfg.K3sBundleSpec()
	if err != nil {
		return err
	}
	dir := c.cfg.bundleDir()
	defer func() { _ = os.RemoveAll(dir) }()
	c.logger.Info("installing k3s", zap.String("version", spec.Version), zap.String("install", spec.Install))
	if _, err := k3s.WriteBundle(ctx, spec, dir, c.logger); err != nil {
		return fmt.Errorf("write node bundle: %w", err)
	}
	if err := k3s.ApplyBundle(ctx, c.NodeExec, c.SCP, dir, c.logger); err != nil {
		return fmt.Errorf("apply node bundle: %w", err)
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

// importKubeconfig reads the k3s kubeconfig off the VM and merges it
// as `y-cluster kubeconfig import` does, pointed at the host-side
// apiserver address.
func (c *Cluster) importKubeconfig(ctx context.Context) error {
	addr, err := c.cfg.endpoints().apiAddress()
	if err != nil {
		return err
	}
	raw, err := c.NodeExec(ctx, "sudo cat "+k3s.KubeconfigPath, nil)
	if err != nil {
		return fmt.Errorf("read %s: %s: %w", k3s.KubeconfigPath, raw, err)
	}
	return k3s.ImportKubeconfig(c.Kubeconfig, raw, addr)
}
