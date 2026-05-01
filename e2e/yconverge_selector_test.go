//go:build e2e

package e2e

import (
	"context"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/Yolean/y-cluster/pkg/yconverge"
)

// allFooNames is the foo-labelled subset of the selector fixture
// (5 in target, 1 in dep). One per converge-mode plus the default.
var allFooNames = []string{
	"foo-target", // default mode
	"foo-create",
	"foo-replace",
	"foo-ssforce",
	"foo-serverside",
	"foo-dep", // dep, default mode
}

// allBarNames is the bar-labelled sibling set: same modes, opposite
// label. Selectors of the form `app=foo` should never touch these.
var allBarNames = []string{
	"bar-target",
	"bar-create",
	"bar-replace",
	"bar-ssforce",
	"bar-serverside",
	"bar-dep",
}

func allSelectorNames() []string {
	all := append([]string{}, allFooNames...)
	all = append(all, allBarNames...)
	sort.Strings(all)
	return all
}

// configMapExists reports whether a ConfigMap with the given name
// exists in the default namespace of the kwok cluster. Used by the
// selector tests to assert which resources made it through the
// label filter and which didn't.
func configMapExists(t *testing.T, name string) bool {
	t.Helper()
	out, err := exec.Command("kubectl", "--context="+contextName,
		"get", "configmap", name, "--ignore-not-found=true",
		"-o", "jsonpath={.metadata.name}").CombinedOutput()
	if err != nil {
		t.Fatalf("get configmap %s: %s: %v", name, out, err)
	}
	return strings.TrimSpace(string(out)) == name
}

// configMapUID is the cluster-assigned UID of the named ConfigMap,
// or "" if absent. A delete+recreate changes the UID; an unchanged
// resource keeps it -- which is how the replace-safety test proves
// "untouched" rather than "deleted-and-restored-with-same-content".
func configMapUID(t *testing.T, name string) string {
	t.Helper()
	out, err := exec.Command("kubectl", "--context="+contextName,
		"get", "configmap", name, "--ignore-not-found=true",
		"-o", "jsonpath={.metadata.uid}").CombinedOutput()
	if err != nil {
		t.Fatalf("get configmap %s uid: %s: %v", name, out, err)
	}
	return strings.TrimSpace(string(out))
}

// deleteConfigMaps best-effort removes the named configmaps so a
// subsequent test sees a clean slate. kwok happily ignores absent
// resources via --ignore-not-found.
func deleteConfigMaps(t *testing.T, names ...string) {
	t.Helper()
	args := append([]string{"--context=" + contextName, "delete", "configmap",
		"--ignore-not-found=true"}, names...)
	if out, err := exec.Command("kubectl", args...).CombinedOutput(); err != nil {
		t.Logf("cleanup delete %v: %s: %v", names, out, err)
	}
}

// TestSelector_NoSelectorAppliesAll is the baseline: every resource
// the kustomize tree renders gets applied (10 in target, 2 in dep,
// across all five converge modes). Without this baseline a passing
// selector test could be vacuous (resources never applying anyway).
func TestSelector_NoSelectorAppliesAll(t *testing.T) {
	setupCluster(t)
	td := testdataDir(t)
	log := logger(t)

	t.Cleanup(func() { deleteConfigMaps(t, allSelectorNames()...) })

	if _, err := yconverge.Run(context.Background(), yconverge.Options{
		Context:      contextName,
		KustomizeDir: filepath.Join(td, "e2e-selector-target/base"),
	}, log); err != nil {
		t.Fatal(err)
	}

	for _, name := range allSelectorNames() {
		if !configMapExists(t, name) {
			t.Errorf("configmap %q should exist after no-selector apply", name)
		}
	}
}

// TestSelector_FiltersAcrossAllConvergeModes is the comprehensive
// propagation+routing contract: with `-l app=foo`, every converge
// mode (default / create / replace / serverside-force / serverside)
// applies its foo-* resource and skips its bar-* sibling, in BOTH
// the target and the dep. Catches selector-AND-ing bugs that would
// route a mode-labelled resource to the wrong bucket as well as
// dep-propagation regressions.
func TestSelector_FiltersAcrossAllConvergeModes(t *testing.T) {
	setupCluster(t)
	td := testdataDir(t)
	log := logger(t)

	t.Cleanup(func() { deleteConfigMaps(t, allSelectorNames()...) })
	deleteConfigMaps(t, allSelectorNames()...) // pre-clean

	if _, err := yconverge.Run(context.Background(), yconverge.Options{
		Context:      contextName,
		KustomizeDir: filepath.Join(td, "e2e-selector-target/base"),
		Selector:     "app=foo",
	}, log); err != nil {
		t.Fatal(err)
	}

	for _, name := range allFooNames {
		if !configMapExists(t, name) {
			t.Errorf("configmap %q should exist after `-l app=foo` apply", name)
		}
	}
	for _, name := range allBarNames {
		if configMapExists(t, name) {
			t.Errorf("configmap %q should NOT exist after `-l app=foo` apply (selector failed to filter or did not propagate)", name)
		}
	}
}

