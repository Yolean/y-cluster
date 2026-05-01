// Package multipass provisions a single-node k3s cluster inside a
// Multipass-managed Ubuntu VM. Multipass uses the system's native
// hypervisor (Hyperkit / QEMU+HVF on macOS, QEMU+KVM on Linux) and
// integrates with the host network stack so the VM gets its own
// host-routable IP — no port forwarding, no host loopback tunnels.
//
// macOS is the primary target: the qemu provisioner needs /dev/kvm
// (Linux only) and the docker provisioner trades the real-Linux-kernel
// boundary for speed. Multipass keeps the VM boundary on macOS without
// the cloud-image plumbing the qemu provisioner does by hand.
package multipass

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	"go.uber.org/zap"

	"github.com/Yolean/y-cluster/pkg/kubeconfig"
	"github.com/Yolean/y-cluster/pkg/multipassexec"
	"github.com/Yolean/y-cluster/pkg/provision"
	"github.com/Yolean/y-cluster/pkg/provision/config"
	"github.com/Yolean/y-cluster/pkg/provision/envoygateway"
	"github.com/Yolean/y-cluster/pkg/provision/registries"
)

var _ provision.Cluster = (*Cluster)(nil)

// Config is the runtime VM and cluster shape used by Provision and
// Teardown. The on-disk shape lives in
// github.com/Yolean/y-cluster/pkg/provision/config.MultipassConfig
// and translates here via FromConfig.
type Config struct {
	Name       string
	Image      string
	Memory     string
	CPUs       string
	Context    string
	K3s        K3s
	Registries config.Registries
	Gateway    config.GatewayConfig
}

// diskSize is the multipass --disk argument. Hardcoded rather than
// surfaced in MultipassConfig because the y-cluster baseline (k3s
// + an Ubuntu rootfs + room for image pulls) fits comfortably under
// 20G and no consumer has needed to override it. Promote to a
// MultipassConfig field when there's a real call site that wants to.
//
// Suffix is plain "G" because multipass launch only accepts
// K/M/G/T (no Ki/Mi/Gi/Ti); both multipass and qemu-img treat the
// bare suffix as 2^30, so the value matches what `20Gi` would mean
// in kubernetes-style notation.
const diskSize = "20G"

// K3s mirrors qemu.K3s -- the runtime view of K3sConfig.
type K3s struct {
	Version string
	Install string
}

// FromConfig translates the on-disk MultipassConfig (already
// defaults-applied and validated by configfile.Load) into the
// runtime Config consumed by Provision/Teardown.
func FromConfig(c *config.MultipassConfig) Config {
	return Config{
		Name:    c.Name,
		Image:   c.Image,
		Memory:  c.Memory,
		CPUs:    c.CPUs,
		Context: c.Context,
		K3s: K3s{
			Version: c.K3s.Version,
			Install: c.K3s.Install,
		},
		Registries: c.Registries,
		Gateway:    c.Gateway,
	}
}

// Cluster is the runtime handle for a Multipass-backed k3s cluster.
type Cluster struct {
	cfg        Config
	vmIP       string
	logger     *zap.Logger
	Kubeconfig *kubeconfig.Manager
}

// CheckPrerequisites verifies that the multipass CLI is on PATH and
// the daemon is reachable. We don't probe for /dev/kvm or qemu
// binaries -- multipass bundles its own hypervisor backend.
func CheckPrerequisites() error {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := multipassexec.Version(ctx); err != nil {
		return fmt.Errorf("multipass not available: install Multipass from https://multipass.run: %w", err)
	}
	return nil
}

