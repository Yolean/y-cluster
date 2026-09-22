package main

import (
	"context"
	"fmt"
	"path/filepath"

	"go.uber.org/zap"

	"github.com/Yolean/y-cluster/pkg/provision/config"
	"github.com/Yolean/y-cluster/pkg/provision/docker"
	"github.com/Yolean/y-cluster/pkg/provision/hetzner"
	"github.com/Yolean/y-cluster/pkg/provision/multipass"
	"github.com/Yolean/y-cluster/pkg/provision/qemu"
)

// providerOps is what the verbs that start from a config directory
// (provision, teardown) need from a provider. The provider packages
// have grown different shapes (runtime config vs. on-disk config,
// handle vs. context name); the adapters below are where those
// differences live, so the verbs themselves have none.
type providerOps struct {
	// provision brings the cluster up. loginHint is how an operator
	// gets a shell on the node, for the "cluster ready" line.
	provision func(ctx context.Context, cfg config.ProviderConfig, logger *zap.Logger) (loginHint string, err error)
	// teardown removes the cluster. keepDisk means something only to
	// providers with a persistent disk.
	teardown func(ctx context.Context, cfg config.ProviderConfig, keepDisk bool, logger *zap.Logger) error
	// hostPorts lists the ports the cluster binds on this host, for
	// the inventory that preflight uses to say who holds a port.
	hostPorts func(cfg config.ProviderConfig) []string
	// stop shuts a running cluster down and keeps what is needed to
	// bring it back. It starts from a kubeconfig context rather than
	// a config directory, so it gets the names cluster.Lookup found.
	stop func(ctx context.Context, contextName, clusterName string, logger *zap.Logger) error
}

// providers has one entry per config.AllProviders; a test holds that.
// Each adapter asserts its own config type, which LoadProvision
// guarantees for the provider name it was looked up by.
var providers = map[string]providerOps{
	config.ProviderQEMU: {
		provision: func(ctx context.Context, cfg config.ProviderConfig, logger *zap.Logger) (string, error) {
			rt := qemu.FromConfig(cfg.(*config.QEMUConfig))
			if err := qemu.CheckPrerequisites(); err != nil {
				return "", err
			}
			if _, err := qemu.Provision(ctx, rt, logger); err != nil {
				return "", err
			}
			// Provision armed the deadline; install the host-side
			// timer that fires the local expiry action.
			armHostTimerIfLifetime(rt.CacheDir, rt.Name, rt.Context, logger)
			return rt.SSHCommand(), nil
		},
		teardown: func(_ context.Context, cfg config.ProviderConfig, keepDisk bool, logger *zap.Logger) error {
			// Remove the host expiry timer before the cluster goes;
			// the deadline is moot once teardown removes the sidecar.
			disarmHostTimer(cfg.Common().Context, logger)
			return qemu.TeardownConfig(qemu.FromConfig(cfg.(*config.QEMUConfig)), keepDisk, logger)
		},
		hostPorts: func(cfg config.ProviderConfig) []string {
			q := cfg.(*config.QEMUConfig)
			ports := forwardHostPorts(q.PortForwards)
			if q.SSHPort != "" {
				ports = append(ports, q.SSHPort)
			}
			return ports
		},
		stop: func(_ context.Context, contextName, clusterName string, logger *zap.Logger) error {
			// A manual stop ends this run's budget; remove the
			// host expiry timer. `start` re-arms a fresh window.
			disarmHostTimer(contextName, logger)
			return qemu.Stop(qemuCacheDir(), clusterName, logger)
		},
	},
	config.ProviderDocker: {
		provision: func(ctx context.Context, cfg config.ProviderConfig, logger *zap.Logger) (string, error) {
			d := cfg.(*config.DockerConfig)
			if _, err := docker.Provision(ctx, *d, logger); err != nil {
				return "", err
			}
			return fmt.Sprintf("docker exec -it %s sh", d.Name), nil
		},
		teardown: func(_ context.Context, cfg config.ProviderConfig, keepDisk bool, logger *zap.Logger) error {
			return docker.TeardownConfig(*cfg.(*config.DockerConfig), keepDisk, logger)
		},
		hostPorts: func(cfg config.ProviderConfig) []string { return forwardHostPorts(cfg.Common().PortForwards) },
		stop: func(ctx context.Context, _, clusterName string, logger *zap.Logger) error {
			return docker.Stop(ctx, clusterName, logger)
		},
	},
	config.ProviderMultipass: {
		provision: func(ctx context.Context, cfg config.ProviderConfig, logger *zap.Logger) (string, error) {
			rt := multipass.FromConfig(cfg.(*config.MultipassConfig))
			if _, err := multipass.Provision(ctx, rt, logger); err != nil {
				return "", err
			}
			return fmt.Sprintf("multipass shell %s", rt.Name), nil
		},
		teardown: func(_ context.Context, cfg config.ProviderConfig, keepDisk bool, logger *zap.Logger) error {
			return multipass.TeardownConfig(multipass.FromConfig(cfg.(*config.MultipassConfig)), keepDisk, logger)
		},
		hostPorts: func(cfg config.ProviderConfig) []string { return forwardHostPorts(cfg.Common().PortForwards) },
		stop: func(ctx context.Context, _, clusterName string, logger *zap.Logger) error {
			return multipass.Stop(ctx, clusterName, logger)
		},
	},
	config.ProviderHetzner: {
		// Lifetime expiry details (or the lack of a budget) are
		// logged by Provision's reaper step.
		provision: func(ctx context.Context, cfg config.ProviderConfig, logger *zap.Logger) (string, error) {
			h := cfg.(*config.HetznerConfig)
			cluster, err := hetzner.Provision(ctx, *h, logger)
			if err != nil {
				return "", err
			}
			return fmt.Sprintf("ssh -i %s %s@%s",
				filepath.Join(hetzner.CacheDir(), h.Context+"-ssh"), h.SSHUser, cluster.PublicIPv4()), nil
		},
		teardown: func(ctx context.Context, cfg config.ProviderConfig, _ bool, logger *zap.Logger) error {
			return hetzner.Teardown(ctx, cfg.Common().Context, logger)
		},
		// A remote server binds nothing on this host. The record is
		// still written: its config path is what teardown lists.
		hostPorts: func(config.ProviderConfig) []string { return nil },
		stop: func(ctx context.Context, _, clusterName string, logger *zap.Logger) error {
			return hetzner.Stop(ctx, clusterName, logger)
		},
	},
}

// opsFor returns the provider's adapters. A provider that is
// registered for config loading but has no entry here can be loaded
// and validated, and that is all.
func opsFor(cfg config.ProviderConfig) (providerOps, error) {
	ops, ok := providers[cfg.Common().Provider]
	if !ok {
		return providerOps{}, fmt.Errorf("provider %q has a config type but no provisioner in this binary", cfg.Common().Provider)
	}
	return ops, nil
}
