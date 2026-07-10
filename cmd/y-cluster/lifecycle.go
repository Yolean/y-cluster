package main

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/spf13/cobra"
	"go.uber.org/zap"

	"github.com/Yolean/y-cluster/pkg/cluster"
	"github.com/Yolean/y-cluster/pkg/provision/docker"
	"github.com/Yolean/y-cluster/pkg/provision/hetzner"
	"github.com/Yolean/y-cluster/pkg/provision/multipass"
	"github.com/Yolean/y-cluster/pkg/provision/qemu"
)

// pauseCmd, resumeCmd, stopCmd, startCmd are the cluster lifecycle
// subcommands -- a provisioner-neutral surface with per-backend
// depth: stop dispatches to all three backends; pause and resume
// are qemu-only and return a "not yet implemented for <backend>"
// error elsewhere; start assumes qemu outright (it rehydrates from
// the qemu sidecar, so a stopped docker cluster gets the qemu "no
// saved state" error rather than a not-implemented one).
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
				// A manual stop ends this run's budget; remove the
				// host expiry timer. `start` re-arms a fresh window.
				disarmHostTimer(contextName, logger)
				return qemu.Stop(qemuCacheDir(), lr.ClusterName, logger)
			case cluster.BackendDocker:
				return docker.Stop(ctx, lr.ClusterName, logger)
			case cluster.BackendMultipass:
				return multipass.Stop(ctx, lr.ClusterName, logger)
			case cluster.BackendHetzner:
				return hetzner.Stop(ctx, lr.ClusterName, logger)
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
			case cluster.BackendHetzner:
				// pause / resume have no Hetzner Cloud analog
				// (no SIGSTOP/SIGCONT against a guest). Surface a
				// clean refusal rather than the generic
				// "not yet implemented" so the operator knows it
				// will not be implemented.
				return fmt.Errorf("%s: not supported on hetzner provider (Hetzner Cloud has no pause/resume primitive); use `y-cluster stop` + `y-cluster start` to power-cycle the server, or `y-cluster teardown` to free billing", name)
			default:
				return fmt.Errorf("%s: not yet implemented for %s", name, lr.Backend)
			}
		},
	}
	cmd.Flags().StringVar(&contextName, "context", cluster.DefaultContext, "kubeconfig context name")
	return cmd
}

// prepareExportCmd readies the appliance disk for export: a LIVE
// phase against the running cluster (clear per-deploy dns-hint-ip
// GatewayClass annotations, snapshot reconciled gateway state),
// then an internal stop, then virt-customize against the offline
// qcow2 for the identity reset. qemu-only today; other backends
// would need their own disk-export hook so we don't stub them with
// a misleading "not implemented" -- the surface stays narrow on
// purpose.
//
// We resolve the cluster name from the kubeconfig context the same
// way startCmd does, not via cluster.Lookup's runtime probing --
// the name must stay resolvable independent of VM state since
// prepare-export transitions the VM from running to stopped.
func prepareExportCmd() *cobra.Command {
	var contextName string
	cmd := &cobra.Command{
		Use:   "prepare-export",
		Short: "Prepare the running qemu appliance for export to a different hypervisor",
		Long: `Readies the appliance disk for shipping, in two phases.

Live phase (cluster must be RUNNING): clears the per-deploy
yolean.se/dns-hint-ip GatewayClass annotations and snapshots the
reconciled Gateway state for the export bundle, then stops the
VM itself. Do not run 'y-cluster stop' first.

Offline phase (virt-customize on the stopped qcow2): wipes
machine-id, SSH host keys, udev persistent net rules, MAC-bound
netplan, and the cloud-init state cache; stages the data seed
and any added manifests; registers a firstboot ssh-keygen so the
imported instance regenerates its own host keys.

	y-cluster provision
	y-cluster prepare-export   # stops the VM internally

Idempotent. A prepared appliance is no longer a usable dev
cluster locally: the next start runs cloud-init re-init and
regenerates identity. Re-provision for a fresh dev loop.

Requires libguestfs-tools (sudo apt install libguestfs-tools)
and kubectl.`,
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
			// prepare-export is a libguestfs-on-qcow2 operation and
			// has no Hetzner Cloud equivalent (no disk-image upload
			// API). Detect a hetzner-provisioned context up front
			// and refuse cleanly so the operator gets the
			// "use the qemu provisioner" guidance instead of a
			// confusing qemu-shaped failure deep inside virt-sysprep.
			if hetzner.HasState(clusterName) {
				return fmt.Errorf("prepare-export: not supported on hetzner provider (Hetzner Cloud has no custom-disk-image upload API); use the qemu provisioner for disk-bound appliances")
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
			// Hetzner: a stopped server still has a state sidecar
			// (Stop only powers off; the cache files stay put).
			// Use that as the discriminator since cluster.Lookup
			// can't probe a powered-off server.
			if hetzner.HasState(clusterName) {
				ipv4, err := hetzner.Start(cmd.Context(), clusterName, logger)
				if err != nil {
					return err
				}
				logger.Info("cluster started",
					zap.String("context", clusterName),
					zap.String("ipv4", ipv4),
				)
				return nil
			}
			c, err := qemu.Start(cmd.Context(), qemuCacheDir(), clusterName, logger)
			if err != nil {
				return err
			}
			// Start re-anchored the deadline to now; install the host
			// timer for the fresh window.
			armHostTimerIfLifetime(qemuCacheDir(), clusterName, contextName, logger)
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
