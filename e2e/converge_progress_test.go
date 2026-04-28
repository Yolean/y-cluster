//go:build e2e

package e2e

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestConvergeProgress_DepAndTargetHeaders pins the four progress
// header shapes against a real run:
//
//   - yconverge dependency <relpath>   -- before each dep
//   - yconverge converge-mode=<mode>   -- before non-empty mode buckets
//   - yconverge target <relpath>       -- before the final step (only when deps ran)
//   - yconverge check N/total <kind>   -- before each check
//
// Builds a CUE module on disk with a base/ that carries
// converge-mode=replace + a dependent that depends on it and
// declares one exec check. Runs the binary, captures stdout,
// asserts the headers appear in the expected order.
func TestConvergeProgress_DepAndTargetHeaders(t *testing.T) {
	setupCluster(t)
	bin := buildServeBinary(t)

	root := t.TempDir()
	baseName := "convergeprogress-base"
	depName := "convergeprogress-dependent"
	writeKustomization(t, root, map[string]string{
		"cue.mod/module.cue": `module: "yolean.se/test"
language: { version: "v0.16.0" }
`,
		// Verify schema lives at the canonical ystack path; see
		// pkg/yconverge/cue.go's verifySchemaImport.
		"cue.mod/pkg/yolean.se/ystack/yconverge/verify/schema.cue": `package verify
#Step: { checks: [...{...}] }
`,
		"base/kustomization.yaml": "resources:\n- cm.yaml\n",
		"base/cm.yaml": fmt.Sprintf(`apiVersion: v1
kind: ConfigMap
metadata:
  name: %s
  labels:
    yolean.se/converge-mode: replace
data:
  marker: same
`, baseName),
		"base/yconverge.cue": `package base
import "yolean.se/ystack/yconverge/verify"
step: verify.#Step & { checks: [] }
`,
		"dependent/kustomization.yaml": "resources:\n- cm.yaml\n",
		"dependent/cm.yaml": fmt.Sprintf(`apiVersion: v1
kind: ConfigMap
metadata:
  name: %s
data:
  who: dependent
`, depName),
		"dependent/yconverge.cue": `package dependent
import (
  "yolean.se/ystack/yconverge/verify"
  "yolean.se/test/base:base"
)
_dep: base.step
step: verify.#Step & { checks: [{
  kind: "exec"
  command: "true"
  description: "noop check that always succeeds"
  timeout: "5s"
}] }
`,
	})
	t.Cleanup(func() {
		for _, n := range []string{baseName, depName} {
			_ = exec.Command("kubectl", "--context="+contextName, "delete", "configmap", n, "--ignore-not-found").Run()
		}
	})

	depDir := filepath.Join(root, "dependent")
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, bin,
		"yconverge", "--context="+contextName, "-k", depDir)
	cmd.Env = append(os.Environ(), "KUBECONFIG="+clusterKubeconfig)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("y-cluster yconverge: %v\noutput:\n%s", err, out)
	}
	got := string(out)

	wants := []string{
		"yconverge dependency base",
		"yconverge converge-mode=replace",
		"yconverge target dependent",
		"yconverge check 1/1 exec",
	}
	for _, w := range wants {
		if !strings.Contains(got, w) {
			t.Errorf("missing progress line %q\nfull output:\n%s", w, got)
		}
	}

	// Order matters: dependency before target, target before check.
	depIdx := strings.Index(got, "yconverge dependency base")
	tgtIdx := strings.Index(got, "yconverge target dependent")
	chkIdx := strings.Index(got, "yconverge check 1/1 exec")
	if !(depIdx < tgtIdx && tgtIdx < chkIdx) {
		t.Errorf("progress lines out of order: dep=%d target=%d check=%d\nfull output:\n%s",
			depIdx, tgtIdx, chkIdx, got)
	}
}

// TestConvergeProgress_NoDepsNoTargetHeader: a run without
// dependencies must NOT print "yconverge target" -- the user
// already knows what they passed via -k. Only the dep+target
// case earns the symmetric header.
func TestConvergeProgress_NoDepsNoTargetHeader(t *testing.T) {
	setupCluster(t)
	bin := buildServeBinary(t)

	dir := filepath.Join(t.TempDir(), "k")
	name := "convergeprogress-nodeps"
	writeKustomization(t, dir, map[string]string{
		"kustomization.yaml": "resources:\n- cm.yaml\n",
		"cm.yaml": fmt.Sprintf(`apiVersion: v1
kind: ConfigMap
metadata:
  name: %s
data:
  x: "1"
`, name),
	})
	t.Cleanup(func() {
		_ = exec.Command("kubectl", "--context="+contextName, "delete", "configmap", name, "--ignore-not-found").Run()
	})

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, bin,
		"yconverge", "--context="+contextName, "-k", dir, "--skip-checks")
	cmd.Env = append(os.Environ(), "KUBECONFIG="+clusterKubeconfig)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("y-cluster yconverge: %v\noutput:\n%s", err, out)
	}
	got := string(out)

	if strings.Contains(got, "yconverge dependency") {
		t.Errorf("no-deps run must not print 'yconverge dependency':\n%s", got)
	}
	if strings.Contains(got, "yconverge target") {
		t.Errorf("no-deps run must not print 'yconverge target':\n%s", got)
	}
	// kubectl's per-resource line should still be there.
	if !strings.Contains(got, fmt.Sprintf("configmap/%s created", name)) {
		t.Errorf("kubectl per-resource line missing:\n%s", got)
	}
}

// TestConvergeProgress_EmptyModeNoHeader: a kustomization that
// uses no labelled modes (all unlabelled, plain apply) must NOT
// print any "yconverge converge-mode=" line. The header is
// suppressed alongside the empty-bucket kubectl output.
func TestConvergeProgress_EmptyModeNoHeader(t *testing.T) {
	setupCluster(t)
	bin := buildServeBinary(t)

	dir := filepath.Join(t.TempDir(), "k")
	name := "convergeprogress-empty"
	writeKustomization(t, dir, map[string]string{
		"kustomization.yaml": "resources:\n- cm.yaml\n",
		"cm.yaml": fmt.Sprintf(`apiVersion: v1
kind: ConfigMap
metadata:
  name: %s
data:
  x: "1"
`, name),
	})
	t.Cleanup(func() {
		_ = exec.Command("kubectl", "--context="+contextName, "delete", "configmap", name, "--ignore-not-found").Run()
	})

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, bin,
		"yconverge", "--context="+contextName, "-k", dir, "--skip-checks")
	cmd.Env = append(os.Environ(), "KUBECONFIG="+clusterKubeconfig)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("y-cluster yconverge: %v\noutput:\n%s", err, out)
	}
	got := string(out)

	if strings.Contains(got, "yconverge converge-mode=") {
		t.Errorf("unlabelled run must not print any 'yconverge converge-mode=' header:\n%s", got)
	}
}
