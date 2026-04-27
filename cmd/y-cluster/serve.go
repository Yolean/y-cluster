package main

import (
	"fmt"
	"strings"

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
assets, one port per -c config directory. Default operation is
in the background (process detaches, ` + "`serve ensure`" + ` waits for
/health to come up before returning); --foreground keeps the
server attached to the current shell with console-style logs.

Three backend types, selected via "type:" in y-cluster-serve.yaml:

  y-kustomize-local      Run kustomize build on each "sources[].dir";
                         every Secret named y-kustomize.{group}.{name}
                         in the build output is served at
                         /v1/{group}/{name}/{key} per data key.

  y-kustomize-incluster  Watch Kubernetes Secrets in the configured
                         namespace with the same naming convention;
                         changes propagate live via the informer.

  static                 Serve files under "static.dir" rooted at
                         "static.root" (with optional yaml->json on
                         .yaml requests and dir-trailing-slash redirect).

The local and in-cluster backends are interchangeable mirrors:
same Secret naming convention, same URL shape. Switch by editing
"type:" in the config; the source of truth (kustomize build vs
apiserver watch) is the only thing that changes.

Reserved data key: "kustomization.yaml". A Secret data key by
that name would mislead consumers into fetching the URL as a
kustomize base, which doesn't work over HTTP. Local mode
fails-fast with the offending key in the error; in-cluster mode
warns and skips the key, leaving the rest of the watch online.

ensure prints typed status to stdout (y-cluster serve started /
restarted / noop, with the resolved port). Errors surface
inline on stderr -- if the daemon refuses startup the error
from its own log is included rather than just "/health timed
out". Use ` + "`serve logs`" + ` for the full daemon log.`,
		RunE: func(cmd *cobra.Command, args []string) error {
			return serve.Run(cmd.Context(), serve.Options{
				ConfigDirs: configDirs,
				Foreground: foreground,
				StateDir:   stateDir,
			})
		},
	}
	cmd.Flags().StringArrayVarP(&configDirs, "config", "c", nil, "directory containing y-cluster-serve.yaml (repeatable)")
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
			res, err := serve.Ensure(cmd.Context(), serve.Options{
				ConfigDirs: configDirs,
				StateDir:   stateDir,
			})
			if err != nil {
				// Errors stay on stderr so a developer who
				// pipes `serve ensure` into something else
				// still sees diagnostics on the tty.
				return err
			}
			// Success status to stdout: makes the line scriptable
			// (`if y-cluster serve ensure ... | grep -q started`)
			// and keeps stderr free for warnings the daemon may
			// emit later.
			fmt.Fprintf(cmd.OutOrStdout(),
				"y-cluster serve %s on %s\n",
				res.Action, formatPorts(res.Ports))
			return nil
		},
	}
	cmd.Flags().StringArrayVarP(&configDirs, "config", "c", nil, "directory containing y-cluster-serve.yaml (repeatable)")
	cmd.Flags().StringVar(&stateDir, "state-dir", "", "override the per-user state directory")
	if err := cmd.MarkFlagRequired("config"); err != nil {
		panic(err)
	}
	return cmd
}

// formatPorts renders ports as ":N" for one port or ":N, :M, ..."
// for several. Single-port is the common case so we optimise the
// reading.
func formatPorts(ports []int) string {
	if len(ports) == 1 {
		return fmt.Sprintf(":%d", ports[0])
	}
	parts := make([]string, len(ports))
	for i, p := range ports {
		parts[i] = fmt.Sprintf(":%d", p)
	}
	return "ports " + strings.Join(parts, ", ")
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
