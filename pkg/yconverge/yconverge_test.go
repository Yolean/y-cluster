package yconverge

import (
	"context"
	"path/filepath"
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
	// No cue.mod — should still work (single step, no dep resolution)
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
