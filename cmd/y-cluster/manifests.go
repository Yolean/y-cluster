package main

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"regexp"
	"strings"

	"github.com/spf13/cobra"

	"github.com/Yolean/y-cluster/pkg/cluster"
)

// manifestsCmd is the umbrella for build-time manifest staging on the
// cluster's appliance disk. Three verbs (strict in both directions):
//
//   - add     : name must NOT be staged (or, if staged with
//               byte-identical content, succeeds silently as a
//               re-run idempotency convenience)
//   - replace : name MUST be staged, overwrites
//   - rm      : name MUST be staged, removes
//
// We deliberately don't ship a `--force` flag: the operator (or
// agent) has to know what state they're in. The verb itself
// documents the operator's intent at the call site.
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
	cmd.AddCommand(manifestsReplaceCmd())
	cmd.AddCommand(manifestsRmCmd())
	return cmd
}

// manifestNameRE constrains <name> to a portable filename: leading
// alphanumeric (no leading dot or dash), then alphanumerics + dot +
// dash + underscore. Reject path separators, ".." traversal, empty,
// and shell-metacharacters in one regex. The same regex would accept
// a kubectl resource name, which is convenient since the manifest's
// filename and metadata.name typically match.
var manifestNameRE = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9._-]*$`)

// stagedManifestPath returns the on-cluster path for a manifest by
// name. Single source of truth -- changing the staging directory or
// the file-suffix convention only touches this function.
func stagedManifestPath(name string) string {
	return "/var/lib/y-cluster/manifests-staging/" + name + ".yaml"
}

// readStagedManifest reads the bytes of a staged manifest off the
// cluster node.
//
// Returns (content, true, nil) when the file is present;
// (nil, false, nil) when it doesn't exist; (nil, false, err) on any
// other failure (I/O, ssh, ctr-exec, etc.). The two-shell-call
// shape costs a round-trip but keeps the "missing" case
// distinguishable from "present but empty," which the idempotency
// + replace/rm flows care about.
func readStagedManifest(ctx context.Context, lr *cluster.LookupResult, target string) ([]byte, bool, error) {
	if err := cluster.RunShell(ctx, lr, "test -e "+target, nil, nil, nil); err != nil {
		return nil, false, nil
	}
	var stdout, stderr bytes.Buffer
	if err := cluster.RunShell(ctx, lr, "cat "+target, nil, &stdout, &stderr); err != nil {
		return nil, false, fmt.Errorf("read %s: %s: %w", target, stderr.String(), err)
	}
	return stdout.Bytes(), true, nil
}

// writeStagedManifest installs the file atomically with mode 0644
// and creates the staging directory under mode 0755 if missing.
// install(1) (not `cat > file`) is used so the file lands with a
// known mode and a deterministic atomicity boundary.
func writeStagedManifest(ctx context.Context, lr *cluster.LookupResult, target string, data []byte) error {
	writeCmd := "install -d -m 0755 /var/lib/y-cluster/manifests-staging && " +
		"install -m 0644 /dev/stdin " + target
	var stderr bytes.Buffer
	if err := cluster.RunShell(ctx, lr, writeCmd, bytes.NewReader(data), nil, &stderr); err != nil {
		return fmt.Errorf("write manifest: %s: %w", stderr.String(), err)
	}
	return nil
}

// removeStagedManifest deletes the file from the staging directory.
// Caller should have already checked existence so a missing file
// here surfaces as a real error (permission, fs problem).
func removeStagedManifest(ctx context.Context, lr *cluster.LookupResult, target string) error {
	var stderr bytes.Buffer
	if err := cluster.RunShell(ctx, lr, "rm "+target, nil, nil, &stderr); err != nil {
		return fmt.Errorf("rm %s: %s: %w", target, stderr.String(), err)
	}
	return nil
}

// readManifestInput is the shared "validate name, read input bytes,
// look up cluster, compute target" prelude for add/replace. Splits
// the per-command logic from the boilerplate that's identical
// across both, so changes to either side don't drift.
func readManifestInput(c *cobra.Command, name, input string, contextName string) ([]byte, *cluster.LookupResult, string, error) {
	if !manifestNameRE.MatchString(name) {
		return nil, nil, "", fmt.Errorf("invalid manifest name %q: must match %s (no slashes, no .., must start with alphanumeric)", name, manifestNameRE)
	}
	r, closer, err := openYAMLInput(input, c.InOrStdin())
	if err != nil {
		return nil, nil, "", err
	}
	defer closer()
	data, err := io.ReadAll(r)
	if err != nil {
		return nil, nil, "", fmt.Errorf("read manifest: %w", err)
	}
	if len(strings.TrimSpace(string(data))) == 0 {
		return nil, nil, "", fmt.Errorf("manifest is empty")
	}
	lr, err := cluster.Lookup(c.Context(), "", contextName)
	if err != nil {
		return nil, nil, "", err
	}
	return data, lr, stagedManifestPath(name), nil
}

func manifestsAddCmd() *cobra.Command {
	var contextName string

	cmd := &cobra.Command{
		Use:   "add <name> <path|->",
		Short: "Stage a new manifest for first-customer-boot apply",
		Long: `Reads the YAML at <path> (or stdin when <path> is "-"), then
