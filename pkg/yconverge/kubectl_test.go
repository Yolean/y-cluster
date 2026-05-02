package yconverge

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"testing"
)

// fakeKubectlOnPATH writes an executable shell script named
// `kubectl` to a fresh temp dir and prepends that dir to $PATH for
// the test. preflightSelectorMatches resolves "kubectl" by name, so
// the script runs in place of any real kubectl on the host.
//
// `body` is the shell body, no shebang. Two patterns are useful:
//
//   - Capture argv: `printf '%s\n' "$@" > "$LOG"` (with $LOG passed
//     via t.Setenv) lets the test inspect the exact argv yconverge
//     constructed.
//   - Simulate kubectl: `echo configmap/foo` for one match, `exit 0`
//     with no stdout for zero matches via empty output, or
//     `echo "no objects passed to apply" >&2; exit 1` for kubectl's
//     real zero-match shape.
//
// Same shape as fakeKubectlOnPATH in pkg/provision/docker/docker_test.go;
// kept package-local for unit-test discoverability.
func fakeKubectlOnPATH(t *testing.T, body string) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("fake kubectl shim is /bin/sh-only")
	}
	dir := t.TempDir()
	script := "#!/bin/sh\n" + body + "\n"
	if err := os.WriteFile(filepath.Join(dir, "kubectl"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
}

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

// findSelector returns the --selector=... arg from inv.args, or ""
// if none. Used by the selector-composition tests below.
func findSelector(args []string) string {
	for _, a := range args {
		if strings.HasPrefix(a, "--selector=") {
			return strings.TrimPrefix(a, "--selector=")
		}
	}
	return ""
}

// TestApplyGroups_UserSelectorANDsIntoEveryGroup pins the propagation
// contract: a non-empty Options.Selector is appended to every group's
// internal converge-mode selector via comma (kubectl's AND separator).
// Without this, a -l app=foo run would route correctly per mode but
// still apply foo-less resources -- defeating the user's filter.
func TestApplyGroups_UserSelectorANDsIntoEveryGroup(t *testing.T) {
	user := "app=foo"
	groups := applyGroups(Options{
		Context:      "local",
		KustomizeDir: "/tmp/k",
		Selector:     user,
	})
	wantSuffix := "," + user
	for gi, g := range groups {
		for ii, inv := range g.invocations {
			sel := findSelector(inv.args)
			if sel == "" {
				t.Errorf("group %d (mode=%q) inv %d: no --selector arg: %v", gi, g.mode, ii, inv.args)
				continue
			}
			if !strings.HasSuffix(sel, wantSuffix) {
				t.Errorf("group %d (mode=%q) inv %d selector %q: missing suffix %q",
					gi, g.mode, ii, sel, wantSuffix)
			}
		}
	}
}

// TestApplyGroups_EmptySelectorLeavesInternalSelectorAlone is the
// inverse: with no user selector, the internal converge-mode body is
// emitted verbatim. Catches a regression where the user-selector
// branch always appended a trailing comma.
func TestApplyGroups_EmptySelectorLeavesInternalSelectorAlone(t *testing.T) {
	groups := applyGroups(Options{Context: "local", KustomizeDir: "/tmp/k"})

	// create's selector should be exactly the converge-mode body.
	createSel := findSelector(groups[0].invocations[0].args)
	want := ConvergeModeLabel + "=create"
	if createSel != want {
		t.Errorf("create selector: %q want %q", createSel, want)
	}

	// default's selector should not end with a stray comma.
	defSel := findSelector(groups[4].invocations[0].args)
	if strings.HasSuffix(defSel, ",") {
		t.Errorf("default selector trailing comma: %q", defSel)
	}
}

// TestApplyGroups_UserSelectorPreservesDefaultExclusions: combining a
// user selector with the default group's negative converge-mode
// selector must not clobber any of the four !=mode terms.
func TestApplyGroups_UserSelectorPreservesDefaultExclusions(t *testing.T) {
	groups := applyGroups(Options{
		Context:      "local",
		KustomizeDir: "/tmp/k",
		Selector:     "app=foo",
	})
	defSel := findSelector(groups[4].invocations[0].args)
	for _, mode := range []string{"create", "replace", "serverside", "serverside-force"} {
		want := ConvergeModeLabel + "!=" + mode
		if !strings.Contains(defSel, want) {
			t.Errorf("default selector lost %q after user-selector AND: %q", want, defSel)
		}
	}
	if !strings.Contains(defSel, "app=foo") {
		t.Errorf("default selector missing user term app=foo: %q", defSel)
	}
}

// TestApplyGroups_UserSelectorWithSetSyntax exercises kubectl's `in`
// set form, which contains parentheses and spaces. yconverge composes
// by literal `,` concat -- the body should be passed through
// unmodified so kubectl's selector parser sees the same string the
// user typed.
func TestApplyGroups_UserSelectorWithSetSyntax(t *testing.T) {
	user := "tier in (frontend, backend)"
	groups := applyGroups(Options{
		Context:      "local",
		KustomizeDir: "/tmp/k",
		Selector:     user,
	})
	got := findSelector(groups[0].invocations[0].args)
	want := ConvergeModeLabel + "=create," + user
	if got != want {
		t.Errorf("create selector: %q want %q", got, want)
	}
}

// TestPreflightSelectorMatches_Argv pins the kubectl argv the
// preflight builds. A regression that drops --dry-run=client (and
// silently round-trips to the apiserver), drops -o name (and
// breaks the count parser), or builds a different flag order
// would fail this test before any cluster involvement.
func TestPreflightSelectorMatches_Argv(t *testing.T) {
	argvLog := filepath.Join(t.TempDir(), "argv.log")
	t.Setenv("YC_TEST_ARGV_LOG", argvLog)
	// The fake kubectl writes its argv (one arg per line) to the
	// log file, prints two `<kind>/<name>` lines so the count is
	// verifiable, and exits 0.
	fakeKubectlOnPATH(t,
		`printf '%s\n' "$@" > "$YC_TEST_ARGV_LOG"
echo configmap/foo
echo configmap/bar`)

	n, err := preflightSelectorMatches(context.Background(), Options{
		Context:      "ctx-name",
		KustomizeDir: "/some/kustomize/dir",
		Selector:     "app=foo",
	})
	if err != nil {
		t.Fatalf("preflight: %v", err)
	}
	if n != 2 {
		t.Errorf("count: got %d, want 2", n)
	}

	logBytes, err := os.ReadFile(argvLog)
	if err != nil {
		t.Fatal(err)
	}
	gotArgv := strings.Split(strings.TrimSpace(string(logBytes)), "\n")
	wantArgv := []string{
		"--context=ctx-name",
		"apply", "--dry-run=client",
		"-k", "/some/kustomize/dir",
		"-l", "app=foo",
		"-o", "name",
	}
	if !reflect.DeepEqual(gotArgv, wantArgv) {
		t.Errorf("argv:\n  got:  %v\n  want: %v", gotArgv, wantArgv)
	}
}

// TestPreflightSelectorMatches_ZeroMatchTreatedAsZero exercises
// the kubectl-real-world zero-match shape: exit 1 with
// "no objects passed to apply" on stderr. The preflight must
// recognise that as (0, nil) -- without this, convergeSingle
// would surface kubectl's exit-1 as an apply error rather than
// the user-friendly "selector matched no resources" bail.
func TestPreflightSelectorMatches_ZeroMatchTreatedAsZero(t *testing.T) {
	fakeKubectlOnPATH(t,
		`echo "error: no objects passed to apply" >&2
exit 1`)

	n, err := preflightSelectorMatches(context.Background(), Options{
		Context:      "ctx",
		KustomizeDir: "/k",
		Selector:     "app=missing",
	})
	if err != nil {
		t.Fatalf("zero-match must return nil error, got: %v", err)
	}
	if n != 0 {
		t.Errorf("zero-match count: got %d, want 0", n)
	}
}

// TestPreflightSelectorMatches_EmptyStdoutIsZero covers the rare
// "kubectl exits 0 with no resources" path (e.g. an empty
// kustomization that nevertheless renders OK under dry-run).
// trimmed == "" must produce 0, not 1 (which a naive
// strings.Count("\n")+1 would otherwise yield).
func TestPreflightSelectorMatches_EmptyStdoutIsZero(t *testing.T) {
	fakeKubectlOnPATH(t, `exit 0`)
	n, err := preflightSelectorMatches(context.Background(), Options{
		Context:      "ctx",
		KustomizeDir: "/k",
		Selector:     "app=foo",
	})
	if err != nil {
		t.Fatalf("empty stdout: %v", err)
	}
	if n != 0 {
		t.Errorf("empty stdout: got %d, want 0", n)
	}
}

// TestPreflightSelectorMatches_OtherErrorPropagates: any kubectl
// failure that isn't the "no objects" zero-match shape must
// surface as an error -- silent (0, nil) on a real failure
// would mask broken kustomize trees, missing contexts, or
// daemon outages as "selector matched no resources".
func TestPreflightSelectorMatches_OtherErrorPropagates(t *testing.T) {
	fakeKubectlOnPATH(t,
		`echo "error: cluster is unreachable" >&2
exit 1`)
	_, err := preflightSelectorMatches(context.Background(), Options{
		Context:      "ctx",
		KustomizeDir: "/k",
		Selector:     "app=foo",
	})
	if err == nil {
		t.Fatal("expected error from kubectl non-zero exit, got nil")
	}
	if !strings.Contains(err.Error(), "cluster is unreachable") {
		t.Errorf("error should preserve kubectl's stderr context: %v", err)
	}
}

// TestPreflightSelectorMatches_CountsLines covers the boundary
// at exactly one match -- a single name with a trailing newline
// must count as 1, not 2.
func TestPreflightSelectorMatches_CountsLines(t *testing.T) {
	cases := []struct {
		body string
		want int
	}{
		{`echo configmap/only`, 1},
		{`printf 'configmap/a\nconfigmap/b\nconfigmap/c\n'`, 3},
		// Trailing whitespace shouldn't inflate the count.
		{`printf 'configmap/a\nconfigmap/b\n\n  \n'`, 2},
	}
	for _, c := range cases {
		t.Run(c.body, func(t *testing.T) {
			fakeKubectlOnPATH(t, c.body)
			n, err := preflightSelectorMatches(context.Background(), Options{
				Context:      "ctx",
				KustomizeDir: "/k",
				Selector:     "app=foo",
			})
			if err != nil {
				t.Fatalf("preflight: %v", err)
			}
			if n != c.want {
				t.Errorf("count: got %d, want %d", n, c.want)
			}
		})
	}
}
