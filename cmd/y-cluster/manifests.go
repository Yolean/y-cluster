package main

import (
	"bytes"
	"fmt"
	"io"
	"regexp"
	"strings"

	"github.com/spf13/cobra"

	"github.com/Yolean/y-cluster/pkg/cluster"
)

// manifestsCmd is the umbrella for build-time manifest staging on the
// cluster's appliance disk. Today: just `add`. Future subcommands
// (`list`, `remove`, `get`) read or mutate the same staging dir.
func manifestsCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "manifests",
		Short: "Stage Kubernetes manifests on the cluster's appliance for first-customer-boot apply",
		Long: `Manifests written via this command land in
` + "`/var/lib/y-cluster/manifests-staging/<name>.yaml`" + ` on the
cluster node, NOT on the apiserver. They are NOT applied during
build.

prepare-export moves the staging directory's contents to
` + "`/var/lib/rancher/k3s/server/manifests/`" + ` on the appliance
disk, where k3s auto-applies them on every cluster start. The
customer's first boot of the appliance therefore runs all the
staged manifests against THEIR cluster (with their data), not
against the build cluster.

Typical use: ship a migration Job that runs once on the
customer's first boot of a new appliance version. See
APPLIANCE_MAINTENANCE.md for the recommended Job shape and
idempotency conventions.`,
	}
	cmd.AddCommand(manifestsAddCmd())
	return cmd
}

// manifestNameRE constrains <name> to a portable filename: leading
// alphanumeric (no leading dot or dash), then alphanumerics + dot +
// dash + underscore. Reject path separators, ".." traversal, empty,
// and shell-metacharacters in one regex. The same regex would accept
// a kubectl resource name, which is convenient since the manifest's
// filename and metadata.name typically match.
var manifestNameRE = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9._-]*$`)

func manifestsAddCmd() *cobra.Command {
	var contextName string

	cmd := &cobra.Command{
		Use:   "add <name> <path|->",
		Short: "Stage a manifest for first-customer-boot apply",
		Long: `Reads the YAML at <path> (or stdin when <path> is "-"), then
writes it to ` + "`/var/lib/y-cluster/manifests-staging/<name>.yaml`" + `
on the cluster node. Bails if a manifest with the same <name> is
already staged.

Example:

  y-cluster manifests add migrate-v0.5.0-userdb \
      ./migrations/v0.5.0-userdb.yaml

  cat my-job.yaml | y-cluster manifests add migrate-v0.5.0-userdb -

Naming convention: include the source-target version pair so the
filename is unique across appliance builds. Identical names across
two builds = idempotent re-apply (no-op on the customer's cluster
since k3s remembers the prior apply).`,
		Args: cobra.ExactArgs(2),
		RunE: func(c *cobra.Command, args []string) error {
			name := args[0]
			input := args[1]

			if !manifestNameRE.MatchString(name) {
				return fmt.Errorf("invalid manifest name %q: must match %s (no slashes, no .., must start with alphanumeric)", name, manifestNameRE)
			}

			r, closer, err := openYAMLInput(input, c.InOrStdin())
			if err != nil {
				return err
			}
			defer closer()

			data, err := io.ReadAll(r)
			if err != nil {
				return fmt.Errorf("read manifest: %w", err)
			}
			if len(strings.TrimSpace(string(data))) == 0 {
				return fmt.Errorf("manifest is empty")
			}

			lr, err := cluster.Lookup(c.Context(), "", contextName)
			if err != nil {
				return err
			}

			target := "/var/lib/y-cluster/manifests-staging/" + name + ".yaml"

			// Stage in two RunShell calls: first probe for an
			// existing entry (refuse if found), then atomically
			// write the new one. We don't use a single
			// `cat > <target>` redirect because that'd overwrite
			// silently. install -m 0644 -T also creates the
			// parent dir's permissions cleanly.
			if cluster.RunShell(c.Context(), lr,
				"test ! -e "+target, nil, nil, nil) != nil {
				return fmt.Errorf("manifest %q already staged at %s; remove it first or pick a different name", name, target)
			}

			// Use install(1) to create the parent dir and write
			// the file atomically with a known mode. /dev/stdin
			// is the standard way to feed install(1) bytes from
			// the pipe.
			writeCmd := "install -d -m 0755 /var/lib/y-cluster/manifests-staging && " +
				"install -m 0644 /dev/stdin " + target
			var stderr bytes.Buffer
			if err := cluster.RunShell(c.Context(), lr, writeCmd,
				bytes.NewReader(data), nil, &stderr); err != nil {
				return fmt.Errorf("write manifest: %s: %w", stderr.String(), err)
			}

			fmt.Fprintf(c.OutOrStdout(), "staged manifest %q -> %s (%d bytes)\n", name, target, len(data))
			return nil
		},
	}
	cmd.Flags().StringVar(&contextName, "context", cluster.DefaultContext, "kubeconfig context name")
	return cmd
}

