package yconverge

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"time"
)

// runKubectl executes `kubectl <args...>` with stdout / stderr
// forwarded to the host process so the user sees verbatim what
// kubectl says -- the line shapes (`<kind>/<name> serverside-applied`,
// `deployment.apps/foo condition met`, etc.) developers already
// know from running kubectl directly.
//
// Output is intentionally NOT captured: the callers don't post-
// process it (in contrast to e.g. cluster.RunCtr where we forward
// a stream into the user's pipe). Forward-without-capture keeps
// the wrapper invisible -- a `kubectl yconverge` invocation looks
// the same as the underlying kubectl invocation modulo the
// `kubectl` prefix.
//
// Errors carry the exit code and the args so a failure surfaces
// "kubectl apply -k /tmp/xyz: exit status 1" -- enough for a
// scripted caller to act, while the actual diagnostic text is
// already on the user's terminal from kubectl's stderr.
func runKubectl(ctx context.Context, args ...string) error {
	cmd := exec.CommandContext(ctx, "kubectl", args...)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	cmd.Stdin = os.Stdin
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("kubectl %s: %w", argSummary(args), err)
	}
	return nil
}

// argSummary returns a short description of an argv for error
// messages. The full argv can be long (kustomize paths, jsonpath
// specs); we keep the first three meaningful entries so the
// reader sees `kubectl apply --server-side ...` rather than the
// whole string.
func argSummary(args []string) string {
	keep := args
	if len(keep) > 3 {
		keep = keep[:3]
	}
	out := ""
	for i, a := range keep {
		if i > 0 {
			out += " "
		}
		out += a
	}
	if len(args) > len(keep) {
		out += " ..."
	}
	return out
}

// kubectlApply runs `kubectl --context=<...> apply --server-side
// --force-conflicts --field-manager=y-cluster -k <dir>` against
// the configured context. The flag set matches what
// pkg/k8sapply.Apply does internally so the wire-level effect is
// identical -- only the developer-visible output differs.
//
// --dry-run=server forwards through; the bash plugin's
// "client mode is rejected" rule is preserved because kubectl
// itself rejects --dry-run=client with --server-side.
func kubectlApply(ctx context.Context, opts Options) error {
	args := []string{
		"--context=" + opts.Context,
		"apply",
		"--server-side",
		"--force-conflicts",
		"--field-manager=y-cluster",
		"-k", opts.KustomizeDir,
	}
	if opts.DryRun == "server" {
		args = append(args, "--dry-run=server")
	}
	return runKubectl(ctx, args...)
}

// kubectlWait runs `kubectl --context=<...> wait <resource> -n <ns>
// --for=<forSpec> --timeout=<timeout>`. forSpec is the same
// `condition=...` / `jsonpath=...` / `delete` form kubectl wait
// accepts -- yconverge.cue's Check.For is already in that shape.
//
// Empty namespace omits `-n`, matching kubectl wait's "use the
// context's default namespace" behaviour. Cluster-scoped kinds
// pass empty here.
func kubectlWait(ctx context.Context, contextName, resource, namespace, forSpec string, timeout time.Duration) error {
	args := []string{
		"--context=" + contextName,
		"wait", resource,
		"--for=" + forSpec,
		"--timeout=" + timeout.String(),
	}
	if namespace != "" {
		args = append(args, "-n", namespace)
	}
	return runKubectl(ctx, args...)
}

// kubectlRolloutStatus runs `kubectl --context=<...> rollout
// status <resource> -n <ns> --timeout=<timeout>`. resource is
// already in the `<kind>/<name>` form the bash plugin used.
func kubectlRolloutStatus(ctx context.Context, contextName, resource, namespace string, timeout time.Duration) error {
	args := []string{
		"--context=" + contextName,
		"rollout", "status", resource,
		"--timeout=" + timeout.String(),
	}
	if namespace != "" {
		args = append(args, "-n", namespace)
	}
	return runKubectl(ctx, args...)
}
