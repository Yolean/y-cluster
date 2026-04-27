package main

import (
	"fmt"
	"io"
	"os"

	"github.com/spf13/cobra"

	"github.com/Yolean/y-cluster/pkg/cluster"
	"github.com/Yolean/y-cluster/pkg/images"
)

// imagesCmd is the `y-cluster images` subcommand group: list →
// cache → load. List extracts refs from a YAML stream; cache
// pulls one ref into the shared OCI cache; load imports a local
// OCI archive into the cluster's containerd.
func imagesCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "images",
		Short: "List, cache, and load images referenced by Kubernetes YAML",
	}
	cmd.AddCommand(imagesListCmd())
	cmd.AddCommand(imagesCacheCmd())
	cmd.AddCommand(imagesLoadCmd())
	return cmd
}

func imagesListCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "list <yaml-file|->",
		Short: "Print every container image referenced by a Kubernetes YAML stream",
		Long: `Read a YAML stream and print every image reference found in any
PodSpec (Deployment, StatefulSet, DaemonSet, Job, CronJob, ReplicaSet,
Pod). Output is sorted, deduplicated, one ref per line — suitable for
piping to xargs or a downstream tool.

Input source is a positional argument:
  <path>   read the file at path
  -        read stdin

To extract images from a kustomize tree, pipe the build through:
  kubectl kustomize ./base | y-cluster images list -

Exit codes: 0 on success, 1 on YAML parse / I/O error, 2 on usage.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			r, closer, err := openYAMLInput(args[0], cmd.InOrStdin())
			if err != nil {
				return err
			}
			defer closer()
			refs, err := images.ListYAML(r)
			if err != nil {
				return err
			}
			out := cmd.OutOrStdout()
			for _, ref := range refs {
				fmt.Fprintln(out, ref)
			}
			return nil
		},
	}
	return cmd
}

func imagesCacheCmd() *cobra.Command {
	var cacheDir string

	cmd := &cobra.Command{
		Use:   "cache <ref>",
		Short: "Pull a registry reference into the local OCI cache",
		Long: `Pull <ref> into the y-cluster shared image cache (cache.Images()).
Idempotent on the resolved digest: a digest already on disk is a no-op;
tag-only refs HEAD the registry to re-resolve, then no-op when the
resolved digest matches an existing cached layout.

Use cases:
  - feed a one-off ref into the cache so a subsequent load can use it
  - script-driven prefetch (xargs over a list of refs)
  - debugging / inspection (cache info shows the resulting layout)

The cache root is resolved per pkg/cache.Root: --cache-dir wins, then
$Y_CLUSTER_CACHE_DIR, then $XDG_CACHE_HOME/y-cluster, then
~/.cache/y-cluster. Prints the resolved digest reference on stdout
so callers can record what landed.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			logger := loggerFromContext(cmd.Context())
			digestRef, err := images.Cache(cmd.Context(), args[0], cacheDir, logger)
			if err != nil {
				return err
			}
			fmt.Fprintln(cmd.OutOrStdout(), digestRef)
			return nil
		},
	}
	cmd.Flags().StringVar(&cacheDir, "cache-dir", "", "override cache root (also: $Y_CLUSTER_CACHE_DIR)")
	return cmd
}

func imagesLoadCmd() *cobra.Command {
	var contextName string

	cmd := &cobra.Command{
		Use:   "load <archive|->",
		Short: "Stream an OCI archive into the cluster node's containerd",
		Long: `Open <archive> (a local file path, or "-" for stdin) and pipe its
bytes into ` + "`ctr -n k8s.io image import -`" + ` on the cluster node. The
ref + tag preserved on the loaded image are whatever the archive's
manifest carries — same as running ctr image import on the node
directly. No cache is touched, which is what callers want when their
build pipelines (e.g. ` + "`contain`" + ` outputting tarballs to /tmp) own
the lifecycle of the archive bytes.

Cluster discovery uses --context (default "local"): the docker
backend imports via the daemon API, the qemu backend via SSH.

Examples:
  y-cluster images load /tmp/builds/myapp.tar
  cat /tmp/builds/myapp.tar | y-cluster images load -
  for f in builds/*.tar; do y-cluster images load "$f"; done`,
		Args: cobra.ExactArgs(1),
		RunE: func(c *cobra.Command, args []string) error {
			r, closer, err := openYAMLInput(args[0], c.InOrStdin())
			if err != nil {
				return err
			}
			defer closer()
			lr, err := cluster.Lookup(c.Context(), "", contextName)
			if err != nil {
				return err
			}
			return images.Load(c.Context(), lr, r, loggerFromContext(c.Context()))
		},
	}
	cmd.Flags().StringVar(&contextName, "context", cluster.DefaultContext, "kubeconfig context name")
	return cmd
}

// openYAMLInput resolves the positional input arg ("<path>" or
// "-") to an io.Reader plus a deferred-close callback. We expose
// the close callback (rather than just an io.ReadCloser) because
// stdin must not be Close()d — the test runner reuses it.
//
// Used by both `images list` (yaml stream) and `images load`
// (oci archive) — same input semantics, different content.
func openYAMLInput(arg string, stdin io.Reader) (io.Reader, func(), error) {
	if arg == "-" {
		return stdin, func() {}, nil
	}
	f, err := os.Open(arg)
	if err != nil {
		return nil, nil, err
	}
	return f, func() { _ = f.Close() }, nil
}