// Provision launches a Multipass VM, installs k3s, extracts the
// kubeconfig (with the server URL rewritten to the VM IP) and
// merges it into the host's KUBECONFIG.
func Provision(ctx context.Context, cfg Config, logger *zap.Logger) (*Cluster, error) {
	if logger == nil {
		logger = zap.NewNop()
	}
	if err := CheckPrerequisites(); err != nil {
		return nil, err
	}

	kubecfg, err := kubeconfig.New(cfg.Context, cfg.Name, logger)
	if err != nil {
		return nil, err
	}

	if _, err := multipassexec.Info(ctx, cfg.Name); err == nil {
		return nil, fmt.Errorf(
			"multipass VM %q already exists; run `multipass delete --purge %s` to recover",
			cfg.Name, cfg.Name)
	} else if !errors.Is(err, multipassexec.ErrNotFound) {
		return nil, err
	}

	kubecfg.CleanupStale()

	c := &Cluster{cfg: cfg, logger: logger, Kubeconfig: kubecfg}

	if err := c.launch(ctx); err != nil {
		return nil, err
	}

	ip, err := c.waitForVMIP(ctx)
	if err != nil {
		return nil, fmt.Errorf("resolve VM IP: %w", err)
	}
	c.vmIP = ip
	logger.Info("multipass VM up", zap.String("name", cfg.Name), zap.String("ip", ip))

	if err := c.writeRegistries(ctx); err != nil {
		return nil, fmt.Errorf("write registries: %w", err)
	}

	if err := c.installK3s(ctx); err != nil {
		return nil, fmt.Errorf("install k3s: %w", err)
	}

	rawKubeconfig, err := c.extractKubeconfig(ctx)
	if err != nil {
		return nil, fmt.Errorf("extract kubeconfig: %w", err)
	}
	if err := kubecfg.Import(rawKubeconfig); err != nil {
		return nil, fmt.Errorf("merge kubeconfig: %w", err)
	}
	logger.Info("k3s ready", zap.String("context", cfg.Context))

	if cfg.Gateway.Skip {
		logger.Info("envoy gateway install skipped (gateway.skip)")
	} else {
		// DNSHintIP stays empty for multipass. The annotation exists
		// to override Service status when ServiceLB would advertise
		// an unreachable address (qemu SLIRP, docker container-internal
		// IP). Multipass binds the VM on a hypervisor-managed network
		// the host is part of, so the node's own IP is host-routable
		// and ServiceLB's default advertisement is correct. Consumer
		// tooling that wants the IP should read Service status (the
		// non-tunnel-NAT path the CHANGE_REQUEST_HINT_IP migration
		// names).
		if err := envoygateway.Install(ctx, envoygateway.Options{
			ContextName:      cfg.Context,
			GatewayClassName: cfg.Gateway.ClassName,
			Logger:           logger,
		}); err != nil {
			return nil, fmt.Errorf("install envoy gateway: %w", err)
		}
		logger.Info("envoy gateway ready",
			zap.String("version", envoygateway.Version),
			zap.String("gatewayClass", cfg.Gateway.ClassName),
		)
	}
	return c, nil
}

// Teardown stops and (unless keepDisk) deletes/purges the VM, then
// removes the kubeconfig context entry. keepDisk=true means the VM
// is stopped but not deleted, so a follow-up `multipass start`
// reuses the same disk.
func (c *Cluster) Teardown(keepDisk bool) error {
	return TeardownConfig(c.cfg, keepDisk, c.logger)
}

