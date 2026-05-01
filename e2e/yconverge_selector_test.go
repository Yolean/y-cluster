//go:build e2e

package e2e

import (
	"context"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Yolean/y-cluster/pkg/yconverge"
)

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

// TestSelector_NoSelectorAppliesAll is the baseline: with an empty
// Options.Selector, every resource the kustomize tree renders gets
// applied -- both labels, both modules. Without this baseline a
// passing TestSelector_FiltersTargetAndDep could be vacuous (the
// resources never applying anyway).
func TestSelector_NoSelectorAppliesAll(t *testing.T) {
	setupCluster(t)
	td := testdataDir(t)
	log := logger(t)

	t.Cleanup(func() {
		deleteConfigMaps(t, "foo-target", "bar-target", "foo-dep", "bar-dep")
	})

	if _, err := yconverge.Run(context.Background(), yconverge.Options{
		Context:      contextName,
		KustomizeDir: filepath.Join(td, "e2e-selector-target/base"),
	}, log); err != nil {
		t.Fatal(err)
	}

	for _, name := range []string{"foo-target", "bar-target", "foo-dep", "bar-dep"} {
		if !configMapExists(t, name) {
			t.Errorf("configmap %q should exist after no-selector apply", name)
		}
	}
}

// TestSelector_FiltersTargetAndDep is the propagation contract:
// `-l app=foo` on a target with a dep applies only foo-labelled
// resources in BOTH the target and the dep. Without propagation
// the dep would apply unfiltered (foo-dep AND bar-dep both land),
// which is the bug the Selector field on depOpts in Run prevents.
func TestSelector_FiltersTargetAndDep(t *testing.T) {
	setupCluster(t)
	td := testdataDir(t)
	log := logger(t)

	t.Cleanup(func() {
		deleteConfigMaps(t, "foo-target", "bar-target", "foo-dep", "bar-dep")
	})

	// Pre-clean in case a prior test left resources around.
	deleteConfigMaps(t, "foo-target", "bar-target", "foo-dep", "bar-dep")

	if _, err := yconverge.Run(context.Background(), yconverge.Options{
		Context:      contextName,
		KustomizeDir: filepath.Join(td, "e2e-selector-target/base"),
		Selector:     "app=foo",
	}, log); err != nil {
		t.Fatal(err)
	}

	// foo-* should be applied in both target and dep.
	for _, name := range []string{"foo-target", "foo-dep"} {
		if !configMapExists(t, name) {
			t.Errorf("configmap %q should exist after `-l app=foo` apply", name)
		}
	}

	// bar-* should be filtered out everywhere -- including the dep.
	// Without selector propagation the dep would apply unfiltered
	// and bar-dep would land here.
	for _, name := range []string{"bar-target", "bar-dep"} {
		if configMapExists(t, name) {
			t.Errorf("configmap %q should NOT exist after `-l app=foo` apply (selector did not propagate to dep, or did not filter target)", name)
		}
	}
}
