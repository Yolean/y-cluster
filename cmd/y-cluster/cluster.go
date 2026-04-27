package main

import (
	"context"
	"fmt"
	"io"
	"os"

	"github.com/spf13/cobra"

	"github.com/Yolean/y-cluster/pkg/cluster"
)

// detectCmd implements `y-cluster detect`. It replaces ystack's
// y-cluster-local-detect:
//
//   - With no positional arg, prints the detected backend (one
//     of: docker, qemu) on stdout.
//   - With a positional arg matching the detected backend, prints
//     "up" and exits 0. Any other arg exits non-zero — the bash
//     equivalent script returned 1 to signal "the asked-about
//     provider isn't running".
//
// --context defaults to "local" — the convention shared with
// ystack and y-cluster's CommonConfig.Context default.
func detectCmd() *cobra.Command {
	var contextName string
	cmd := &cobra.Command{
		Use:   "detect [backend]",
		Short: "Detect which provisioner serves the cluster behind the kubeconfig context",
		Long: `Reads the kubeconfig cluster name for --context (default "local") and
probes each supported backend (docker, qemu) to find which one is
running. Prints the backend on stdout, or — when called with a
positional argument equal to the detected backend — prints "up" and
exits 0. Any other positional value exits non-zero.

Replaces ystack's y-cluster-local-detect.`,
		Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			lr, err := cluster.Lookup(cmd.Context(), "", contextName)
			if err != nil {
				return err
			}
			out := cmd.OutOrStdout()
			if len(args) == 0 {
				fmt.Fprintln(out, lr.Backend)
				return nil
			}
			if args[0] != string(lr.Backend) {
				return fmt.Errorf("expected %q, detected %q", args[0], lr.Backend)
			}
			fmt.Fprintln(out, "up")
			return nil
		},
	}
	cmd.Flags().StringVar(&contextName, "context", cluster.DefaultContext, "kubeconfig context name")
	return cmd
}

// nodeRunFn is the shared signature of cluster.RunCtr and
// cluster.RunCrictl so ctrCmd / crictlCmd share one builder.
type nodeRunFn func(ctx context.Context, lr *cluster.LookupResult, args []string, stdin io.Reader, stdout, stderr io.Writer) error

// ctrCmd implements `y-cluster ctr [-- args...]`. Routes to
// `docker exec -i <container> ctr ...` or `ssh ... sudo k3s ctr
// ...` depending on the detected backend. stdin/stdout/stderr
// are passthrough so e.g. `cat archive.tar | y-cluster ctr image
// import -` works without buffering.
//
// Replaces ystack's y-cluster-local-ctr.
func ctrCmd() *cobra.Command {
	return nodeBinaryCmd("ctr", cluster.RunCtr)
}

// crictlCmd is ctrCmd's sibling for crictl. Replaces ystack's
// y-cluster-local-crictl.
func crictlCmd() *cobra.Command {
	return nodeBinaryCmd("crictl", cluster.RunCrictl)
}

func nodeBinaryCmd(name string, run nodeRunFn) *cobra.Command {
	var contextName string
	cmd := &cobra.Command{
		Use:   name + " [-- args...]",
		Short: "Run " + name + " on the local cluster's node",
		Long: `Routes <args> to the cluster node's ` + name + `:
  docker backend: docker exec -i <container> ` + name + ` <args>
  qemu   backend: ssh ystack@127.0.0.1 sudo k3s ` + name + ` <args>

stdin / stdout / stderr are passthrough — pipes work end to end.
--context defaults to "local". Use -- to forward flags that
` + name + ` itself accepts (e.g. y-cluster ` + name + ` -- --help).`,
		// We want every positional we get (including unknown
		// flags after `--`) forwarded to the remote binary.
		DisableFlagParsing: false,
		RunE: func(cmd *cobra.Command, args []string) error {
			lr, err := cluster.Lookup(cmd.Context(), "", contextName)
			if err != nil {
				return err
			}
			if err := run(cmd.Context(), lr, args, os.Stdin, cmd.OutOrStdout(), cmd.ErrOrStderr()); err != nil {
				return fmt.Errorf("%s: %w", name, err)
			}
			return nil
		},
	}
	cmd.Flags().StringVar(&contextName, "context", cluster.DefaultContext, "kubeconfig context name")
	return cmd
}
