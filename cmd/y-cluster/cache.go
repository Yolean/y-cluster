package main

import (
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"

	"github.com/spf13/cobra"

	"github.com/Yolean/y-cluster/pkg/cache"
)

// cacheCmd is the `y-cluster cache` subcommand group: introspect
// and reset the on-disk download root that other commands write
// to (see pkg/cache).
func cacheCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "cache",
		Short: "Inspect or purge y-cluster's download cache",
	}
	cmd.AddCommand(cacheInfoCmd())
	cmd.AddCommand(cachePurgeCmd())
	return cmd
}

func cacheInfoCmd() *cobra.Command {
	var cacheDir string
	var pathOnly bool

	cmd := &cobra.Command{
		Use:   "info",
		Short: "Print the cache root and per-subtree disk usage",
		Long: `By default prints a small report:

  root:   <resolved cache root>
  images: <bytes>
  k3s:    <bytes>

With -p / --path, prints only the resolved root on stdout, so
shell scripts can do  ROOT=$(y-cluster cache info -p)  and act
on it.`,
		RunE: func(cmd *cobra.Command, args []string) error {
			root, err := cache.Root(cacheDir)
			if err != nil {
				return err
			}
			out := cmd.OutOrStdout()
			if pathOnly {
				fmt.Fprintln(out, root)
				return nil
			}
			subtrees, err := cache.Subtrees(cacheDir)
			if err != nil {
				return err
			}
			fmt.Fprintf(out, "root:   %s\n", root)
			for _, st := range subtrees {
				// Padded to the width scripts already parse
				// ("images: ", "k3s:    "); longer names get one space.
				fmt.Fprintf(out, "%-7s %s\n", st.Name+":", humanBytes(dirSize(st.Path)))
			}
			return nil
		},
	}
	cmd.Flags().StringVar(&cacheDir, "cache-dir", "", "override cache root (also: $Y_CLUSTER_CACHE_DIR)")
	cmd.Flags().BoolVarP(&pathOnly, "path", "p", false, "print only the cache root path")
	return cmd
}

func cachePurgeCmd() *cobra.Command {
	var cacheDir string
	var images, k3s, envoyGateway, all bool

	cmd := &cobra.Command{
		Use:   "purge",
		Short: "Delete cached artefacts. Requires --images, --k3s, --envoygateway, or --all.",
		Long: `Removes cache subtrees from disk. The flags must be explicit so
adding a new subtree later doesn't silently expand "purge" to it:

  --images         delete <root>/images/
  --k3s            delete <root>/k3s/
  --envoygateway   delete <root>/envoygateway/
  --all            delete every subtree the running binary knows about

Bare 'cache purge' (no flag) exits non-zero with a usage error.
Combine flags to delete several subtrees in one invocation.`,
		RunE: func(cmd *cobra.Command, args []string) error {
			selected := map[string]bool{"images": images, "k3s": k3s, "envoygateway": envoyGateway}
			if !images && !k3s && !envoyGateway && !all {
				return fmt.Errorf("specify --images, --k3s, --envoygateway, or --all")
			}
			subtrees, err := cache.Subtrees(cacheDir)
			if err != nil {
				return err
			}
			out := cmd.OutOrStdout()
			for _, st := range subtrees {
				if !all && !selected[st.Name] {
					continue
				}
				if err := purgeDir(out, st.Path); err != nil {
					return err
				}
			}
			return nil
		},
	}
	cmd.Flags().StringVar(&cacheDir, "cache-dir", "", "override cache root (also: $Y_CLUSTER_CACHE_DIR)")
	cmd.Flags().BoolVar(&images, "images", false, "delete the images subtree")
	cmd.Flags().BoolVar(&k3s, "k3s", false, "delete the k3s subtree")
	cmd.Flags().BoolVar(&envoyGateway, "envoygateway", false, "delete the envoygateway subtree")
	cmd.Flags().BoolVar(&all, "all", false, "delete every known subtree")
	return cmd
}

// purgeDir removes path. A non-existent path is treated as a
// no-op so `purge` is idempotent -- we don't want re-runs to
// error just because the first one already cleaned up.
func purgeDir(w io.Writer, path string) error {
	if _, err := os.Stat(path); err != nil {
		if os.IsNotExist(err) {
			fmt.Fprintf(w, "skip %s (not present)\n", path)
			return nil
		}
		return err
	}
	if err := os.RemoveAll(path); err != nil {
		return fmt.Errorf("remove %s: %w", path, err)
	}
	fmt.Fprintf(w, "removed %s\n", path)
	return nil
}

// dirSize returns the total bytes used by path and its
// descendants. Returns 0 (not an error) when path doesn't
// exist; cache info on a fresh machine should print "0 B" not
// fail.
func dirSize(path string) int64 {
	var total int64
	_ = filepath.WalkDir(path, func(_ string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if d.IsDir() {
			return nil
		}
		info, err := d.Info()
		if err != nil {
			return nil
		}
		total += info.Size()
		return nil
	})
	return total
}

// humanBytes formats n with a binary unit suffix (KiB/MiB/GiB).
// Cache contents are typically megabytes, so a coarse-grained
// presentation is enough.
func humanBytes(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := int64(unit), 0
	for n/div >= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %ciB", float64(n)/float64(div), "KMGTPE"[exp])
}
