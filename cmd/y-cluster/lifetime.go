package main

import (
	"errors"
	"fmt"
	"os"
	"time"

	"github.com/spf13/cobra"
	"go.uber.org/zap"

	"github.com/Yolean/y-cluster/pkg/cluster"
	"github.com/Yolean/y-cluster/pkg/lifetime"
	"github.com/Yolean/y-cluster/pkg/provision/config"
	"github.com/Yolean/y-cluster/pkg/provision/qemu"
)

// lifetimeCmd is the cost-control auto-expiry surface. A dev cluster
// left running after a task is paused or finished is pure cost; a
// lifetime budget makes the cluster expire on its own.
//
// Three enforcement paths, picked by where the cost lives:
//   - LOCAL (qemu): a host-side timer (status/reap/extend/arm/disarm
//     below) runs the onExpiry action. The host is the cost, so a
//     host timer is the right trigger.
//   - CLOUD (GCP appliance): `gcp-flags` emits gcloud scheduling
//     flags so GCP itself deletes the instance at the deadline -- no
//     host or cluster dependency.
//   - CLOUD (hetzner): an in-cluster reaper Job installed at
//     provision sleeps the budget then runs the expiry action via
//     the hcloud API. Cluster-side rather than host-side for the
//     same reason as GCP: the trigger for a paid cloud resource
//     must survive the provisioning machine going away.
//
// The host-side subcommands are qemu-only today, matching the rest
// of the lifecycle surface; the qemu state sidecar is where the
// deadline lives. For hetzner, `kubectl -n y-cluster-reaper get
// job reaper -o yaml` shows the armed window (max-run / on-expiry /
// expires-at annotations).
func lifetimeCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "lifetime",
		Short: "Cost-control auto-expiry: stop/decommission a cluster that is no longer needed",
	}
	cmd.AddCommand(
		lifetimeStatusCmd(),
		lifetimeReapCmd(),
		lifetimeExtendCmd(),
		lifetimeArmCmd(),
		lifetimeDisarmCmd(),
		lifetimeGCPFlagsCmd(),
	)
	return cmd
}

// armHostTimerIfLifetime installs the host-side reap timer when the
// qemu cluster has an armed deadline. Called from provision/start
// after the deadline has been (re)anchored. Best-effort: a failure
// to arm is logged, not fatal -- the deadline is persisted and
// `y-cluster lifetime reap` (by hand or from any scheduler) remains
// the backstop. No-op when no lifetime is configured.
func armHostTimerIfLifetime(cacheDir, name, contextName string, logger *zap.Logger) {
	ls, err := qemu.LoadLifetime(cacheDir, name)
	if err != nil || !ls.Enabled() || ls.ExpiresAt.IsZero() {
		return
	}
	bin, err := os.Executable()
	if err != nil {
		logger.Warn("could not resolve binary path to arm lifetime timer", zap.Error(err))
		return
	}
	if err := lifetime.Arm(bin, contextName, ls.Remaining(), logger); err != nil {
		logger.Warn("could not arm lifetime host timer", zap.Error(err))
	}
}

// disarmHostTimer removes the host-side reap timer for a context.
// Called from stop/teardown. Best-effort by design.
func disarmHostTimer(contextName string, logger *zap.Logger) {
	_ = lifetime.Disarm(contextName, logger)
}

// resolveQemuCluster maps a kubeconfig context to the qemu cache dir
// + cluster name that the lifetime sidecar is keyed on. Works for a
// stopped cluster too (the context survives in kubeconfig), unlike
// cluster.Lookup which needs a running runtime.
func resolveQemuCluster(contextName string) (cacheDir, name string, err error) {
	name, err = cluster.ResolveClusterName("", contextName)
	if err != nil {
		return "", "", err
	}
	if name == "" {
		return "", "", fmt.Errorf("kubeconfig context %q has no associated cluster", contextName)
	}
	return qemuCacheDir(), name, nil
}

// lifetimeStateErr renders the missing-sidecar case as a clear
// scope message rather than a raw "no such file" error: the
// host-side verbs are qemu-only, and on hetzner expiry is enforced
// by the in-cluster reaper Job installed at provision instead.
func lifetimeStateErr(name string, err error) error {
	if errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("no lifetime state for cluster %q; the host-side lifetime verbs are implemented for the qemu provider only (on hetzner expiry is enforced by the in-cluster reaper Job installed at provision)", name)
	}
	return err
}

