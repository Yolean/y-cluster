package yconverge

import (
	"os"
	"path/filepath"
	"testing"
)

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestParseImports_ExtractsDependencies(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "yconverge.cue"), `
package test

import (
	"yolean.se/ystack/yconverge/verify"
	"yolean.se/ystack/k3s/30-blobs:blobs"
	"yolean.se/ystack/k3s/40-kafka-ystack:kafka_ystack"
)

_dep_blobs: blobs.step
_dep_kafka: kafka_ystack.step

step: verify.#Step & {
	checks: []
}
`)
	deps, err := ParseImports(filepath.Join(dir, "yconverge.cue"))
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"k3s/30-blobs", "k3s/40-kafka-ystack"}
	if !equalStrings(deps, want) {
		t.Fatalf("got %v want %v", deps, want)
	}
}

func TestParseImports_SkipsVerifyImport(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "yconverge.cue"), `
package test

import "yolean.se/ystack/yconverge/verify"

step: verify.#Step & {
	checks: []
}
`)
	deps, err := ParseImports(filepath.Join(dir, "yconverge.cue"))
	if err != nil {
		t.Fatal(err)
	}
	if len(deps) != 0 {
		t.Fatalf("expected no deps, got %v", deps)
	}
}

func TestParseImports_NoImports(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "yconverge.cue"), `
package test
step: checks: []
`)
	deps, err := ParseImports(filepath.Join(dir, "yconverge.cue"))
	if err != nil {
		t.Fatal(err)
	}
	if len(deps) != 0 {
		t.Fatalf("expected no deps, got %v", deps)
	}
}

func TestParseImports_MissingFile(t *testing.T) {
	deps, err := ParseImports("/nonexistent/yconverge.cue")
	if err != nil {
		t.Fatal(err)
	}
	if deps != nil {
		t.Fatalf("expected nil, got %v", deps)
	}
}

func TestFindCueFiles(t *testing.T) {
	root := t.TempDir()
	writeFile(t, filepath.Join(root, "a/yconverge.cue"), "package a\n")
	writeFile(t, filepath.Join(root, "b/kustomization.yaml"), "")
	writeFile(t, filepath.Join(root, "c/yconverge.cue"), "package c\n")

	dirs := []string{
		filepath.Join(root, "a"),
		filepath.Join(root, "b"),
		filepath.Join(root, "c"),
	}
	found := FindCueFiles(dirs)
	if len(found) != 2 {
		t.Fatalf("expected 2, got %v", found)
	}
	if found[0] != filepath.Join(root, "a") || found[1] != filepath.Join(root, "c") {
		t.Fatalf("unexpected dirs: %v", found)
	}
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
