package yconverge

import (
	"reflect"
	"strings"
	"testing"
)

// TestApplyGroups_OrderAndCount: the apply plan is five groups in
// a fixed order. Pin both the count and the mode-header sequence
// so a refactor that reshuffles them (or drops the unlabelled
// fall-through group) fails loudly.
func TestApplyGroups_OrderAndCount(t *testing.T) {
	groups := applyGroups(Options{Context: "local", KustomizeDir: "/tmp/k"})
	if len(groups) != 5 {
		t.Fatalf("want 5 groups, got %d", len(groups))
	}

	wantModes := []string{"create", "replace", "serverside-force", "serverside", ""}
	for i, want := range wantModes {
		if got := groups[i].mode; got != want {
			t.Errorf("group %d: mode %q, want %q", i, got, want)
		}
	}
}

// TestApplyGroups_ReplaceHasTwoInvocations is the structural
// invariant for the replace mode: one delete followed by one
// re-creating apply, both under one progress header. If a future
// refactor splits or merges them, the user-facing line ("yconverge
// converge-mode=replace" covering both delete + recreate) breaks.
func TestApplyGroups_ReplaceHasTwoInvocations(t *testing.T) {
	groups := applyGroups(Options{Context: "local", KustomizeDir: "/tmp/k"})

	for i, g := range groups {
		want := 1
		if g.mode == "replace" {
			want = 2
		}
		if len(g.invocations) != want {
			t.Errorf("group %d (mode=%q): %d invocations, want %d",
				i, g.mode, len(g.invocations), want)
		}
	}

	// Replace's two invocations are delete then apply with the
	// same selector -- catches an accidental selector drift
	// between them.
	replace := groups[1]
	if got := replace.invocations[0].args[1]; got != "delete" {
		t.Errorf("replace invocation 0: verb %q, want delete", got)
	}
	if got := replace.invocations[1].args[1]; got != "apply" {
		t.Errorf("replace invocation 1: verb %q, want apply", got)
	}
	sel := "--selector=" + ConvergeModeLabel + "=replace"
	for j, inv := range replace.invocations {
		found := false
		for _, a := range inv.args {
			if a == sel {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("replace invocation %d missing %q: %v", j, sel, inv.args)
		}
	}
}

// TestApplyGroups_DefaultExcludesReplaceToo: the unlabelled bucket's
// negative selector must exclude replace too, because replace's
// resources are re-applied inside the replace group's second
// invocation. If the default group also re-applied them we'd
// double-apply -- visible as duplicate "<kind>/<name> created"
// lines on first run.
func TestApplyGroups_DefaultExcludesReplaceToo(t *testing.T) {
	groups := applyGroups(Options{Context: "local", KustomizeDir: "/tmp/k"})
	def := groups[4]
	if def.mode != "" {
		t.Fatalf("group 4 should be the default (mode=\"\") group, got mode=%q", def.mode)
	}
	args := def.invocations[0].args
	var sel string
	for _, a := range args {
		if strings.HasPrefix(a, "--selector=") {
			sel = a
			break
		}
	}
	for _, mode := range []string{"create", "replace", "serverside", "serverside-force"} {
		want := ConvergeModeLabel + "!=" + mode
		if !strings.Contains(sel, want) {
			t.Errorf("default selector missing %q\nsel: %s", want, sel)
		}
	}
}

// TestApplyGroups_DryRunForwards: --dry-run=server must reach
// every invocation, including delete and replace's recreate, so
// a dry-run of a replace-mode resource is provably non-mutating
// end-to-end.
func TestApplyGroups_DryRunForwards(t *testing.T) {
	groups := applyGroups(Options{Context: "local", KustomizeDir: "/tmp/k", DryRun: "server"})
	for gi, g := range groups {
		for ii, inv := range g.invocations {
			found := false
			for _, a := range inv.args {
				if a == "--dry-run=server" {
					found = true
					break
				}
			}
			if !found {
				t.Errorf("group %d (mode=%q) invocation %d missing --dry-run=server: %v",
					gi, g.mode, ii, inv.args)
			}
		}
	}
}

// TestApplyGroups_NoDryRunByDefault confirms the inverse: an
// empty Options.DryRun must not insert --dry-run=anything. Catches
// a typo where a default value (e.g. "none") gets forwarded.
func TestApplyGroups_NoDryRunByDefault(t *testing.T) {
	groups := applyGroups(Options{Context: "local", KustomizeDir: "/tmp/k"})
	for gi, g := range groups {
		for ii, inv := range g.invocations {
			for _, a := range inv.args {
				if strings.HasPrefix(a, "--dry-run=") {
					t.Errorf("group %d (mode=%q) invocation %d unexpectedly carries %q",
						gi, g.mode, ii, a)
				}
			}
		}
	}
}

// TestApplyGroups_TolerateContract pins the per-invocation
// "expected idempotent" output substrings, split between the
// two channels kubectl uses:
//
//   - stderrTolerate: stderr substrings that, together with a
//     non-zero exit, count as success ("no objects passed to
//     <verb>" empty-selector case, "AlreadyExists" on create
//     re-runs).
//   - stdoutSuppress: stdout substrings that, on otherwise
//     successful runs, are dropped before forwarding (kubectl
//     delete's "No resources found" line on empty match).
//
// Loosening either silently swallows a real bug or noises up
// the user-facing output.
func TestApplyGroups_TolerateContract(t *testing.T) {
	groups := applyGroups(Options{Context: "local", KustomizeDir: "/tmp/k"})

	type pair struct {
		stderr []string
		stdout []string
	}
	want := [][]pair{
		// create
		{
			{stderr: []string{"AlreadyExists", "no objects passed to create"}},
		},
		// replace: delete + apply
		{
			{stdout: []string{"No resources found"}},
			{stderr: []string{"no objects passed to apply"}},
		},
		// serverside-force
		{
			{stderr: []string{"no objects passed to apply"}},
		},
		// serverside
		{
			{stderr: []string{"no objects passed to apply"}},
		},
		// default
		{
			{stderr: []string{"no objects passed to apply"}},
		},
	}
	for gi, g := range groups {
		if len(g.invocations) != len(want[gi]) {
			t.Errorf("group %d invocations: %d want %d", gi, len(g.invocations), len(want[gi]))
			continue
		}
		for ii, inv := range g.invocations {
			if !reflect.DeepEqual(inv.stderrTolerate, want[gi][ii].stderr) {
				t.Errorf("group %d (mode=%q) inv %d stderrTolerate: got %v want %v",
					gi, g.mode, ii, inv.stderrTolerate, want[gi][ii].stderr)
			}
			if !reflect.DeepEqual(inv.stdoutSuppress, want[gi][ii].stdout) {
				t.Errorf("group %d (mode=%q) inv %d stdoutSuppress: got %v want %v",
					gi, g.mode, ii, inv.stdoutSuppress, want[gi][ii].stdout)
			}
		}
	}
}
