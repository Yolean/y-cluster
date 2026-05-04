package main

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/spf13/cobra"
	"go.uber.org/zap"

	"github.com/Yolean/y-cluster/pkg/cluster"
	"github.com/Yolean/y-cluster/pkg/provision/docker"
	"github.com/Yolean/y-cluster/pkg/provision/multipass"
	"github.com/Yolean/y-cluster/pkg/provision/qemu"
)

// pauseCmd, resumeCmd, stopCmd, startCmd are the cluster lifecycle
// subcommands -- a provisioner-neutral surface that today only
// implements the qemu backend. docker and multipass return a "not
// yet implemented for <backend>" error so the surface is stable
// while the per-backend semantics are still being worked out.
//
// All four resolve the cluster via the kubeconfig context (the
// same convention detect / ctr / crictl use) -- no -c <dir>
// required. Pause / Resume / Stop find the running cluster via
// cluster.Lookup; Start can't (the cluster is stopped) and
// instead reads the cluster name from kubeconfig and rehydrates
// the qemu launch parameters from the sidecar Provision wrote.

func pauseCmd() *cobra.Command {
	return signalCmd("pause", "Pause the cluster VM (SIGSTOP); resume to unfreeze", qemu.Pause)
}

func resumeCmd() *cobra.Command {
	return signalCmd("resume", "Resume a paused cluster VM (SIGCONT)", qemu.Resume)
}

// stopCmd dispatches per-backend rather than going through
// signalCmd: qemu.Stop's signature takes (cacheDir, name); docker
// and multipass take (ctx, name); a uniform signalCmd helper
// would need a wrapper per backend anyway, so the explicit switch
// is clearer.
func stopCmd() *cobra.Command {
	var contextName string
	cmd := &cobra.Command{
		Use:   "stop",
		Short: "Gracefully shut down the cluster; disk preserved for `y-cluster start`",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			ctx := cmd.Context()
			logger := loggerFromContext(ctx)
			lr, err := cluster.Lookup(ctx, "", contextName)
			if err != nil {
				return err
			}
			switch lr.Backend {
			case cluster.BackendQEMU:
				return qemu.Stop(qemuCacheDir(), lr.ClusterName, logger)
			case cluster.BackendDocker:
				return docker.Stop(ctx, lr.ClusterName, logger)
			case cluster.BackendMultipass:
				return multipass.Stop(ctx, lr.ClusterName, logger)
			default:
				return fmt.Errorf("stop: not yet implemented for %s", lr.Backend)
			}
		},
	}
	cmd.Flags().StringVar(&contextName, "context", cluster.DefaultContext, "kubeconfig context name")
	return cmd
}

// signalCmd is the shared shape for pause / resume / stop. Each
// looks up the running cluster, dispatches by backend, and calls
// the qemu lifecycle function. Non-qemu backends return a "not
// implemented" error.
func signalCmd(name, short string, run func(cacheDir, name string, logger *zap.Logger) error) *cobra.Command {
	var contextName string
	cmd := &cobra.Command{
		Use:   name,
		Short: short,
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			logger := loggerFromContext(cmd.Context())
			lr, err := cluster.Lookup(cmd.Context(), "", contextName)
			if err != nil {
				return err
			}
			switch lr.Backend {
			case cluster.BackendQEMU:
				return run(qemuCacheDir(), lr.ClusterName, logger)
			default:
				return fmt.Errorf("%s: not yet implemented for %s", name, lr.Backend)
			}
		},
	}
	cmd.Flags().StringVar(&contextName, "context", cluster.DefaultContext, "kubeconfig context name")
	return cmd
}

// prepareExportCmd runs libguestfs's virt-sysprep against the
// stopped appliance disk so it boots cleanly on a different
// hypervisor. qemu-only today; other backends would need their own
// disk-export hook so we don't stub them with a misleading "not
// implemented" -- the surface stays narrow on purpose.
//
// Cluster must be stopped first; virt-sysprep operates on the
// offline qcow2 (libguestfs mounts it directly, no boot involved).
// We resolve the cluster name from the kubeconfig context the same
// way startCmd does, since cluster.Lookup needs a running cluster
// to probe.
func prepareExportCmd() *cobra.Command {
	var contextName string
	cmd := &cobra.Command{
		Use:   "prepare-export",
		Short: "Prepare a stopped qemu appliance for export to a different hypervisor",
		Long: `Clears host-specific state baked into the appliance disk so the
same disk boots cleanly when imported elsewhere (VMware, KVM,
cloud providers, Hetzner). Wipes machine-id, SSH host keys,
udev persistent net rules, MAC-bound netplan, and the cloud-init
state cache. Registers a firstboot ssh-keygen so the imported
instance regenerates its own host keys.

Cluster must be stopped first (virt-sysprep needs an offline
disk):

	y-cluster stop
	y-cluster prepare-export

Idempotent. A prepared appliance is no longer a usable dev
cluster locally: the next start runs cloud-init re-init and
regenerates identity. Re-provision for a fresh dev loop.

Requires libguestfs-tools (sudo apt install libguestfs-tools).`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			logger := loggerFromContext(cmd.Context())
			clusterName, err := cluster.ResolveClusterName("", contextName)
			if err != nil {
				return err
			}
			if clusterName == "" {
				return fmt.Errorf("kubeconfig context %q has no associated cluster; nothing to prepare", contextName)
			}
			return qemu.PrepareExport(cmd.Context(), qemuCacheDir(), clusterName, logger)
		},
	}
	cmd.Flags().StringVar(&contextName, "context", cluster.DefaultContext, "kubeconfig context name")
	return cmd
}

