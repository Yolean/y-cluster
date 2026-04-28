package yconverge

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"time"
)

// ConvergeModeLabel is the resource label kustomizations stamp on
// resources to opt into a non-default apply strategy. Mirrors the
// label introduced by the bash kubectl-yconverge plugin so existing
// `commonLabels: { yolean.se/converge-mode: serverside-force }`
// declarations keep working under the Go binary.
const ConvergeModeLabel = "yolean.se/converge-mode"

// runKubectlStreaming executes `kubectl <args...>` with stdin /
// stdout / stderr forwarded to the host process. Output is not
// captured -- the user sees verbatim what kubectl says, which is
// the line shape (`<kind>/<name> serverside-applied`,
// `... condition met`) developers already know from running
// kubectl directly.
func runKubectlStreaming(ctx context.Context, args ...string) error {
	cmd := exec.CommandContext(ctx, "kubectl", args...)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	cmd.Stdin = os.Stdin
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("kubectl %s: %w", argSummary(args), err)
	}
	return nil
}

// runKubectlTolerant runs `kubectl <args...>` and suppresses
// "expected idempotent" output. Two slots:
//
//   - stderrTolerate: if kubectl exits non-zero AND its stderr
//     matches one of these substrings, treat as success and
//     drop the stderr text. Used for "no objects passed to
//     <verb>" (empty selector match) and "AlreadyExists" (re-run
//     of a create-mode resource).
//   - stdoutSuppress: if kubectl's stdout matches one of these
//     substrings, drop the stdout text from the user's view.
//     Used for `kubectl delete`'s "No resources found" line --
//     it lands on stdout with exit 0, so without suppression a
//     vanilla yconverge run prints noise even when there's
//     nothing for the delete step to do.
//
// Both lists default to nil. stdout that doesn't match a
// suppress pattern is forwarded verbatim so the kubectl-style
// per-resource lines (`<kind>/<name> serverside-applied` etc.)
// reach the user. stderr that doesn't match a tolerate pattern
// is forwarded too, before the error propagates, so a real
// kubectl diagnostic isn't swallowed.
func runKubectlTolerant(ctx context.Context, stderrTolerate, stdoutSuppress []string, args ...string) error {
	cmd := exec.CommandContext(ctx, "kubectl", args...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	err := cmd.Run()

	if sout := stdout.String(); sout != "" {
		suppress := false
		for _, p := range stdoutSuppress {
			if strings.Contains(sout, p) {
				suppress = true
				break
			}
		}
		if !suppress {
			_, _ = os.Stdout.WriteString(sout)
		}
	}

	if err == nil {
		return nil
	}
	msg := stderr.String()
	for _, t := range stderrTolerate {
		if strings.Contains(msg, t) {
			return nil
		}
	}
	_, _ = os.Stderr.Write([]byte(msg))
	return fmt.Errorf("kubectl %s: %w", argSummary(args), err)
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

// kubectlApply renders the kustomize tree at opts.KustomizeDir and
// applies its resources via five label-routed kubectl invocations,
// matching the bash kubectl-yconverge plugin (90e8923) one-for-one:
//
//  1. yolean.se/converge-mode=create       -> kubectl create --save-config
//  2. yolean.se/converge-mode=replace      -> kubectl delete (followed by step 5's apply)
//  3. yolean.se/converge-mode=serverside-force -> kubectl apply --server-side --force-conflicts
//  4. yolean.se/converge-mode=serverside   -> kubectl apply --server-side
//  5. (no label, or label=replace)         -> kubectl apply
//
// Step 5's selector excludes the labels handled in 1, 3, 4 -- the
// replace-mode resources land here too so a delete (step 2) is
// followed by a fresh apply.
//
// Each step tolerates the empty-match stderr ("no objects passed
// to <verb>") so re-running yconverge against a kustomization
// that uses only one mode doesn't print four "error" lines.
// Step 1 also tolerates "AlreadyExists" so create-mode is the
// "skip if exists" semantic the bash plugin documents.
//
// Dry-run forwards to delete + create + apply so a replace-mode
// resource's dry-run plan is provably non-mutating.
func kubectlApply(ctx context.Context, opts Options) error {
	for _, step := range applySteps(opts) {
		if err := runKubectlTolerant(ctx, step.stderrTolerate, step.stdoutSuppress, step.args...); err != nil {
			return err
		}
	}
	return nil
}

// applyStep is the data shape behind one of the five label-routed
// kubectl invocations. Pulled out so unit tests can assert the
// argv each step produces without spawning kubectl.
type applyStep struct {
	args           []string
	stderrTolerate []string
	stdoutSuppress []string
}

// applySteps returns the five-step apply plan for the given
// Options. The returned slice's order matters and is preserved by
// the runtime: create -> delete (replace) -> serverside-force ->
// serverside -> plain-apply (the rest, including replace-mode
// resources reapplied after their delete).
func applySteps(opts Options) []applyStep {
	dryRun := ""
	if opts.DryRun == "server" {
		dryRun = "--dry-run=server"
	}
	ctxFlag := "--context=" + opts.Context
	dirFlag := []string{"-k", opts.KustomizeDir}
	sel := func(eq string) string { return "--selector=" + ConvergeModeLabel + "=" + eq }

	withDryRun := func(args ...string) []string {
		out := append([]string(nil), args...)
		if dryRun != "" {
			out = append(out, dryRun)
		}
		return append(out, dirFlag...)
	}

	return []applyStep{
		// 1. create: --save-config; skip-if-exists via AlreadyExists tolerance.
		{
			args:           withDryRun(ctxFlag, "create", "--save-config", sel("create")),
			stderrTolerate: []string{"AlreadyExists", "no objects passed to create"},
		},
		// 2. delete (replace-mode resources). kubectl prints "No
		// resources found" to STDOUT (not stderr) with exit 0 when
		// the selector matches nothing, so we suppress that line on
		// stdout rather than tolerating it on stderr. Real deletes
		// ("configmap "x" deleted") still pass through.
		// kubectl simulates under --dry-run=server so the plan
		// stays non-mutating end-to-end.
		{
			args:           withDryRun(ctxFlag, "delete", sel("replace")),
			stdoutSuppress: []string{"No resources found"},
		},
		// 3. apply --server-side --force-conflicts.
		{
			args: withDryRun(ctxFlag, "apply",
				"--server-side", "--force-conflicts", "--field-manager=y-cluster",
				sel("serverside-force")),
			stderrTolerate: []string{"no objects passed to apply"},
		},
		// 4. apply --server-side (no force).
		{
			args: withDryRun(ctxFlag, "apply",
				"--server-side", "--field-manager=y-cluster",
				sel("serverside")),
			stderrTolerate: []string{"no objects passed to apply"},
		},
		// 5. plain apply for everything else (including replace-mode
		// resources, now reapplied after the delete in step 2).
		{
			args: withDryRun(ctxFlag, "apply",
				"--selector="+ConvergeModeLabel+"!=create,"+
					ConvergeModeLabel+"!=serverside,"+
					ConvergeModeLabel+"!=serverside-force"),
			stderrTolerate: []string{"no objects passed to apply"},
		},
	}
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
	return runKubectlStreaming(ctx, args...)
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
	return runKubectlStreaming(ctx, args...)
}
