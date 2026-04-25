package main

import (
	"fmt"

	"github.com/spf13/cobra"

	"github.com/Yolean/y-cluster/pkg/serve"
)

// serveCmd wires the `y-cluster serve` subcommands. The CLI is a thin
// adapter — every action delegates to pkg/serve.
func serveCmd() *cobra.Command {
	var (
		configDirs []string
		foreground bool
		stateDir   string
	)

	cmd := &cobra.Command{
		Use:   "serve",
		Short: "HTTP server for config assets (y-kustomize bases, static dirs)",
		Long: `y-cluster serve brings up a lightweight HTTP server for config
assets, one port per -c config directory. Default operation is in the
background; --foreground keeps the server attached to the current shell.`,
		RunE: func(cmd *cobra.Command, args []string) error {
			return serve.Run(cmd.Context(), serve.Options{
				ConfigDirs: configDirs,
				Foreground: foreground,
				StateDir:   stateDir,
			})
		},
	}
	cmd.Flags().StringArrayVarP(&configDirs, "config", "c", nil, "config directory (repeatable)")
	cmd.Flags().BoolVar(&foreground, "foreground", false, "run in the foreground instead of detaching")
	cmd.Flags().StringVar(&stateDir, "state-dir", "", "override the per-user state directory")
	if err := cmd.MarkFlagRequired("config"); err != nil {
		panic(err)
	}

	cmd.AddCommand(serveEnsureCmd())
	cmd.AddCommand(serveStopCmd())
	cmd.AddCommand(serveLogsCmd())
	return cmd
}

func serveEnsureCmd() *cobra.Command {
	var (
		configDirs []string
		stateDir   string
	)
	cmd := &cobra.Command{
		Use:   "ensure",
		Short: "Start the serve daemon if it is not running or the config has changed",
		RunE: func(cmd *cobra.Command, args []string) error {
			started, err := serve.Ensure(cmd.Context(), serve.Options{
				ConfigDirs: configDirs,
				StateDir:   stateDir,
			})
			if err != nil {
				return err
			}
			if started {
				fmt.Fprintln(cmd.ErrOrStderr(), "y-cluster serve started")
			} else {
				fmt.Fprintln(cmd.ErrOrStderr(), "y-cluster serve already running")
			}
			return nil
		},
	}
	cmd.Flags().StringArrayVarP(&configDirs, "config", "c", nil, "config directory (repeatable)")
	cmd.Flags().StringVar(&stateDir, "state-dir", "", "override the per-user state directory")
	if err := cmd.MarkFlagRequired("config"); err != nil {
		panic(err)
	}
	return cmd
}

func serveStopCmd() *cobra.Command {
	var stateDir string
	cmd := &cobra.Command{
		Use:   "stop",
		Short: "Stop the running serve daemon",
		RunE: func(cmd *cobra.Command, args []string) error {
			return serve.Stop(cmd.Context(), stateDir)
		},
	}
	cmd.Flags().StringVar(&stateDir, "state-dir", "", "override the per-user state directory")
	return cmd
}

func serveLogsCmd() *cobra.Command {
	var (
		stateDir string
		follow   bool
	)
	cmd := &cobra.Command{
		Use:   "logs",
		Short: "Print the serve daemon log file",
		RunE: func(cmd *cobra.Command, args []string) error {
			return serve.Logs(cmd.Context(), cmd.OutOrStdout(), stateDir, follow)
		},
	}
	cmd.Flags().StringVar(&stateDir, "state-dir", "", "override the per-user state directory")
	cmd.Flags().BoolVarP(&follow, "follow", "f", false, "follow the log file like `tail -f`")
	return cmd
}
