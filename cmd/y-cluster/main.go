package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"

	"github.com/spf13/cobra"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"

	"github.com/Yolean/y-cluster/pkg/provision/qemu"
	"github.com/Yolean/y-cluster/pkg/yconverge"
)

var version = "dev"

func main() {
	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()

	cmd := rootCmd()

	// When invoked as kubectl-yconverge, act as the yconverge subcommand directly
	if strings.HasPrefix(filepath.Base(os.Args[0]), "kubectl-yconverge") {
		cmd = yconvergePluginCmd()
	}

	if err := cmd.ExecuteContext(ctx); err != nil {
		os.Exit(1)
	}
}

func rootCmd() *cobra.Command {
	var verbose bool

	root := &cobra.Command{
		Use:     binaryName(),
		Short:   "Idempotent Kubernetes convergence with dependency ordering and checks",
		Version: version,
		PersistentPreRun: func(cmd *cobra.Command, args []string) {
			logger := newLogger(verbose)
			cmd.SetContext(withLogger(cmd.Context(), logger))
		},
	}

	root.PersistentFlags().BoolVarP(&verbose, "verbose", "v", false, "debug logging")

	root.AddCommand(yconvergeCmd())
	root.AddCommand(provisionCmd())
	root.AddCommand(teardownCmd())
	root.AddCommand(exportCmd())
	root.AddCommand(importCmd())
	root.AddCommand(serveCmd())
	root.AddCommand(imagesCmd())
	root.AddCommand(detectCmd())
	root.AddCommand(ctrCmd())
	root.AddCommand(crictlCmd())
	root.AddCommand(cacheCmd())

	return root
}

func yconvergePluginCmd() *cobra.Command {
	cmd := yconvergeCmd()
	cmd.Use = "kubectl-yconverge"
	cmd.Short = "kubectl plugin: apply a kustomize base with dependency resolution and checks"
	// Add persistent flags that rootCmd normally provides
	var verbose bool
	cmd.PersistentFlags().BoolVarP(&verbose, "verbose", "v", false, "debug logging")
	cmd.PersistentPreRun = func(c *cobra.Command, args []string) {
		logger := newLogger(verbose)
		c.SetContext(withLogger(c.Context(), logger))
	}
	return cmd
}

func yconvergeCmd() *cobra.Command {
	var opts yconverge.Options

	cmd := &cobra.Command{
		Use:   "yconverge",
		Short: "Apply a kustomize base with dependency resolution and checks",
		RunE: func(cmd *cobra.Command, args []string) error {
			logger := loggerFromContext(cmd.Context())
			result, err := yconverge.Run(cmd.Context(), opts, logger)
			if err != nil {
				logger.Error("convergence failed", zap.Error(err))
				return err
			}
			if opts.PrintDeps {
				for _, step := range result.Steps {
					fmt.Fprintln(cmd.OutOrStdout(), step)
				}
			}
			return nil
		},
	}

	cmd.Flags().StringVar(&opts.Context, "context", "", "Kubernetes context name (required)")
	cmd.Flags().StringVarP(&opts.KustomizeDir, "kustomize-dir", "k", "", "path to kustomize base (required)")
	cmd.Flags().StringVar(&opts.DryRun, "dry-run", "", "dry-run mode (server|none)")
	cmd.Flags().BoolVar(&opts.ChecksOnly, "checks-only", false, "run checks without applying")
	cmd.Flags().BoolVar(&opts.PrintDeps, "print-deps", false, "print dependency order and exit")
	cmd.Flags().BoolVar(&opts.SkipChecks, "skip-checks", false, "skip checks after apply")

	if err := cmd.MarkFlagRequired("context"); err != nil {
		panic(err)
	}
	if err := cmd.MarkFlagRequired("kustomize-dir"); err != nil {
		panic(err)
	}

	return cmd
}

// binaryName returns "y-cluster" or "kubectl-yconverge" depending on
// how the binary was invoked, supporting kubectl plugin symlinks.
func binaryName() string {
	name := filepath.Base(os.Args[0])
	if strings.HasPrefix(name, "kubectl-") {
		return name
	}
	return "y-cluster"
}