func lifetimeStatusCmd() *cobra.Command {
	var contextName string
	cmd := &cobra.Command{
		Use:   "status",
		Short: "Show the cluster's lifetime policy and time remaining",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			cacheDir, name, err := resolveQemuCluster(contextName)
			if err != nil {
				return err
			}
			ls, err := qemu.LoadLifetime(cacheDir, name)
			if err != nil {
				return lifetimeStateErr(name, err)
			}
			out := cmd.OutOrStdout()
			if !ls.Enabled() {
				fmt.Fprintf(out, "lifetime: disabled (no maxRun) for %q\n", name)
				return nil
			}
			fmt.Fprintf(out, "cluster:   %s\n", name)
			fmt.Fprintf(out, "maxRun:    %s\n", ls.MaxRun)
			fmt.Fprintf(out, "onExpiry:  %s\n", ls.OnExpiry)
			if ls.ExpiresAt.IsZero() {
				fmt.Fprintln(out, "expiresAt: (not armed; run `y-cluster start` or `y-cluster lifetime arm`)")
				return nil
			}
			fmt.Fprintf(out, "expiresAt: %s\n", ls.ExpiresAt.Format(time.RFC3339))
			rem := ls.Remaining().Round(time.Second)
			if rem < 0 {
				fmt.Fprintf(out, "remaining: EXPIRED (%s ago)\n", (-rem).String())
			} else {
				fmt.Fprintf(out, "remaining: %s\n", rem)
			}
			return nil
		},
	}
	cmd.Flags().StringVar(&contextName, "context", cluster.DefaultContext, "kubeconfig context name")
	return cmd
}