// TeardownConfig stops a VM by config without a running Cluster
// instance. Each step treats "VM not found" as success so a partial
// previous teardown can be re-run.
func TeardownConfig(cfg Config, keepDisk bool, logger *zap.Logger) error {
	if logger == nil {
		logger = zap.NewNop()
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	logger.Info("stopping multipass VM", zap.String("name", cfg.Name))
	if err := multipassexec.Stop(ctx, cfg.Name); err != nil && !errors.Is(err, multipassexec.ErrNotFound) {
		return err
	}

	if keepDisk {
		logger.Info("teardown complete, VM preserved (keepDisk)", zap.String("name", cfg.Name))
	} else {
		if err := multipassexec.Delete(ctx, cfg.Name, true); err != nil && !errors.Is(err, multipassexec.ErrNotFound) {
			return err
		}
		logger.Info("teardown complete, VM deleted", zap.String("name", cfg.Name))
	}

	kubecfg, err := kubeconfig.New(cfg.Context, cfg.Name, logger)
	if err == nil {
		kubecfg.CleanupTeardown()
	}
	return nil
}

// Context implements provision.Cluster.
func (c *Cluster) Context() string { return c.cfg.Context }

// NodeExec runs a command inside the VM via `multipass exec`.
// stdin (when non-nil) is piped through so callers can stream OCI
// tarballs into `ctr image import` on the node.
func (c *Cluster) NodeExec(ctx context.Context, command string, stdin io.Reader) ([]byte, error) {
	return multipassexec.Exec(ctx, c.cfg.Name, command, stdin)
}

// VMIP returns the VM's hypervisor-managed IPv4 address. Empty
// before Provision has resolved it.
func (c *Cluster) VMIP() string { return c.vmIP }

// cloudInitBody is the minimal cloud-config we hand to
// `multipass launch`. No SSH key plumbing -- `multipass exec`
// runs as root over the daemon's IPC channel.
func (c *Cluster) cloudInitBody() string {
	return fmt.Sprintf(`#cloud-config
hostname: %s
users:
  - name: ystack
    sudo: ALL=(ALL) NOPASSWD:ALL
    shell: /bin/bash
package_update: false
`, c.cfg.Name)
}

// launch invokes `multipass launch` with the configured shape.
// Memory takes the qemu/docker convention of plain MB, which we
// convert to multipass's `<n>M` form.
//
// cloud-init is piped via stdin (`--cloud-init -`) rather than
// referenced as a file path. The snap-packaged multipass on Linux
// runs the daemon under AppArmor confinement: the auto-connected
// `home` interface grants the daemon access to `$HOME/*` but not
// to hidden dotfiles or directories, and `/tmp` is private to the
// snap. Avoiding the path entirely sidesteps the whole class of
// confinement issues and works identically on macOS where there is
// no confinement.
func (c *Cluster) launch(ctx context.Context) error {
	c.logger.Info("launching multipass VM",
		zap.String("name", c.cfg.Name),
		zap.String("image", c.cfg.Image),
		zap.String("cpus", c.cfg.CPUs),
		zap.String("memory", c.cfg.Memory+"M"),
		zap.String("disk", diskSize),
	)
	args := []string{
		"launch",
		"--name", c.cfg.Name,
		"--cpus", c.cfg.CPUs,
		"--memory", c.cfg.Memory + "M",
		"--disk", diskSize,
		"--cloud-init", "-",
		c.cfg.Image,
	}
	out, err := multipassexec.Run(ctx, strings.NewReader(c.cloudInitBody()), args...)
	if err != nil {
		return fmt.Errorf("multipass launch: %s: %w", out, err)
	}
	return nil
}

// waitForVMIP polls `multipass info` until the VM reports a
// non-empty IPv4 address. multipass occasionally returns the VM
// before the dhcp lease is visible, so we retry rather than fail
// on a single empty read.
func (c *Cluster) waitForVMIP(ctx context.Context) (string, error) {
	deadline := time.Now().Add(60 * time.Second)
	for {
		info, err := multipassexec.Info(ctx, c.cfg.Name)
		if err == nil {
			if ip := multipassexec.FirstIPv4(info); ip != "" {
				return ip, nil
			}
		}
		if time.Now().After(deadline) {
			return "", fmt.Errorf("VM %q has no IPv4 after 60s", c.cfg.Name)
		}
		select {
		case <-ctx.Done():
			return "", ctx.Err()
		case <-time.After(2 * time.Second):
		}
	}
}

// writeRegistries renders the configured registries.yaml and
// stages it in the VM at registries.Path. Empty config is a no-op
// (containerd then falls back to its defaults).
func (c *Cluster) writeRegistries(ctx context.Context) error {
	body, err := registries.Marshal(c.cfg.Registries)
	if err != nil {
		return err
	}
	if body == nil {
		return nil
	}
	c.logger.Info("writing registries.yaml",
		zap.String("path", registries.Path),
		zap.Int("mirrors", len(c.cfg.Registries.Mirrors)),
		zap.Int("configs", len(c.cfg.Registries.Configs)),
	)
	cmd := "sudo install -d -m 0755 /etc/rancher/k3s && sudo install -m 0600 /dev/stdin " + registries.Path
	out, err := c.NodeExec(ctx, cmd, bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("write %s: %s: %w", registries.Path, out, err)
	}
	return nil
}

// extractKubeconfig reads the k3s-generated kubeconfig and rewrites
// the embedded server URL to point at the VM's IP. k3s's installer
// sets `server: https://127.0.0.1:6443` (loopback inside the VM);
// the host needs to dial the VM directly because there is no port
// forward in this topology. The tls-san we passed in INSTALL_K3S_EXEC
// puts the VM IP in the apiserver cert so the rewrite verifies.
func (c *Cluster) extractKubeconfig(ctx context.Context) ([]byte, error) {
	out, err := c.NodeExec(ctx, "sudo cat /etc/rancher/k3s/k3s.yaml", nil)
	if err != nil {
		return nil, fmt.Errorf("read kubeconfig: %s: %w", out, err)
	}
	if c.vmIP == "" {
		return nil, fmt.Errorf("VM IP not resolved before extracting kubeconfig")
	}
	return bytes.ReplaceAll(out, []byte("127.0.0.1:6443"), []byte(c.vmIP+":6443")), nil
}