func newLogger(verbose bool) *zap.Logger {
	level := zapcore.InfoLevel
	if verbose {
		level = zapcore.DebugLevel
	}
	cfg := zap.Config{
		Level:            zap.NewAtomicLevelAt(level),
		Encoding:         "console",
		EncoderConfig:    zap.NewDevelopmentEncoderConfig(),
		OutputPaths:      []string{"stderr"},
		ErrorOutputPaths: []string{"stderr"},
	}
	logger, err := cfg.Build()
	if err != nil {
		panic(err)
	}
	return logger
}

type loggerKey struct{}

func withLogger(ctx context.Context, logger *zap.Logger) context.Context {
	return context.WithValue(ctx, loggerKey{}, logger)
}

func loggerFromContext(ctx context.Context) *zap.Logger {
	if l, ok := ctx.Value(loggerKey{}).(*zap.Logger); ok {
		return l
	}
	return zap.NewNop()
}

func provisionCmd() *cobra.Command {
	cfg := qemu.DefaultConfig()

	cmd := &cobra.Command{
		Use:   "provision",
		Short: "Create a local Kubernetes cluster",
		RunE: func(cmd *cobra.Command, args []string) error {
			logger := loggerFromContext(cmd.Context())
			if err := qemu.CheckPrerequisites(); err != nil {
				return err
			}
			cluster, err := qemu.Provision(cmd.Context(), cfg, logger)
			if err != nil {
				return err
			}
			logger.Info("cluster ready",
				zap.String("ssh", fmt.Sprintf("ssh -p %s -i %s ystack@localhost",
					cfg.SSHPort, filepath.Join(cfg.CacheDir, cfg.Name+"-ssh"))),
			)
			_ = cluster // cluster is running, caller can now converge
			return nil
		},
	}

	cmd.Flags().StringVar(&cfg.Name, "name", cfg.Name, "VM name")
	cmd.Flags().StringVar(&cfg.DiskSize, "disk-size", cfg.DiskSize, "disk size")
	cmd.Flags().StringVar(&cfg.Memory, "memory", cfg.Memory, "memory in MB")
	cmd.Flags().StringVar(&cfg.CPUs, "cpus", cfg.CPUs, "CPU count")
	cmd.Flags().StringVar(&cfg.SSHPort, "ssh-port", cfg.SSHPort, "host SSH port")
	cmd.Flags().StringVar(&cfg.Context, "context", cfg.Context, "kubeconfig context name")
	return cmd
}

func teardownCmd() *cobra.Command {
	cfg := qemu.DefaultConfig()
	var keepDisk bool

	cmd := &cobra.Command{
		Use:   "teardown",
		Short: "Stop and remove the local cluster",
		RunE: func(cmd *cobra.Command, args []string) error {
			logger := loggerFromContext(cmd.Context())
			return qemu.TeardownConfig(cfg, keepDisk, logger)
		},
	}

	cmd.Flags().StringVar(&cfg.Name, "name", cfg.Name, "VM name")
	cmd.Flags().BoolVar(&keepDisk, "keep-disk", false, "preserve disk image for faster re-provision")
	return cmd
}

func exportCmd() *cobra.Command {
	var diskPath string

	cmd := &cobra.Command{
		Use:   "export <output.vmdk>",
		Short: "Export the cluster disk as a VMware appliance",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if diskPath == "" {
				cfg := qemu.DefaultConfig()
				diskPath = filepath.Join(cfg.CacheDir, cfg.Name+".qcow2")
			}
			return qemu.ExportVMDK(diskPath, args[0])
		},
	}

	cmd.Flags().StringVar(&diskPath, "disk", "", "path to qcow2 disk (default: auto-detect from config)")
	return cmd
}

func importCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "import <input.vmdk>",
		Short: "Import a VMware appliance as the cluster disk",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg := qemu.DefaultConfig()
			diskPath := filepath.Join(cfg.CacheDir, cfg.Name+".qcow2")
			return qemu.ImportVMDK(args[0], diskPath)
		},
	}
	return cmd
}