func lifetimeReapCmd() *cobra.Command {
	var contextName string
	cmd := &cobra.Command{
		Use:   "reap",
		Short: "Run the expiry action if the deadline has passed; otherwise re-arm",
		Long: `reap is what the host timer fires at the deadline. It is
idempotent and self-healing: it re-reads the persisted deadline and
acts only if it has truly elapsed. If the deadline was pushed out
(e.g. via ` + "`lifetime extend`" + `) since the timer was set, reap
simply re-arms for the remaining window and exits. Safe to run by
hand or from an external cron.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			logger := loggerFromContext(cmd.Context())
			cacheDir, name, err := resolveQemuCluster(contextName)
			if err != nil {
				return err
			}
			ls, err := qemu.LoadLifetime(cacheDir, name)
			if err != nil {
				return lifetimeStateErr(name, err)
			}
			if !ls.Enabled() {
				logger.Info("no lifetime configured; nothing to reap", zap.String("cluster", name))
				return nil
			}
			if ls.ExpiresAt.IsZero() {
				logger.Info("lifetime not armed; nothing to reap", zap.String("cluster", name))
				return nil
			}
			if !ls.Expired() {
				rem := ls.Remaining()
				if bin, err := os.Executable(); err == nil {
					if err := lifetime.Arm(bin, contextName, rem, logger); err != nil {
						logger.Warn("could not re-arm host timer", zap.Error(err))
					}
				}
				logger.Info("not yet expired; re-armed",
					zap.String("cluster", name), zap.Duration("remaining", rem.Round(time.Second)))
				return nil
			}

			logger.Info("lifetime expired; reaping",
				zap.String("cluster", name), zap.String("onExpiry", ls.OnExpiry))
			switch ls.OnExpiry {
			case config.OnExpiryPause:
				err = qemu.Pause(cacheDir, name, logger)
			case config.OnExpiryTeardown:
				err = qemu.TeardownByName(cacheDir, name, false, logger)
			default: // stop is the default and the empty-value behaviour
				err = qemu.Stop(cacheDir, name, logger)
			}
			if err != nil {
				return err
			}
			// Action performed: remove the host timer (best-effort;
			// reap's recheck makes a stray timer harmless anyway).
			_ = lifetime.Disarm(contextName, logger)
			return nil
		},
	}
	cmd.Flags().StringVar(&contextName, "context", cluster.DefaultContext, "kubeconfig context name")
	return cmd
}

func lifetimeExtendCmd() *cobra.Command {
	var contextName string
	cmd := &cobra.Command{
		Use:   "extend <duration>",
		Short: "Push the deadline out by <duration> (e.g. 2h) and re-arm",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			logger := loggerFromContext(cmd.Context())
			d, err := time.ParseDuration(args[0])
			if err != nil {
				return fmt.Errorf("invalid duration %q: %w", args[0], err)
			}
			if d <= 0 {
				return fmt.Errorf("extend duration must be positive, got %q", args[0])
			}
			cacheDir, name, err := resolveQemuCluster(contextName)
			if err != nil {
				return err
			}
			nt, err := qemu.ExtendDeadline(cacheDir, name, d)
			if err != nil {
				return lifetimeStateErr(name, err)
			}
			if bin, err := os.Executable(); err == nil {
				if err := lifetime.Arm(bin, contextName, time.Until(nt), logger); err != nil {
					logger.Warn("could not re-arm host timer", zap.Error(err))
				}
			}
			fmt.Fprintf(cmd.OutOrStdout(), "extended; expiresAt %s\n", nt.Format(time.RFC3339))
			return nil
		},
	}
	cmd.Flags().StringVar(&contextName, "context", cluster.DefaultContext, "kubeconfig context name")
	return cmd
}

func lifetimeArmCmd() *cobra.Command {
	var contextName string
	cmd := &cobra.Command{
		Use:   "arm",
		Short: "(Re)install the host timer that fires the expiry action",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			logger := loggerFromContext(cmd.Context())
			cacheDir, name, err := resolveQemuCluster(contextName)
			if err != nil {
				return err
			}
			ls, err := qemu.LoadLifetime(cacheDir, name)
			if err != nil {
				return lifetimeStateErr(name, err)
			}
			if !ls.Enabled() {
				return fmt.Errorf("no lifetime configured for %q; set lifetime.maxRun and re-provision", name)
			}
			if ls.ExpiresAt.IsZero() {
				return fmt.Errorf("no deadline armed for %q; `y-cluster start` re-anchors it", name)
			}
			bin, err := os.Executable()
			if err != nil {
				return err
			}
			return lifetime.Arm(bin, contextName, ls.Remaining(), logger)
		},
	}
	cmd.Flags().StringVar(&contextName, "context", cluster.DefaultContext, "kubeconfig context name")
	return cmd
}

func lifetimeDisarmCmd() *cobra.Command {
	var contextName string
	cmd := &cobra.Command{
		Use:   "disarm",
		Short: "Remove the host timer (the persisted deadline is left intact)",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			logger := loggerFromContext(cmd.Context())
			return lifetime.Disarm(contextName, logger)
		},
	}
	cmd.Flags().StringVar(&contextName, "context", cluster.DefaultContext, "kubeconfig context name")
	return cmd
}

func lifetimeGCPFlagsCmd() *cobra.Command {
	var configDir string
	cmd := &cobra.Command{
		Use:   "gcp-flags",
		Short: "Print gcloud instances-create flags that enforce the lifetime cloud-side",
		Long: `Reads lifetime.maxRun from the y-cluster-provision.yaml in -c
<dir> and prints the matching
` + "`--max-run-duration=<secs>s --instance-termination-action=DELETE`" + `
flags for ` + "`gcloud compute instances create`" + `. Prints nothing
when no lifetime is configured, so a build script can append the
output unconditionally:

    EXTRA=$(y-cluster lifetime gcp-flags -c "$CONFIG_DIR")
    gcloud compute instances create ... $EXTRA`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			loaded, err := loadProvision(configDir)
			if err != nil {
				return err
			}
			acc, ok := loaded.(interface {
				LifetimePolicy() config.LifetimeConfig
			})
			if !ok {
				return nil // provider with no lifetime surface: emit nothing
			}
			flags, err := lifetime.GCPFlags(acc.LifetimePolicy().MaxRun)
			if err != nil {
				return err
			}
			if flags != "" {
				fmt.Fprintln(cmd.OutOrStdout(), flags)
			}
			return nil
		},
	}
	cmd.Flags().StringVarP(&configDir, "config", "c", "", "directory containing y-cluster-provision.yaml")
	if err := cmd.MarkFlagRequired("config"); err != nil {
		panic(err)
	}
	return cmd
}
