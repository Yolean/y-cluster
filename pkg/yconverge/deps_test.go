package yconverge

import (
	"path/filepath"
	"testing"
)

func TestResolveDeps_NoDeps(t *testing.T) {
	root := t.TempDir()
	writeFile(t, filepath.Join(root, "base/yconverge.cue"), `
package base
step: checks: []
`)
	writeFile(t, filepath.Join(root, "base/kustomization.yaml"), "")

	order, err := ResolveDeps(root, filepath.Join(root, "base"))
	if err != nil {
		t.Fatal(err)
	}
	if len(order) != 1 {
		t.Fatalf("expected 1 entry, got %v", order)
	}
}

func TestResolveDeps_LinearChain(t *testing.T) {
	root := t.TempDir()

	// a depends on b, b depends on c
	writeFile(t, filepath.Join(root, "c/yconverge.cue"), `
package c
step: checks: []
`)
	writeFile(t, filepath.Join(root, "b/yconverge.cue"), `
package b
import "yolean.se/ystack/c:c"
_dep: c.step
step: checks: []
`)
	writeFile(t, filepath.Join(root, "a/yconverge.cue"), `
package a
import "yolean.se/ystack/b:b"
_dep: b.step
step: checks: []
`)

	order, err := ResolveDeps(root, filepath.Join(root, "a"))
	if err != nil {
		t.Fatal(err)
	}
	if len(order) != 3 {
		t.Fatalf("expected 3 entries, got %d: %v", len(order), order)
	}
	// c first, then b, then a
	if filepath.Base(order[0]) != "c" {
		t.Fatalf("expected c first, got %s", order[0])
	}
	if filepath.Base(order[1]) != "b" {
		t.Fatalf("expected b second, got %s", order[1])
	}
	if filepath.Base(order[2]) != "a" {
		t.Fatalf("expected a last, got %s", order[2])
	}
}

func TestResolveDeps_DiamondDependency(t *testing.T) {
	root := t.TempDir()

	// top depends on left and right, both depend on shared
	writeFile(t, filepath.Join(root, "shared/yconverge.cue"), `
package shared
step: checks: []
`)
	writeFile(t, filepath.Join(root, "left/yconverge.cue"), `
package left
import "yolean.se/ystack/shared:shared"
_dep: shared.step
step: checks: []
`)
	writeFile(t, filepath.Join(root, "right/yconverge.cue"), `
package right
import "yolean.se/ystack/shared:shared"
_dep: shared.step
step: checks: []
`)
	writeFile(t, filepath.Join(root, "top/yconverge.cue"), `
package top
import (
	"yolean.se/ystack/left:left"
	"yolean.se/ystack/right:right"
)
_dep_l: left.step
_dep_r: right.step
step: checks: []
`)

	order, err := ResolveDeps(root, filepath.Join(root, "top"))
	if err != nil {
		t.Fatal(err)
	}
	if len(order) != 4 {
		t.Fatalf("expected 4 entries, got %d: %v", len(order), order)
	}
	// shared must come before both left and right
	sharedIdx := indexOf(order, "shared")
	leftIdx := indexOf(order, "left")
	rightIdx := indexOf(order, "right")
	topIdx := indexOf(order, "top")

	if sharedIdx < 0 || leftIdx < 0 || rightIdx < 0 || topIdx < 0 {
		t.Fatalf("missing entries: %v", order)
	}
	if sharedIdx >= leftIdx || sharedIdx >= rightIdx {
		t.Fatalf("shared must come before left and right: %v", basenames(order))
	}
	if topIdx != len(order)-1 {
		t.Fatalf("top must be last: %v", basenames(order))
	}
}

func TestResolveDeps_NoCueFile(t *testing.T) {
	root := t.TempDir()
	writeFile(t, filepath.Join(root, "base/kustomization.yaml"), "")
	// No yconverge.cue — should return just the target
	order, err := ResolveDeps(root, filepath.Join(root, "base"))
	if err != nil {
		t.Fatal(err)
	}
	if len(order) != 1 {
		t.Fatalf("expected 1 entry (target only), got %v", order)
	}
}

func TestResolveDeps_VisitsEachOnce(t *testing.T) {
	root := t.TempDir()
	writeFile(t, filepath.Join(root, "shared/yconverge.cue"), `
package shared
step: checks: []
`)
	// Both a and b depend on shared
	writeFile(t, filepath.Join(root, "a/yconverge.cue"), `
package a
import "yolean.se/ystack/shared:shared"
_dep: shared.step
step: checks: []
`)
	writeFile(t, filepath.Join(root, "b/yconverge.cue"), `
package b
import "yolean.se/ystack/shared:shared"
_dep: shared.step
step: checks: []
`)
	// top depends on a and b
	writeFile(t, filepath.Join(root, "top/yconverge.cue"), `
package top
import (
	"yolean.se/ystack/a:a"
	"yolean.se/ystack/b:b"
)
_dep_a: a.step
_dep_b: b.step
step: checks: []
`)

	order, err := ResolveDeps(root, filepath.Join(root, "top"))
	if err != nil {
		t.Fatal(err)
	}
	// shared should appear exactly once
	count := 0
	for _, d := range order {
		if filepath.Base(d) == "shared" {
			count++
		}
	}
	if count != 1 {
		t.Fatalf("shared should appear once, got %d in %v", count, basenames(order))
	}
}

func TestFindCueModuleRoot(t *testing.T) {
	root := t.TempDir()
	writeFile(t, filepath.Join(root, "cue.mod/module.cue"), `module: "test"`)
	writeFile(t, filepath.Join(root, "deep/nested/dir/file.txt"), "")

	got := FindCueModuleRoot(filepath.Join(root, "deep/nested/dir"))
	abs, _ := filepath.Abs(root)
	if got != abs {
		t.Fatalf("expected %s, got %s", abs, got)
	}
}

func TestFindCueModuleRoot_NotFound(t *testing.T) {
	root := t.TempDir()
	got := FindCueModuleRoot(root)
	if got != "" {
		t.Fatalf("expected empty, got %s", got)
	}
}

func indexOf(order []string, name string) int {
	for i, d := range order {
		if filepath.Base(d) == name {
			return i
		}
	}
	return -1
}

func basenames(paths []string) []string {
	var names []string
	for _, p := range paths {
		names = append(names, filepath.Base(p))
	}
	return names
}
