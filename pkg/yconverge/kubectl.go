package yconverge

import (
	"bytes"
	"context"
	"fmt"
	"io"
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
// stdout / stderr forwarded. Output is not captured -- the user
// sees verbatim what kubectl says (`<kind>/<name> condition met`
// from `kubectl wait`, etc.). Used for the wait / rollout-status
// paths where kubectl's own progress output is the answer the
// user wants.
//
// progress is the writer kubectl's stdout goes to (typically
// opts.progressOut() to keep the four "yconverge ..." headers
// and the kubectl lines on the same sink). nil falls back to
// os.Stdout so callers that don't care can pass nil.
func runKubectlStreaming(ctx context.Context, progress io.Writer, args ...string) error {
	if progress == nil {
		progress = os.Stdout
	}
	cmd := exec.CommandContext(ctx, "kubectl", args...)
	cmd.Stdout = progress
	cmd.Stderr = os.Stderr
	cmd.Stdin = os.Stdin
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("kubectl %s: %w", argSummary(args), err)
	}
	return nil
}

// runApplyInvocation runs one of a group's kubectl invocations,
// captures its stdout into out (so the caller can decide whether
// to forward it after seeing all invocations in the group), and
// surfaces stderr. The two tolerance slots match the bash
// plugin's pattern:
//
//   - stderrTolerate: if kubectl exits non-zero AND its stderr
//     contains one of these substrings, treat as success and
//     drop the stderr text. Used for "no objects passed to
//     <verb>" (empty selector match) and "AlreadyExists" (re-run
//     of a create-mode resource).
//   - stdoutSuppress: if kubectl's stdout contains one of these
//     substrings, drop the stdout entirely. Used for
//     `kubectl delete`'s "No resources found" line -- lands on
//     stdout with exit 0 so a vanilla yconverge run would surface
//     it as noise without suppression.
//
// stderr that doesn't match a tolerate pattern is forwarded to
// the user's stderr before the error propagates, so a real
// kubectl diagnostic isn't swallowed.
func runApplyInvocation(ctx context.Context, step applyStep, out io.Writer) error {
	cmd := exec.CommandContext(ctx, "kubectl", step.args...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	err := cmd.Run()

	if sout := stdout.String(); sout != "" {
		suppress := false
		for _, p := range step.stdoutSuppress {
			if strings.Contains(sout, p) {
				suppress = true
				break
			}
		}
		if !suppress {
			_, _ = out.Write([]byte(sout))
		}
	}

	if err == nil {
		return nil
	}
	msg := stderr.String()
	for _, t := range step.stderrTolerate {
		if strings.Contains(msg, t) {
			return nil
		}
	}
	_, _ = os.Stderr.Write([]byte(msg))
	return fmt.Errorf("kubectl %s: %w", argSummary(step.args), err)
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
// applies its resources via label-routed kubectl invocations,
// matching the bash kubectl-yconverge plugin (90e8923):
//
//  1. yolean.se/converge-mode=create       -> kubectl create --save-config
//  2. yolean.se/converge-mode=replace      -> kubectl delete + kubectl apply (re-create)
//  3. yolean.se/converge-mode=serverside-force -> kubectl apply --server-side --force-conflicts
//  4. yolean.se/converge-mode=serverside   -> kubectl apply --server-side
//  5. (unlabelled)                         -> kubectl apply
//
// Replace-mode is two kubectl invocations -- a delete followed by
// a re-creating apply -- but conceptually one operation, so its
// progress line ("yconverge converge-mode=replace") covers both.
// applyGroups encodes that structure: each group is one mode and
// holds the one or two kubectl invocations behind it; the runner
// emits the group's header at most once and only when at least
// one invocation produces non-suppressed output.
//
// Each step's tolerance lists keep idempotent paths quiet:
// empty-selector match ("no objects passed to <verb>") on apply,
// AlreadyExists on create re-runs, and "No resources found" on
// the delete step's stdout when nothing is replace-labelled.
//
// Dry-run forwards to every invocation including delete, so a
// replace-mode resource's dry-run plan is provably non-mutating
// end-to-end.
func kubectlApply(ctx context.Context, opts Options) error {
	out := opts.progressOut()
	for _, g := range applyGroups(opts) {
		var groupBuf bytes.Buffer
		for _, step := range g.invocations {
			if err := runApplyInvocation(ctx, step, &groupBuf); err != nil {
				// On failure, surface anything the group produced
				// so far so the diagnostic isn't preceded by an
				// orphaned header. Then propagate the error.
				if groupBuf.Len() > 0 {
					if g.mode != "" {
						fmt.Fprintf(out, "yconverge converge-mode=%s\n", g.mode)
					}
					_, _ = out.Write(groupBuf.Bytes())
				}
				return err
			}
		}
		if groupBuf.Len() > 0 {
			if g.mode != "" {
				fmt.Fprintf(out, "yconverge converge-mode=%s\n", g.mode)
			}
			_, _ = out.Write(groupBuf.Bytes())
		}
	}
	return nil
}

// applyStep is one kubectl invocation. Pulled out so tests can
// assert the argv without spawning kubectl.
type applyStep struct {
	args           []string
	stderrTolerate []string
	stdoutSuppress []string
}

// applyGroup gathers one or more applySteps under a single mode
// header. The runner emits the header once before the first
// non-empty invocation's output. mode == "" means no header
// (the unlabelled / default bucket -- kubectl's per-resource
// lines speak for themselves).
type applyGroup struct {
	mode        string
	invocations []applyStep
}

// applyGroups returns the label-routed apply plan for opts.
// Group order matters and is preserved by the runtime: create
// -> replace -> serverside-force -> serverside -> default.
//
// Replace-mode is the only group with two invocations: the delete
// followed by an apply that re-creates the same selector's
// resources. Both go under one "yconverge converge-mode=replace"
// header to match the user-facing semantics ("replace" is one
// operation conceptually).
//
// The default group's selector excludes every other mode so the
// replace-mode resources don't get re-applied a second time
// here.
func applyGroups(opts Options) []applyGroup {
	dryRun := ""
	if opts.DryRun == "server" {
		dryRun = "--dry-run=server"
	}
	ctxFlag := "--context=" + opts.Context
	dirFlag := []string{"-k", opts.KustomizeDir}

	// withUser ANDs the optional user selector (opts.Selector) onto
	// an internal selector body. Empty user selector means
	// "no extra filter" and the body is returned untouched. Comma is
	// kubectl's selector AND separator, so a body like
	// "yolean.se/converge-mode=create" combined with "app=foo"
	// becomes "yolean.se/converge-mode=create,app=foo" and only
	// matches resources that satisfy both halves.
	withUser := func(body string) string {
		if opts.Selector == "" {
			return body
		}
		return body + "," + opts.Selector
	}
	sel := func(eq string) string {
		return "--selector=" + withUser(ConvergeModeLabel+"="+eq)
	}

	withDryRun := func(args ...string) []string {
		out := append([]string(nil), args...)
		if dryRun != "" {
			out = append(out, dryRun)
		}
		return append(out, dirFlag...)
	}

	return []applyGroup{
		{
			mode: "create",
			invocations: []applyStep{
				{
					args:           withDryRun(ctxFlag, "create", "--save-config", sel("create")),
					stderrTolerate: []string{"AlreadyExists", "no objects passed to create"},
				},
			},
		},
		{
			// Delete + re-apply for replace-mode resources, both
			// under one "converge-mode=replace" header. The delete
			// step prints "No resources found" to STDOUT (not stderr)
			// with exit 0 when nothing matches, so we suppress that
			// stdout line; real deletes ("<kind> 'x' deleted") still
			// pass through.
			mode: "replace",
			invocations: []applyStep{
				{
					args:           withDryRun(ctxFlag, "delete", sel("replace")),
					stdoutSuppress: []string{"No resources found"},
				},
				{
					args:           withDryRun(ctxFlag, "apply", sel("replace")),
					stderrTolerate: []string{"no objects passed to apply"},
				},
			},
		},
		{
			mode: "serverside-force",
			invocations: []applyStep{
				{
					args: withDryRun(ctxFlag, "apply",
						"--server-side", "--force-conflicts", "--field-manager=y-cluster",
						sel("serverside-force")),
					stderrTolerate: []string{"no objects passed to apply"},
				},
			},
		},
		{
			mode: "serverside",
			invocations: []applyStep{
				{
					args: withDryRun(ctxFlag, "apply",
						"--server-side", "--field-manager=y-cluster",
						sel("serverside")),
					stderrTolerate: []string{"no objects passed to apply"},
				},
			},
		},
		{
			// Default bucket: unlabelled resources only. Replace-mode
			// resources are NOT re-applied here -- they were already
			// re-created in the replace group's second invocation
			// above, so the negative selector excludes replace too.
			// No header: kubectl's `<kind>/<name> created` lines are
			// enough signal.
			mode: "",
			invocations: []applyStep{
				{
					args: withDryRun(ctxFlag, "apply",
						"--selector="+withUser(
							ConvergeModeLabel+"!=create,"+
								ConvergeModeLabel+"!=replace,"+
								ConvergeModeLabel+"!=serverside,"+
								ConvergeModeLabel+"!=serverside-force")),
					stderrTolerate: []string{"no objects passed to apply"},
				},
			},
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
func kubectlWait(ctx context.Context, progress io.Writer, contextName, resource, namespace, forSpec string, timeout time.Duration) error {
	args := []string{
		"--context=" + contextName,
		"wait", resource,
		"--for=" + forSpec,
		"--timeout=" + timeout.String(),
	}
	if namespace != "" {
		args = append(args, "-n", namespace)
	}
	return runKubectlStreaming(ctx, progress, args...)
}

// kubectlRolloutStatus runs `kubectl --context=<...> rollout
// status <resource> -n <ns> --timeout=<timeout>`. resource is
// already in the `<kind>/<name>` form the bash plugin used.
func kubectlRolloutStatus(ctx context.Context, progress io.Writer, contextName, resource, namespace string, timeout time.Duration) error {
	args := []string{
		"--context=" + contextName,
		"rollout", "status", resource,
		"--timeout=" + timeout.String(),
	}
	if namespace != "" {
		args = append(args, "-n", namespace)
	}
	return runKubectlStreaming(ctx, progress, args...)
}
