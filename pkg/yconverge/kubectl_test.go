package yconverge

import (
	"reflect"
	"strings"
	"testing"
)

// TestApplySteps_OrderAndCount: the bash plugin runs five
// label-routed kubectl invocations in a fixed order. Pin the
// order here so a refactor that reshuffles them (or drops the
// fall-through plain-apply step) fails loudly.
func TestApplySteps_OrderAndCount(t *testing.T) {
	steps := applySteps(Options{Context: "local", KustomizeDir: "/tmp/k"})
	if len(steps) != 5 {
		t.Fatalf("want 5 steps, got %d", len(steps))
	}

	// Step ordering, asserted on the first verb-ish token after
	// --context=... so the test stays stable if a flag's order
	// inside one step shifts.
	wantVerbs := []string{"create", "delete", "apply", "apply", "apply"}
	for i, want := range wantVerbs {
		if got := steps[i].args[1]; got != want {
			t.Errorf("step %d: verb %q, want %q (full args: %v)", i, got, want, steps[i].args)
		}
	}
}

// TestApplySteps_PerStepArgs locks the exact argv each step
// produces for a vanilla (non-dry-run) Options. This is the
// regression guard for the converge-mode contract: any drift
// in `--save-config`, `--server-side`, `--force-conflicts`,
// `--field-manager=y-cluster`, or the selector formula
// surfaces as a test failure rather than as silent semantic
// change against the cluster.
func TestApplySteps_PerStepArgs(t *testing.T) {
	steps := applySteps(Options{Context: "local", KustomizeDir: "/tmp/k"})
	want := [][]string{
		{
			"--context=local", "create", "--save-config",
			"--selector=yolean.se/converge-mode=create",
			"-k", "/tmp/k",
		},
		{
			"--context=local", "delete",
			"--selector=yolean.se/converge-mode=replace",
			"-k", "/tmp/k",
		},
		{
			"--context=local", "apply",
			"--server-side", "--force-conflicts", "--field-manager=y-cluster",
			"--selector=yolean.se/converge-mode=serverside-force",
			"-k", "/tmp/k",
		},
		{
			"--context=local", "apply",
			"--server-side", "--field-manager=y-cluster",
			"--selector=yolean.se/converge-mode=serverside",
			"-k", "/tmp/k",
		},
		{
			"--context=local", "apply",
			"--selector=yolean.se/converge-mode!=create," +
				"yolean.se/converge-mode!=serverside," +
				"yolean.se/converge-mode!=serverside-force",
			"-k", "/tmp/k",
		},
	}
	for i, w := range want {
		if !reflect.DeepEqual(steps[i].args, w) {
			t.Errorf("step %d args mismatch\n got:  %v\n want: %v", i, steps[i].args, w)
		}
	}
}

// TestApplySteps_DryRunForwards: --dry-run=server must reach
// every step (delete + create + apply variants), so a dry-run of
// a kustomization with replace-mode resources is provably
// non-mutating end-to-end -- the bash plugin's behaviour.
func TestApplySteps_DryRunForwards(t *testing.T) {
	steps := applySteps(Options{Context: "local", KustomizeDir: "/tmp/k", DryRun: "server"})
	for i, s := range steps {
		found := false
		for _, a := range s.args {
			if a == "--dry-run=server" {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("step %d (verb=%s) missing --dry-run=server: %v", i, s.args[1], s.args)
		}
	}
}

// TestApplySteps_NoDryRunByDefault confirms the inverse: an
// empty Options.DryRun must not insert --dry-run=anything.
// Catches a typo where a default value (e.g. "none") gets
// forwarded as a flag.
func TestApplySteps_NoDryRunByDefault(t *testing.T) {
	steps := applySteps(Options{Context: "local", KustomizeDir: "/tmp/k"})
	for i, s := range steps {
		for _, a := range s.args {
			if strings.HasPrefix(a, "--dry-run=") {
				t.Errorf("step %d unexpectedly carries %q: %v", i, a, s.args)
			}
		}
	}
}

// TestApplySteps_TolerateContract pins the per-step "expected
// empty / idempotent" stderr substrings. The bash plugin
// distinguishes which kubectl errors are the documented
// converge-mode happy path (empty selector match, AlreadyExists
// on a re-create) from a real failure -- accidental loosening
// of these would silently swallow a genuine bug.
func TestApplySteps_TolerateContract(t *testing.T) {
	steps := applySteps(Options{Context: "local", KustomizeDir: "/tmp/k"})
	want := [][]string{
		{"AlreadyExists", "no objects passed to create"}, // create
		{"No resources found"},                           // delete
		{"no objects passed to apply"},                   // serverside-force
		{"no objects passed to apply"},                   // serverside
		{"no objects passed to apply"},                   // plain apply
	}
	for i, w := range want {
		if !reflect.DeepEqual(steps[i].tolerate, w) {
			t.Errorf("step %d tolerate mismatch\n got:  %v\n want: %v", i, steps[i].tolerate, w)
		}
	}
}
