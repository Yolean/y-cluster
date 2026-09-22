package main

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"path"
	"regexp"
	"strings"

	"github.com/spf13/cobra"

	"github.com/Yolean/y-cluster/pkg/cluster"
	"github.com/Yolean/y-cluster/pkg/shquote"
)

// manifestsCmd is the umbrella for build-time manifest staging on the
// cluster's appliance disk. Three verbs (strict in both directions):
//
//   - add     : name must NOT be staged (or, if staged with
//     byte-identical content, succeeds silently as a
//     re-run idempotency convenience)
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

// nodeShell runs a shell command as root on the cluster node. The
// manifests verbs go through it rather than cluster.RunShell directly
// so their add/replace/rm rules can be tested against a real shell
// and file system without a cluster.
type nodeShell func(ctx context.Context, cmd string, stdin io.Reader, stdout, stderr io.Writer) error

func nodeShellFor(lr *cluster.LookupResult) nodeShell {
	return func(ctx context.Context, cmd string, stdin io.Reader, stdout, stderr io.Writer) error {
		return cluster.RunShell(ctx, lr, cmd, stdin, stdout, stderr)
	}
}

// Markers the read command prints before the content, so that
// "absent" is something the node said rather than something inferred
// from a failed command.
const (
	stagedPresent = "y-cluster-manifest:present"
	stagedAbsent  = "y-cluster-manifest:absent"
)

// readStagedManifest reads the bytes of a staged manifest off the
// cluster node.
//
// Returns (content, true, nil) when the file is present;
// (nil, false, nil) when the node reports it absent; (nil, false,
// err) on any failure to ask (ssh, docker exec, I/O). A transport
// failure must never read as "absent": add would then overwrite a
// manifest it could not see, and replace/rm would blame the operator
// for a name that is staged.
func readStagedManifest(ctx context.Context, sh nodeShell, target string) ([]byte, bool, error) {
	q := shquote.Quote(target)
	cmd := "if [ -e " + q + " ]; then echo " + stagedPresent + "; cat " + q + "; else echo " + stagedAbsent + "; fi"
	var stdout, stderr bytes.Buffer
	if err := sh(ctx, cmd, nil, &stdout, &stderr); err != nil {
		return nil, false, fmt.Errorf("read %s: %s: %w", target, strings.TrimSpace(stderr.String()), err)
	}
	marker, content, _ := bytes.Cut(stdout.Bytes(), []byte("\n"))
	switch string(marker) {
	case stagedPresent:
		return content, true, nil
	case stagedAbsent:
		return nil, false, nil
	default:
		return nil, false, fmt.Errorf("read %s: unexpected answer from the node: %q", target, marker)
	}
}

// writeStagedManifest puts the file in place atomically with mode
// 0644 and creates the staging directory under mode 0755 if missing.
//
// Written to a sibling temp file and renamed, with nothing fancier
// than cat, chmod and mv. `install -m 0644 /dev/stdin <target>` reads
// nicer but depends on how install treats a pipe as its source:
// uutils coreutils 0.2 (Ubuntu 25.10) fails it whenever the target
// already exists, which is every `replace`. The temp name ends in
// .tmp, which k3s does not apply should one be left behind and moved
// to the manifests directory by prepare-export.
func writeStagedManifest(ctx context.Context, sh nodeShell, target string, data []byte) error {
	dir, tmp, dst := shquote.Quote(path.Dir(target)), shquote.Quote(target+".tmp"), shquote.Quote(target)
	writeCmd := "install -d -m 0755 " + dir + " && " +
		"{ cat > " + tmp + " && chmod 0644 " + tmp + " && mv -f " + tmp + " " + dst + "; } || " +
		"{ rc=$?; rm -f " + tmp + "; exit $rc; }"
	var stderr bytes.Buffer
	if err := sh(ctx, writeCmd, bytes.NewReader(data), nil, &stderr); err != nil {
		return fmt.Errorf("write manifest: %s: %w", stderr.String(), err)
	}
	return nil
}

// removeStagedManifest deletes the file from the staging directory.
// Caller should have already checked existence so a missing file
// here surfaces as a real error (permission, fs problem).
func removeStagedManifest(ctx context.Context, sh nodeShell, target string) error {
	var stderr bytes.Buffer
	if err := sh(ctx, "rm "+shquote.Quote(target), nil, nil, &stderr); err != nil {
		return fmt.Errorf("rm %s: %s: %w", target, stderr.String(), err)
	}
	return nil
}

// stageAdd, stageReplace and stageRemove are the rules of the three
// verbs, apart from cobra and the cluster lookup.

func stageAdd(ctx context.Context, sh nodeShell, out io.Writer, name, target string, data []byte) error {
	existing, present, err := readStagedManifest(ctx, sh, target)
	if err != nil {
		return err
	}
	if present {
		if bytes.Equal(existing, data) {
			fmt.Fprintf(out, "manifest %q already staged at %s with identical content; no change\n", name, target)
			return nil
		}
		return fmt.Errorf("manifest %q already staged at %s with different content; use `y-cluster manifests replace` to overwrite", name, target)
	}
	if err := writeStagedManifest(ctx, sh, target, data); err != nil {
		return err
	}
	fmt.Fprintf(out, "staged manifest %q -> %s (%d bytes)\n", name, target, len(data))
	return nil
}

func stageReplace(ctx context.Context, sh nodeShell, out io.Writer, name, target string, data []byte) error {
	existing, present, err := readStagedManifest(ctx, sh, target)
	if err != nil {
		return err
	}
	if !present {
		return fmt.Errorf("manifest %q is not staged at %s; use `y-cluster manifests add` to create", name, target)
	}
	if bytes.Equal(existing, data) {
		fmt.Fprintf(out, "manifest %q at %s already matches input; no change\n", name, target)
		return nil
	}
	if err := writeStagedManifest(ctx, sh, target, data); err != nil {
		return err
	}
	fmt.Fprintf(out, "replaced manifest %q -> %s (%d bytes)\n", name, target, len(data))
	return nil
}

func stageRemove(ctx context.Context, sh nodeShell, out io.Writer, name, target string) error {
	if _, present, err := readStagedManifest(ctx, sh, target); err != nil {
		return err
	} else if !present {
		return fmt.Errorf("manifest %q is not staged at %s; nothing to remove", name, target)
	}
	if err := removeStagedManifest(ctx, sh, target); err != nil {
		return err
	}
	fmt.Fprintf(out, "removed manifest %q from %s\n", name, target)
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
	r, closer, err := openInput(c.Context(), input, c.InOrStdin())
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
			return stageAdd(c.Context(), nodeShellFor(lr), c.OutOrStdout(), name, target, data)
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
			return stageReplace(c.Context(), nodeShellFor(lr), c.OutOrStdout(), name, target, data)
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
named manifest doesn't exist -- there's no ` + "`--force`" + ` /
"don't care" mode. Use this when iterating on the staged
manifest's content alongside ` + "`add`" + `, or to drop a manifest
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
			return stageRemove(c.Context(), nodeShellFor(lr), c.OutOrStdout(), name, stagedManifestPath(name))
		},
	}
	cmd.Flags().StringVar(&contextName, "context", cluster.DefaultContext, "kubeconfig context name")
	return cmd
}