// exportCmd writes a customer-handoff bundle. The cluster must
// be stopped (and ideally prepare-export'd first) so the disk is
// quiesced and portable. Bundle layout + boot instructions live
// in pkg/provision/qemu.Export.
//
// --format is required (qcow2 or raw) until we know which one
// becomes the canonical handoff. Customers on different
// hypervisors will prefer different things; better to pick
// explicitly than have the default surprise someone.
func exportCmd() *cobra.Command {
	var (
		contextName   string
		format        string
		vmdkSubformat string
	)
	cmd := &cobra.Command{
		Use:   "export <bundle-dir>",
		Short: "Write a customer-installable appliance bundle to <bundle-dir>",
		Long: `Writes a self-contained bundle directory containing the
appliance disk (flattened, no backing-file dependency on the
build host), the SSH keypair, and a README with boot + ssh
instructions for the customer.

Run AFTER y-cluster stop (and ideally after y-cluster
prepare-export). Refuses to overwrite a non-empty bundle
directory; remove it first if you really mean to re-export.

Per-customer keypair: each provision generates a fresh keypair
which is bundled here. Re-running provision (after teardown)
produces a different keypair, so re-exporting an old cluster
versus a new provision yields a distinct customer handoff.

When --format=vmdk, --vmdk-subformat picks the qemu-img VMDK
shape. Default is streamOptimized (ESXi-friendly); pass
monolithicSparse for VirtualBox.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			logger := loggerFromContext(cmd.Context())
			clusterName, err := cluster.ResolveClusterName("", contextName)
			if err != nil {
				return err
			}
			if clusterName == "" {
				return fmt.Errorf("kubeconfig context %q has no associated cluster; nothing to export", contextName)
			}
			if vmdkSubformat != "" && format != string(qemu.FormatVMDK) {
				return fmt.Errorf("--vmdk-subformat is only valid with --format=vmdk")
			}
			return qemu.Export(cmd.Context(), qemu.ExportOptions{
				CacheDir:      qemuCacheDir(),
				Name:          clusterName,
				BundleDir:     args[0],
				Format:        qemu.Format(format),
				VMDKSubformat: vmdkSubformat,
				Logger:        logger,
			})
		},
	}
	cmd.Flags().StringVar(&contextName, "context", cluster.DefaultContext, "kubeconfig context name")
	cmd.Flags().StringVar(&format, "format", "",
		fmt.Sprintf("disk format (one of: %v); required until a default is chosen", qemu.AllFormats()))
	cmd.Flags().StringVar(&vmdkSubformat, "vmdk-subformat", "",
		fmt.Sprintf("VMDK subformat (one of: %v); only valid with --format=vmdk; default %q", qemu.AllVMDKSubformats(), qemu.VMDKSubformatDefault))
	if err := cmd.MarkFlagRequired("format"); err != nil {
		panic(err)
	}
	return cmd
}

// startCmd reverses stopCmd. The cluster is stopped, so
// cluster.Lookup can't find it; we resolve the cluster name
// straight off the kubeconfig context and ask qemu.Start to
// rehydrate from the saved sidecar in the default cache dir.
func startCmd() *cobra.Command {
	var contextName string
	cmd := &cobra.Command{
		Use:   "start",
		Short: "Start a previously-stopped cluster VM (qemu only)",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			logger := loggerFromContext(cmd.Context())
			clusterName, err := cluster.ResolveClusterName("", contextName)
			if err != nil {
				return err
			}
			if clusterName == "" {
				return fmt.Errorf("kubeconfig context %q has no associated cluster; nothing to start", contextName)
			}
			c, err := qemu.Start(cmd.Context(), qemuCacheDir(), clusterName, logger)
			if err != nil {
				return err
			}
			logger.Info("cluster started",
				zap.String("context", c.Context()),
			)
			return nil
		},
	}
	cmd.Flags().StringVar(&contextName, "context", cluster.DefaultContext, "kubeconfig context name")
	return cmd
}

// qemuCacheDir returns the same cache dir the qemu provisioner
// uses by default: $Y_CLUSTER_QEMU_CACHE_DIR when set, else
// ~/.cache/y-cluster-qemu. Centralised so the lifecycle
// subcommands and pkg/cluster's qemuRunning probe agree on where
// to look.
func qemuCacheDir() string {
	if env := os.Getenv("Y_CLUSTER_QEMU_CACHE_DIR"); env != "" {
		return env
	}
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".cache", "y-cluster-qemu")
}
