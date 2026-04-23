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

	"github.com/Yolean/y-cluster/pkg/yconverge"
)

var version = "dev"

func main() {
	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()

	if err := rootCmd().ExecuteContext(ctx); err != nil {
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

	return root
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
