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

// setupCueModule creates a temp directory with a CUE module and the
// vendored verify schema, so ParseChecks can resolve imports.
func setupCueModule(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	writeFile(t, filepath.Join(root, "cue.mod/module.cue"), `module: "test.local"
language: version: "v0.16.0"
`)
	writeFile(t, filepath.Join(root, "cue.mod/pkg/yolean.se/ystack/yconverge/verify/schema.cue"), `package verify

#Step: {
	checks: [...#Check]
}

#Check: #Wait | #Rollout | #Exec

#Wait: {
	kind:        "wait"
	resource:    string
	for:         string
	namespace?:  string
	timeout:     *"60s" | string
	description: *"" | string
}

#Rollout: {
	kind:        "rollout"
	resource:    string
	namespace?:  string
	timeout:     *"60s" | string
	description: *"" | string
}

#Exec: {
	kind:        "exec"
	command:     string
	timeout:     *"60s" | string
	description: string
}
`)
	return root
}

func TestParseChecks_RolloutCheck(t *testing.T) {
	root := setupCueModule(t)
	writeFile(t, filepath.Join(root, "base/yconverge.cue"), `package base

import "yolean.se/ystack/yconverge/verify"

step: verify.#Step & {
	checks: [{
		kind:     "rollout"
		resource: "deployment/my-app"
		timeout:  "120s"
	}]
}
`)
	checks, err := ParseChecks(filepath.Join(root, "base"))
	if err != nil {
		t.Fatal(err)
	}
	if len(checks) != 1 {
		t.Fatalf("expected 1 check, got %d", len(checks))
	}
	if checks[0].Kind != "rollout" {
		t.Fatalf("expected rollout, got %s", checks[0].Kind)
	}
	if checks[0].Resource != "deployment/my-app" {
		t.Fatalf("expected deployment/my-app, got %s", checks[0].Resource)
	}
	if checks[0].Timeout != "120s" {
		t.Fatalf("expected 120s, got %s", checks[0].Timeout)
	}
}

func TestParseChecks_ExecCheck(t *testing.T) {
	root := setupCueModule(t)
	writeFile(t, filepath.Join(root, "base/yconverge.cue"), `package base

import "yolean.se/ystack/yconverge/verify"

step: verify.#Step & {
	checks: [{
		kind:        "exec"
		command:     "curl -sf http://$NAMESPACE.example.com/"
		timeout:     "60s"
		description: "site responds"
	}]
}
`)
	checks, err := ParseChecks(filepath.Join(root, "base"))
	if err != nil {
		t.Fatal(err)
	}
	if len(checks) != 1 {
		t.Fatalf("expected 1 check, got %d", len(checks))
	}
	if checks[0].Kind != "exec" {
		t.Fatalf("expected exec, got %s", checks[0].Kind)
	}
	if checks[0].Command != "curl -sf http://$NAMESPACE.example.com/" {
		t.Fatalf("unexpected command: %s", checks[0].Command)
	}
	if checks[0].Description != "site responds" {
		t.Fatalf("unexpected description: %s", checks[0].Description)
	}
}

func TestParseChecks_WaitCheck(t *testing.T) {
	root := setupCueModule(t)
	writeFile(t, filepath.Join(root, "base/yconverge.cue"), `package base

import "yolean.se/ystack/yconverge/verify"

step: verify.#Step & {
	checks: [{
		kind:        "wait"
		resource:    "ns/dev"
		for:         "jsonpath={.status.phase}=Active"
		timeout:     "30s"
		description: "namespace active"
	}]
}
`)
	checks, err := ParseChecks(filepath.Join(root, "base"))
	if err != nil {
		t.Fatal(err)
	}
	if len(checks) != 1 {
		t.Fatalf("expected 1 check, got %d", len(checks))
	}
	if checks[0].Kind != "wait" {
		t.Fatalf("expected wait, got %s", checks[0].Kind)
	}
	if checks[0].For != "jsonpath={.status.phase}=Active" {
		t.Fatalf("unexpected for: %s", checks[0].For)
	}
}

func TestParseChecks_MultipleChecks(t *testing.T) {
	root := setupCueModule(t)
	writeFile(t, filepath.Join(root, "base/yconverge.cue"), `package base

import "yolean.se/ystack/yconverge/verify"

step: verify.#Step & {
	checks: [
		{
			kind:     "rollout"
			resource: "deployment/app"
		},
		{
			kind:        "exec"
			command:     "true"
			description: "always passes"
		},
	]
}
`)
	checks, err := ParseChecks(filepath.Join(root, "base"))
	if err != nil {
		t.Fatal(err)
	}
	if len(checks) != 2 {
		t.Fatalf("expected 2 checks, got %d", len(checks))
	}
	if checks[0].Kind != "rollout" || checks[1].Kind != "exec" {
		t.Fatalf("unexpected kinds: %s, %s", checks[0].Kind, checks[1].Kind)
	}
}

func TestParseChecks_EmptyChecks(t *testing.T) {
	root := setupCueModule(t)
	writeFile(t, filepath.Join(root, "base/yconverge.cue"), `package base

import "yolean.se/ystack/yconverge/verify"

step: verify.#Step & {
	checks: []
}
`)
	checks, err := ParseChecks(filepath.Join(root, "base"))
	if err != nil {
		t.Fatal(err)
	}
	if len(checks) != 0 {
		t.Fatalf("expected 0 checks, got %d", len(checks))
	}
}

func TestParseChecks_DefaultTimeout(t *testing.T) {
	root := setupCueModule(t)
	writeFile(t, filepath.Join(root, "base/yconverge.cue"), `package base

import "yolean.se/ystack/yconverge/verify"

step: verify.#Step & {
	checks: [{
		kind:     "rollout"
		resource: "deployment/app"
	}]
}
`)
	checks, err := ParseChecks(filepath.Join(root, "base"))
	if err != nil {
		t.Fatal(err)
	}
	if checks[0].Timeout != "60s" {
		t.Fatalf("expected default 60s, got %s", checks[0].Timeout)
	}
}

func TestParseChecks_WithNamespace(t *testing.T) {
	root := setupCueModule(t)
	writeFile(t, filepath.Join(root, "base/yconverge.cue"), `package base

import "yolean.se/ystack/yconverge/verify"

step: verify.#Step & {
	checks: [{
		kind:      "rollout"
		resource:  "deployment/registry"
		namespace: "ystack"
		timeout:   "120s"
	}]
}
`)
	checks, err := ParseChecks(filepath.Join(root, "base"))
	if err != nil {
		t.Fatal(err)
	}
	if checks[0].Namespace != "ystack" {
		t.Fatalf("expected namespace ystack, got %s", checks[0].Namespace)
	}
}

func TestParseChecks_NoCueFile(t *testing.T) {
	root := setupCueModule(t)
	if err := os.MkdirAll(filepath.Join(root, "empty"), 0o755); err != nil {
		t.Fatal(err)
	}
	_, err := ParseChecks(filepath.Join(root, "empty"))
	if err == nil {
		t.Fatal("expected error for dir with no CUE files")
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
