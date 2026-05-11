package main

import (
	"context"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/spf13/cobra"
	"go.uber.org/zap"

	"github.com/Yolean/y-cluster/pkg/cache"
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
	var cacheDir string
	useCache := true

	cmd := &cobra.Command{
		Use:   "load <ref|path|->",
		Short: "Ensure an image is in the cluster's containerd",
		Long: `Make an image available in the cluster's containerd. The single
positional argument dispatches by leading character (same shape
as docker build / git clone):

  ./...     a local path (relative -- MUST start with "./")
  /...      a local path (absolute)
  ~/...     a local path (home-relative)
  -         stdin (an OCI archive)
  <other>   a remote image reference

Bare names without "./" are remote refs by rule. A directory
called "myimage" in CWD is reached as "./myimage" --
"myimage" alone is dispatched as a remote registry ref and
fails fast. The rule is unambiguous because container references
can't legally start with ./, /, or ~/ per the OCI distribution
spec.

Path inputs:
  - file: an already-tarred OCI archive
  - directory: an OCI v1 layout (tar streamed on the fly,
    equivalent to ` + "`tar -cf - -C <dir> . | y-cluster images load -`" + `)

Remote refs (default flow): resolve digest, check the cluster's
k8s.io namespace -- if the digest is already indexed, no-op.
Otherwise pull into the y-cluster shared cache (idempotent,
dedup by digest, reused on the next load to another cluster)
and stream to ` + "`ctr image import`" + `. Pass --cache=false to skip
the persistent cache (pull into a tempdir, load, throw the
tempdir away); rejected for path / stdin input where the caller
already owns the bytes.

Cluster discovery uses --context (default "local"): docker
backends import via the daemon API, qemu backends via SSH. No
SSH/docker-exec bytes are wasted on a re-deploy of a digest the
cluster already has.

Examples:
  y-cluster images load registry.k8s.io/pause:3.10
  y-cluster images load registry.k8s.io/pause:3.10 --cache=false
  y-cluster images load ./myimage/target-oci
  y-cluster images load /tmp/builds/myapp.tar
  cat /tmp/builds/myapp.tar | y-cluster images load -`,
		Args: cobra.ExactArgs(1),
		RunE: func(c *cobra.Command, args []string) error {
			ctx := c.Context()
			logger := loggerFromContext(ctx)
			arg := args[0]
			// Validate the input-flag combo BEFORE touching the
			// kubeconfig -- a misuse error should fire even when
			// the context isn't reachable, so the operator sees
			// the real problem first.
			switch {
			case arg == "-":
				if !useCache {
					return fmt.Errorf("--cache=false is incompatible with stdin input")
				}
			case isPathArg(arg):
				if !useCache {
					return fmt.Errorf("--cache=false is incompatible with path input %q (caller already owns the bytes)", arg)
				}
			}
			lr, err := cluster.Lookup(ctx, "", contextName)
			if err != nil {
				return err
			}
			switch {
			case arg == "-":
				return images.Load(ctx, lr, c.InOrStdin(), logger)
			case isPathArg(arg):
				return loadFromPath(ctx, lr, arg, logger)
			default:
				return loadFromRef(ctx, lr, arg, cacheDir, useCache, logger)
			}
		},
	}
	cmd.Flags().StringVar(&contextName, "context", cluster.DefaultContext, "kubeconfig context name")
	cmd.Flags().StringVar(&cacheDir, "cache-dir", "", "override cache root (also: $Y_CLUSTER_CACHE_DIR) -- only honoured for remote-ref input")
	cmd.Flags().BoolVar(&useCache, "cache", true, "for remote-ref input, keep the pulled layout in the shared cache for reuse on the next load to another cluster")
	return cmd
}

// isPathArg classifies the positional argument as a path
// (vs. a remote registry ref). Container references can't
// legally start with these characters per the OCI distribution
// spec -- hosts never lead with `.`, `/`, or `~` -- so the rule
// is unambiguous. Bare names like "nginx" are remote refs;
// callers who want the local "nginx" directory write "./nginx",
// matching standard shell hygiene. The `~` rule is mostly
// documentation -- the shell expands tilde before exec so the
// program usually sees an absolute path -- but we honour it
// when the shell hasn't expanded for any reason.
func isPathArg(arg string) bool {
	return strings.HasPrefix(arg, "./") ||
		strings.HasPrefix(arg, "/") ||
		strings.HasPrefix(arg, "~/") ||
		arg == "." || arg == ".."
}

// loadFromPath dispatches an existing-path input to either the
// archive (file) or OCI-layout (dir) variant of Load. Errors
// clearly when the path doesn't exist so the operator doesn't
// have to guess whether they typo'd.
func loadFromPath(ctx context.Context, lr *cluster.LookupResult, path string, logger *zap.Logger) error {
	st, err := os.Stat(path)
	if err != nil {
		return err
	}
	if st.IsDir() {
		r := images.TarOCIDirReader(path)
		defer func() { _ = r.Close() }()
		return images.Load(ctx, lr, r, logger)
	}
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer func() { _ = f.Close() }()
	return images.Load(ctx, lr, f, logger)
}

// loadFromRef implements the "ref -> cluster" flow: resolve
// digest, skip if the cluster already has it, otherwise pull
// and stream to containerd. The cache root is either the
// shared y-cluster cache (useCache=true) or a tempdir cleaned
// up after the load (useCache=false). Same code path for both
// -- the only difference is the root and whether we
// RemoveAll at the end. No second cache structure.
func loadFromRef(ctx context.Context, lr *cluster.LookupResult, ref, cacheDir string, useCache bool, logger *zap.Logger) error {
	// Resolve digest first so we can ask the cluster whether
	// it already has the image -- and skip the pull entirely
	// when the answer is yes. Tag input HEADs; digest input
	// is offline-safe.
	digestRef, err := images.ResolveDigest(ctx, ref)
	if err != nil {
		return err
	}
	logger.Info("resolved ref",
		zap.String("input", ref),
		zap.String("digest", digestRef),
	)
	if images.PresentInCluster(ctx, lr, digestRef) {
		logger.Info("image already in cluster; skipping import",
			zap.String("ref", digestRef),
		)
		return nil
	}

	root := cacheDir
	if !useCache {
		tmp, err := os.MkdirTemp("", "y-cluster-images-load-")
		if err != nil {
			return err
		}
		defer func() { _ = os.RemoveAll(tmp) }()
		root = tmp
	}

	if _, err := images.Cache(ctx, ref, root, logger); err != nil {
		return err
	}

	// Derive the layout directory the cache just wrote. The
	// helper centralises the path shape so a future cache
	// backend swap (e.g. embedded registry:3 in 2.0) touches
	// one helper, not the load path here.
	digest := digestRef
	if at := strings.LastIndex(digest, "@"); at >= 0 {
		digest = digest[at+1:]
	}
	dir, err := cache.ImageLayout(root, digest)
	if err != nil {
		return err
	}
	r := images.TarOCIDirReader(dir)
	defer func() { _ = r.Close() }()
	return images.Load(ctx, lr, r, logger)
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