// TestSelector_IdempotentReapply pins idempotence under selector
// across all modes. The create mode's tolerate-AlreadyExists, the
// replace mode's delete+reapply, and the serverside modes'
// merge-on-conflict semantics each have to survive a second run
// with the same selector. Without per-mode tolerance the second
// run would error somewhere (typically AlreadyExists on create).
func TestSelector_IdempotentReapply(t *testing.T) {
	setupCluster(t)
	td := testdataDir(t)
	log := logger(t)

	t.Cleanup(func() { deleteConfigMaps(t, allSelectorNames()...) })
	deleteConfigMaps(t, allSelectorNames()...) // pre-clean

	for i := 0; i < 2; i++ {
		if _, err := yconverge.Run(context.Background(), yconverge.Options{
			Context:      contextName,
			KustomizeDir: filepath.Join(td, "e2e-selector-target/base"),
			Selector:     "app=foo",
		}, log); err != nil {
			t.Fatalf("apply %d: %v", i+1, err)
		}
	}
	for _, name := range allFooNames {
		if !configMapExists(t, name) {
			t.Errorf("configmap %q should still exist after re-apply", name)
		}
	}
}

// TestSelector_ReplaceModeSafetyAcrossSelectorChange is the safety
// contract: a resource matching the kustomize tree but NOT the
// user selector must never be touched, even when its converge-mode
// is replace (the only mode that issues a delete). Proof:
//
//  1. Apply unfiltered: bar-replace lands on the cluster.
//  2. Note bar-replace's UID.
//  3. Re-apply with `-l app=foo`: replace-mode's delete-step is
//     `kubectl delete -k <dir> --selector=converge-mode=replace,app=foo`
//     -- the AND of the user selector with the converge-mode
//     filter MUST exclude bar-replace.
//  4. Assert bar-replace still exists with the SAME UID. A
//     delete-then-recreate would change the UID; an untouched
//     resource keeps it.
//
// This is the regression guard against any future "user selector
// shortcuts that bypass the converge-mode AND" bug.
func TestSelector_ReplaceModeSafetyAcrossSelectorChange(t *testing.T) {
	setupCluster(t)
	td := testdataDir(t)
	log := logger(t)

	t.Cleanup(func() { deleteConfigMaps(t, allSelectorNames()...) })
	deleteConfigMaps(t, allSelectorNames()...) // pre-clean

	if _, err := yconverge.Run(context.Background(), yconverge.Options{
		Context:      contextName,
		KustomizeDir: filepath.Join(td, "e2e-selector-target/base"),
	}, log); err != nil {
		t.Fatal(err)
	}
	beforeUID := configMapUID(t, "bar-replace")
	if beforeUID == "" {
		t.Fatal("bar-replace missing after unfiltered apply -- baseline broken")
	}

	if _, err := yconverge.Run(context.Background(), yconverge.Options{
		Context:      contextName,
		KustomizeDir: filepath.Join(td, "e2e-selector-target/base"),
		Selector:     "app=foo",
	}, log); err != nil {
		t.Fatal(err)
	}
	afterUID := configMapUID(t, "bar-replace")
	if afterUID == "" {
		t.Fatal("bar-replace was DELETED by `-l app=foo` apply -- selector failed to constrain replace-mode delete")
	}
	if afterUID != beforeUID {
		t.Fatalf("bar-replace UID changed across selector apply: before=%q after=%q -- a delete+recreate happened that shouldn't have",
			beforeUID, afterUID)
	}
}

// TestSelector_BailsWhenNoMatch is the non-obvious-no-op safeguard:
// a non-empty user selector that matches zero resources in the
// kustomize tree is refused with a clear error rather than silently
// succeeding via the per-group "no objects passed to apply" tolerance.
// Catches the "user typo'd their selector and the apply skipped
// silently" UX trap.
func TestSelector_BailsWhenNoMatch(t *testing.T) {
	setupCluster(t)
	td := testdataDir(t)
	log := logger(t)

	_, err := yconverge.Run(context.Background(), yconverge.Options{
		Context:      contextName,
		KustomizeDir: filepath.Join(td, "e2e-selector-target/base"),
		Selector:     "app=does-not-exist",
	}, log)
	if err == nil {
		t.Fatal("expected error when selector matches no resources, got nil")
	}
	for _, want := range []string{"selector", "no resources", "refusing"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q should mention %q so the user can fix the typo", err.Error(), want)
		}
	}
}