writes it to ` + "`/var/lib/y-cluster/manifests-staging/<name>.yaml`" + `
on the cluster node.

Strict: the name must NOT already be staged. To overwrite an
existing manifest use ` + "`y-cluster manifests replace`" + `; to
delete one use ` + "`y-cluster manifests rm`" + `. As a re-run
convenience, ` + "`add`" + ` succeeds silently when the same name
is already staged WITH BYTE-IDENTICAL content (covers the
"my script ran twice, file unchanged" case without weakening the
strict-new contract).

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
			name, input := args[0], args[1]
			data, lr, target, err := readManifestInput(c, name, input, contextName)
			if err != nil {
				return err
			}
			existing, present, err := readStagedManifest(c.Context(), lr, target)
			if err != nil {
				return err
			}
			if present {
				if bytes.Equal(existing, data) {
					fmt.Fprintf(c.OutOrStdout(), "manifest %q already staged at %s with identical content; no change\n", name, target)
					return nil
				}
				return fmt.Errorf("manifest %q already staged at %s with different content; use `y-cluster manifests replace` to overwrite", name, target)
			}
			if err := writeStagedManifest(c.Context(), lr, target, data); err != nil {
				return err
			}
			fmt.Fprintf(c.OutOrStdout(), "staged manifest %q -> %s (%d bytes)\n", name, target, len(data))
			return nil
		},
	}
	cmd.Flags().StringVar(&contextName, "context", cluster.DefaultContext, "kubeconfig context name")
	return cmd
}

func manifestsReplaceCmd() *cobra.Command {
	var contextName string

	cmd := &cobra.Command{
		Use:   "replace <name> <path|->",
		Short: "Overwrite an already-staged manifest with new content",
		Long: `Reads the YAML at <path> (or stdin when <path> is "-"), then
overwrites the existing manifest at
` + "`/var/lib/y-cluster/manifests-staging/<name>.yaml`" + ` on the
cluster node.

Strict: the name MUST already be staged. Bails loud when the
named manifest doesn't exist (use ` + "`y-cluster manifests add`" + `
for the create path). The verb documents intent at the call site
("I know this name is in use and I'm intentionally overwriting").

No-op when the new content is byte-identical to the existing
file: ` + "`replace`" + ` doesn't bump anything visible to k3s in
that case either, but we still print a "no change" message so
scripts can read the outcome.`,
		Args: cobra.ExactArgs(2),
		RunE: func(c *cobra.Command, args []string) error {
			name, input := args[0], args[1]
			data, lr, target, err := readManifestInput(c, name, input, contextName)
			if err != nil {
				return err
			}
			existing, present, err := readStagedManifest(c.Context(), lr, target)
			if err != nil {
				return err
			}
			if !present {
				return fmt.Errorf("manifest %q is not staged at %s; use `y-cluster manifests add` to create", name, target)
			}
			if bytes.Equal(existing, data) {
				fmt.Fprintf(c.OutOrStdout(), "manifest %q at %s already matches input; no change\n", name, target)
				return nil
			}
			if err := writeStagedManifest(c.Context(), lr, target, data); err != nil {
				return err
			}
			fmt.Fprintf(c.OutOrStdout(), "replaced manifest %q -> %s (%d bytes)\n", name, target, len(data))
			return nil
		},
	}
	cmd.Flags().StringVar(&contextName, "context", cluster.DefaultContext, "kubeconfig context name")
	return cmd
}

func manifestsRmCmd() *cobra.Command {
	var contextName string

	cmd := &cobra.Command{
		Use:   "rm <name>",
		Short: "Remove a staged manifest from the cluster's appliance",
		Long: `Removes the file at
` + "`/var/lib/y-cluster/manifests-staging/<name>.yaml`" + ` on the
cluster node.

Strict: the name MUST already be staged. Bails loud when the
named manifest doesn't exist -- there's no `+"`--force`"+` /
"don't care" mode. Use this when iterating on the staged
manifest's content alongside `+"`add`"+`, or to drop a manifest
that's no longer wanted before prepare-export captures it.`,
		Args: cobra.ExactArgs(1),
		RunE: func(c *cobra.Command, args []string) error {
			name := args[0]
			if !manifestNameRE.MatchString(name) {
				return fmt.Errorf("invalid manifest name %q: must match %s (no slashes, no .., must start with alphanumeric)", name, manifestNameRE)
			}
			lr, err := cluster.Lookup(c.Context(), "", contextName)
			if err != nil {
				return err
			}
			target := stagedManifestPath(name)
			if _, present, err := readStagedManifest(c.Context(), lr, target); err != nil {
				return err
			} else if !present {
				return fmt.Errorf("manifest %q is not staged at %s; nothing to remove", name, target)
			}
			if err := removeStagedManifest(c.Context(), lr, target); err != nil {
				return err
			}
			fmt.Fprintf(c.OutOrStdout(), "removed manifest %q from %s\n", name, target)
			return nil
		},
	}
	cmd.Flags().StringVar(&contextName, "context", cluster.DefaultContext, "kubeconfig context name")
	return cmd
}
