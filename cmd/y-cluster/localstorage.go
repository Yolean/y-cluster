package main

import (
	"github.com/spf13/cobra"

	"github.com/Yolean/y-cluster/pkg/provision/config"
	"github.com/Yolean/y-cluster/pkg/provision/localstorage"
)

// localstorageCmd groups the bundled-local-path-provisioner
// subcommands. Today only `render` is implemented; `apply`
// could land later if a build tool wants to skip the y-cluster
// provision flow but still install the same StorageClass.
//
// The flag defaults match CommonConfig.Storage so a render with
// no flags produces the exact manifest the Go-side provisioners
// install (path /data/yolean, predictable PVC namespace_name
// pattern, Retain reclaim).
func localstorageCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "localstorage",
		Short: "Manage the bundled local-path-provisioner install",
	}
	cmd.AddCommand(localstorageRenderCmd())
	return cmd
}

// localstorageRenderCmd prints the rendered manifest to stdout
// with the configured Path / Pattern / ReclaimPolicy. Used by
// the Hetzner Packer driver (scripts/e2e-appliance-hetzner.sh)
// to render once on the build host and ship a single yaml file
// to the build VM, avoiding kustomize / template machinery on
// the build VM side.
func localstorageRenderCmd() *cobra.Command {
	defaults := defaultStorage()
	var (
		path    string
		pattern string
		reclaim string
	)
	cmd := &cobra.Command{
		Use:   "render",
		Short: "Print the rendered local-path-provisioner manifest YAML to stdout (no cluster contact)",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			body, err := localstorage.Render(localstorage.Options{
				Path:          path,
				Pattern:       pattern,
				ReclaimPolicy: reclaim,
			})
			if err != nil {
				return err
			}
			_, err = cmd.OutOrStdout().Write(body)
			return err
		},
	}
	cmd.Flags().StringVar(&path, "path", defaults.Path,
		"nodePathMap default path; where one subdirectory per PV lands on the node")
	cmd.Flags().StringVar(&pattern, "pattern", defaults.PathPattern,
		"per-PV directory pattern (Go text/template against local-path-provisioner helper-pod vars)")
	cmd.Flags().StringVar(&reclaim, "reclaim-policy", defaults.ReclaimPolicy,
		"StorageClass reclaim policy (Retain or Delete)")
	return cmd
}

// defaultStorage returns the same StorageConfig
// applyCommonDefaults installs on a freshly loaded
// y-cluster-provision.yaml. Wired off the QEMU defaulter (any
// provider would yield the same Storage block; QEMU is just
// the most-used one) so the CLI flag defaults track the
// runtime defaults -- one source of truth.
func defaultStorage() config.StorageConfig {
	c := &config.QEMUConfig{CommonConfig: config.CommonConfig{Provider: config.ProviderQEMU}}
	c.ApplyDefaults()
	return c.Storage
}
