package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
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
	cmd.AddCommand(imagesPushCmd())
	return cmd
}

func imagesListCmd() *cobra.Command {
	var contextName string
	var format string
	var sortKey string

	cmd := &cobra.Command{
		Use:   "list [<yaml-file>|-]",
		Short: "Print images referenced by a YAML stream or stored in a cluster",
		Long: `Two input modes; mutually exclusive.

YAML mode (positional argument):
  <path>   read the file at path
  -        read stdin
  Prints every image reference found in any PodSpec
  (Deployment, StatefulSet, DaemonSet, Job, CronJob, ReplicaSet,
  Pod). Output is sorted, deduplicated, one ref per line —
  suitable for piping to xargs or a downstream tool.
  Pipe a kustomize build through it:
    kubectl kustomize ./base | y-cluster images list -

Cluster mode (--context=<ctx>):
  Queries the cluster's containerd k8s.io namespace and prints
  one row per stored manifest, sorted by descending compressed
  size by default. Digest-aliases of the same manifest are
  collapsed (no double-count). Use this to answer "what's
  taking the space in this appliance qcow2".
  Default output is a SIZE/IMAGE table; --format=json emits
  [{ref, digest, size_bytes, size_human}]. --sort=name switches
  to alphabetical.

Exit codes: 0 on success, 1 on YAML parse / I/O / cluster
error, 2 on usage.`,
		Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if len(args) > 0 && contextName != "" {
				return fmt.Errorf("--context is mutually exclusive with positional input")
			}
			if len(args) == 0 && contextName == "" {
				return fmt.Errorf("specify a positional input (<path>|-) or --context=<ctx>")
			}
			if contextName != "" {
				return runListFromCluster(cmd, contextName, format, sortKey)
			}
			r, closer, err := openInput(cmd.Context(), args[0], cmd.InOrStdin())
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
	cmd.Flags().StringVar(&contextName, "context", "", "kubeconfig context — query the cluster's containerd (mutex with positional input)")
	cmd.Flags().StringVar(&format, "format", "table", "cluster mode output format: table|json")
	cmd.Flags().StringVar(&sortKey, "sort", "size", "cluster mode sort key: size (desc) | name (asc)")
	return cmd
}

func runListFromCluster(cmd *cobra.Command, contextName, format, sortKey string) error {
	ctx := cmd.Context()
	lr, err := cluster.Lookup(ctx, "", contextName)
	if err != nil {
		return err
	}
	rows, err := images.ListFromCluster(ctx, lr)
	if err != nil {
		return err
	}
	switch sortKey {
	case "", "size":
		images.SortClusterImagesBySizeDesc(rows)
	case "name":
		images.SortClusterImagesByName(rows)
	default:
		return fmt.Errorf("--sort: unknown value %q (size|name)", sortKey)
	}
	out := cmd.OutOrStdout()
	switch format {
	case "", "table":
		return writeClusterImagesTable(out, rows)
	case "json":
		enc := json.NewEncoder(out)
		enc.SetIndent("", "  ")
		return enc.Encode(rows)
	default:
		return fmt.Errorf("--format: unknown value %q (table|json)", format)
	}
}

// writeClusterImagesTable renders one row per stored manifest
// as "SIZE  IMAGE", with the SIZE column padded to the widest
// value so refs line up. Matches the spec's example output.
func writeClusterImagesTable(w io.Writer, rows []images.ClusterImage) error {
	sizeWidth := len("SIZE")
	for _, r := range rows {
		if l := len(r.SizeHuman); l > sizeWidth {
			sizeWidth = l
		}
	}
	if _, err := fmt.Fprintf(w, "%-*s  %s\n", sizeWidth, "SIZE", "IMAGE"); err != nil {
		return err
	}
	for _, r := range rows {
		if _, err := fmt.Fprintf(w, "%-*s  %s\n", sizeWidth, r.SizeHuman, r.Ref); err != nil {
			return err
		}
	}
	return nil
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
		Use:   "load <ref|path|-|url>",
		Short: "Ensure an image is in the cluster's containerd",
		Long: `Make an image available in the cluster's containerd. The single
positional argument dispatches by leading character (same shape
as docker build / git clone):

  ./...          a local path (relative -- MUST start with "./")
  /...           a local path (absolute)
  ~/...          a local path (home-relative)
  -              stdin (an OCI archive)
  http(s)://...  an OCI archive fetched over HTTP
  <other>        a remote image reference

Bare names without "./" are remote refs by rule. A directory
called "myimage" in CWD is reached as "./myimage" --
"myimage" alone is dispatched as a remote registry ref and
fails fast. The rule is unambiguous because container references
can't legally start with ./, /, or ~/ per the OCI distribution
spec, and never carry an http(s):// scheme.

Path inputs:
  - file: an already-tarred OCI archive
  - directory: an OCI v1 layout (tar streamed on the fly,
    equivalent to ` + "`tar -cf - -C <dir> . | y-cluster images load -`" + `)

URL inputs stream the HTTP response body into the same code
path that file and stdin input use -- the byte stream is the
contract. The motivating case is a Hetzner S3 blob URL: one
less ` + "`curl ... | y-cluster images load -`" + ` shell-pipe in the
workflow. Re-running re-downloads; no cache is touched.

Remote refs (default flow): resolve digest, check the cluster's
k8s.io namespace -- if the digest is already indexed, no-op.
Otherwise pull into the y-cluster shared cache (idempotent,
dedup by digest, reused on the next load to another cluster)
and stream to ` + "`ctr image import`" + `. Pass --cache=false to skip
the persistent cache (pull into a tempdir, load, throw the
tempdir away); rejected for path / stdin / url input where the
cache is never involved.

Cluster discovery uses --context (default "local"): docker
backends import via the daemon API, the qemu / hetzner backends
via SSH. No SSH/docker-exec bytes are wasted on a re-deploy of
a digest the cluster already has.

Examples:
  y-cluster images load registry.k8s.io/pause:3.10
  y-cluster images load registry.k8s.io/pause:3.10 --cache=false
  y-cluster images load ./myimage/target-oci
  y-cluster images load /tmp/builds/myapp.tar
  cat /tmp/builds/myapp.tar | y-cluster images load -
  y-cluster images load https://hel1.your-objectstorage.com/bucket/myapp.tar`,
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
			case isURLArg(arg):
				if !useCache {
					return fmt.Errorf("--cache=false is incompatible with url input %q (the stream is never cached)", arg)
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
			case isURLArg(arg):
				r, closer, err := openInput(ctx, arg, c.InOrStdin())
				if err != nil {
					return err
				}
				defer closer()
				return images.Load(ctx, lr, r, logger)
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

// isURLArg classifies the positional argument as an http(s) URL
// input. Registry refs never carry a scheme, so this check does
// not collide with remote-ref dispatch as long as it runs before
// the remote-ref default.
func isURLArg(arg string) bool {
	return strings.HasPrefix(arg, "http://") || strings.HasPrefix(arg, "https://")
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

// openInput resolves the positional input arg to an io.Reader
// plus a deferred-close callback. Three modes by prefix detection:
//
//   - "-"                       → stdin (no close)
//   - "http://" / "https://"    → HTTP GET, stream the response body
//   - anything else             → file open
//
// We expose the close callback (rather than just an io.ReadCloser)
// because stdin must not be Close()d -- the test runner reuses it.
//
// Used by `images list` (YAML stream), `images load` (OCI
// archive, url input), and `manifests add` (kustomize YAML). The
// shared helper means a Hetzner S3 blob URL works as input to any
// of them; the byte stream is the contract, not the content type.
//
// HTTP errors (non-2xx) close the body and return a clear error
// rather than letting the consumer parse error HTML as the
// expected content.
func openInput(ctx context.Context, arg string, stdin io.Reader) (io.Reader, func(), error) {
	if arg == "-" {
		return stdin, func() {}, nil
	}
	if isURLArg(arg) {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, arg, nil)
		if err != nil {
			return nil, nil, fmt.Errorf("build GET %s: %w", arg, err)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			return nil, nil, fmt.Errorf("GET %s: %w", arg, err)
		}
		if resp.StatusCode/100 != 2 {
			body, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
			_ = resp.Body.Close()
			return nil, nil, fmt.Errorf("GET %s: %s: %s", arg, resp.Status, strings.TrimSpace(string(body)))
		}
		return resp.Body, func() { _ = resp.Body.Close() }, nil
	}
	f, err := os.Open(arg)
	if err != nil {
		return nil, nil, err
	}
	return f, func() { _ = f.Close() }, nil
}
