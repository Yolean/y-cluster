package yconverge

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"go.uber.org/zap"
)

func TestRun_PrintDeps(t *testing.T) {
	root := t.TempDir()

	// Create a CUE module root matching the import prefix ParseImports expects
	writeFile(t, filepath.Join(root, "cue.mod/module.cue"), `module: "yolean.se/ystack"`)

	// Base with no deps
	writeFile(t, filepath.Join(root, "base/kustomization.yaml"), "")
	writeFile(t, filepath.Join(root, "base/yconverge.cue"), `
package base
step: checks: []
`)
	// Target depends on base
	writeFile(t, filepath.Join(root, "target/kustomization.yaml"), "")
	writeFile(t, filepath.Join(root, "target/yconverge.cue"), `
package target
import "yolean.se/ystack/base:base"
_dep: base.step
step: checks: []
`)

	logger, _ := zap.NewDevelopment()
	result, err := Run(context.Background(), Options{
		Context:      "test",
		KustomizeDir: filepath.Join(root, "target"),
		PrintDeps:    true,
	}, logger)
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Steps) != 2 {
		t.Fatalf("expected 2 steps, got %d: %v", len(result.Steps), result.Steps)
	}
	if filepath.Base(result.Steps[0]) != "base" {
		t.Fatalf("expected base first, got %s", filepath.Base(result.Steps[0]))
	}
	if filepath.Base(result.Steps[1]) != "target" {
		t.Fatalf("expected target last, got %s", filepath.Base(result.Steps[1]))
	}
}

func TestRun_NoCueModule(t *testing.T) {
	root := t.TempDir()
	// No cue.mod -- should still work (single step, no dep resolution)
	writeFile(t, filepath.Join(root, "base/kustomization.yaml"), "")

	logger, _ := zap.NewDevelopment()
	result, err := Run(context.Background(), Options{
		Context:      "test",
		KustomizeDir: filepath.Join(root, "base"),
		PrintDeps:    true,
	}, logger)
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Steps) != 1 {
		t.Fatalf("expected 1 step, got %d", len(result.Steps))
	}
}

func TestRun_MissingContext(t *testing.T) {
	logger, _ := zap.NewDevelopment()
	_, err := Run(context.Background(), Options{
		KustomizeDir: "/tmp",
	}, logger)
	if err == nil {
		t.Fatal("expected error for missing context")
	}
}

func TestRun_MissingDir(t *testing.T) {
	logger, _ := zap.NewDevelopment()
	_, err := Run(context.Background(), Options{
		Context:      "test",
		KustomizeDir: "",
	}, logger)
	if err == nil {
		t.Fatal("expected error for missing dir")
	}
}

// TestRun_TraverseErrorIsFatal covers Q12: a corrupt kustomization
// inside a CUE module must surface as a fatal error rather than
// fall through to a single-step run that silently skips checks.
func TestRun_TraverseErrorIsFatal(t *testing.T) {
	logger, _ := zap.NewDevelopment()
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "cue.mod"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "cue.mod", "module.cue"),
		[]byte(`module: "yolean.se/ystack"`), 0o644); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(root, "broken")
	if err := os.MkdirAll(target, 0o755); err != nil {
		t.Fatal(err)
	}
	// Invalid YAML in kustomization triggers the traverse failure.
	if err := os.WriteFile(filepath.Join(target, "kustomization.yaml"),
		[]byte("resources:\n- ../missing\n  bogus: ["), 0o644); err != nil {
		t.Fatal(err)
	}
	_, err := Run(context.Background(), Options{
		Context:      "test",
		KustomizeDir: target,
		PrintDeps:    true, // keep network/cluster out of the test
	}, logger)
	if err == nil {
		t.Fatal("expected traverse error to be fatal")
	}
	if !strings.Contains(err.Error(), "traverse") {
		t.Fatalf("expected traverse error wrap, got %v", err)
	}
}

// An unknown --dry-run value used to fall through to a real apply.
func TestRun_RejectsUnknownDryRun(t *testing.T) {
	for _, v := range []string{"client", "true", "sever"} {
		_, err := Run(context.Background(), Options{Context: "x", KustomizeDir: t.TempDir(), DryRun: v}, nil)
		if err == nil || !strings.Contains(err.Error(), "refusing to apply") {
			t.Errorf("--dry-run=%s: want a refusal before anything runs, got %v", v, err)
		}
	}
}

func TestNormalizeDryRun(t *testing.T) {
	for in, want := range map[string]string{"": "", "none": "", "server": "server"} {
		got, err := normalizeDryRun(in)
		if err != nil || got != want {
			t.Errorf("normalizeDryRun(%q) = %q, %v; want %q", in, got, err, want)
		}
	}
}

// stepsFor returns the converge plan for a testdata directory,
// relative to testdata/, without touching a cluster.
func stepsFor(t *testing.T, dir string) []string {
	t.Helper()
	td, err := filepath.Abs(filepath.Join("..", "..", "testdata"))
	if err != nil {
		t.Fatal(err)
	}
	result, err := Run(context.Background(), Options{
		Context:      "unused",
		KustomizeDir: filepath.Join(td, dir),
		PrintDeps:    true,
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	var rel []string
	for _, s := range result.Steps {
		r, err := filepath.Rel(td, s)
		if err != nil {
			t.Fatal(err)
		}
		rel = append(rel, r)
	}
	return rel
}

// e2e-backend/qa is an overlay without a yconverge.cue of its own; its
// base, e2e-backend/base, has one that imports e2e-db/base.
//
// The overlay inherits the base's dependency. The base itself is not
// a step: it is part of the overlay and applied as the overlay builds
// it. As a step of its own it used to be applied first and unpatched,
// which for an overlay that sets a namespace or a name prefix puts
// resources where they do not belong.
func TestRun_OverlayInheritsBaseDepsButDoesNotApplyTheBaseSeparately(t *testing.T) {
	got := stepsFor(t, "e2e-backend/qa")
	want := []string{"e2e-db/base", "e2e-backend/qa"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("plan for the overlay:\ngot  %v\nwant %v", got, want)
	}
}

// Converging the base directly is unchanged: its dependency, then it.
func TestRun_BaseWithDependency(t *testing.T) {
	got := stepsFor(t, "e2e-backend/base")
	want := []string{"e2e-db/base", "e2e-backend/base"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("plan for the base:\ngot  %v\nwant %v", got, want)
	}
}

// An overlay over a base without imports is a single step.
func TestRun_OverlayOverLeafBaseIsOneStep(t *testing.T) {
	got := stepsFor(t, "e2e-db/qa")
	want := []string{"e2e-db/qa"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("plan:\ngot  %v\nwant %v", got, want)
	}
}
