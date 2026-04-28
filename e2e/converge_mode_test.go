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

	"github.com/Yolean/y-cluster/pkg/yconverge"
)

// writeKustomization drops a minimal kustomization tree at dir.
// files maps relative path -> content. Each call replaces the
// directory contents wholesale so a test can re-render a base
// between Run() invocations.
func writeKustomization(t *testing.T, dir string, files map[string]string) {
	t.Helper()
	if err := os.RemoveAll(dir); err != nil {
		t.Fatalf("clear %s: %v", dir, err)
	}
	for rel, body := range files {
		full := filepath.Join(dir, rel)
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatalf("mkdir %s: %v", full, err)
		}
		if err := os.WriteFile(full, []byte(body), 0o644); err != nil {
			t.Fatalf("write %s: %v", full, err)
		}
	}
}

// kubectlGet runs `kubectl --context=ctxName get <args...>` and
// returns trimmed stdout. Test helper so each assertion reads as
// "what does the cluster currently say".
func kubectlGet(t *testing.T, args ...string) string {
	t.Helper()
	full := append([]string{"--context=" + contextName, "get"}, args...)
	cmd := exec.Command("kubectl", full...)
	cmd.Env = append(os.Environ(), "KUBECONFIG="+clusterKubeconfig)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("kubectl get %v: %s: %v", args, out, err)
	}
	return strings.TrimSpace(string(out))
}

// runConverge runs yconverge.Run against a kustomization dir and
// fails the test on error. Wraps the boilerplate every
// converge-mode test would otherwise repeat.
func runConverge(t *testing.T, dir string) {
	t.Helper()
	_, err := yconverge.Run(context.Background(), yconverge.Options{
		Context:      contextName,
		KustomizeDir: dir,
		SkipChecks:   true, // these tests run their own assertions
	}, logger(t))
	if err != nil {
		t.Fatalf("yconverge.Run %s: %v", dir, err)
	}
}

// TestConvergeMode_CreateSkipsIfExists is the regression we
// motivated this branch with: yolean.se/converge-mode=create
// must hand the resource to `kubectl create --save-config`,
// which means a re-run with edited source data leaves the
// cluster value alone (skip-if-exists).
//
// Before this branch's kubectl-shellouts work, every label was
// silently coerced to server-side-apply force, and the second
// run would have updated the data.
func TestConvergeMode_CreateSkipsIfExists(t *testing.T) {
	setupCluster(t)
	name := "convergetest-cm-create"
	dir := filepath.Join(t.TempDir(), "k")

	// First render: foo=original.
	writeKustomization(t, dir, map[string]string{
		"kustomization.yaml": "resources:\n- cm.yaml\n",
		"cm.yaml": fmt.Sprintf(`apiVersion: v1
kind: ConfigMap
metadata:
  name: %s
  labels:
    yolean.se/converge-mode: create
data:
  foo: original
`, name),
	})
	t.Cleanup(func() {
		_ = exec.Command("kubectl", "--context="+contextName, "delete", "configmap", name, "--ignore-not-found").Run()
	})

	runConverge(t, dir)
	if got := kubectlGet(t, "configmap", name, "-o", "jsonpath={.data.foo}"); got != "original" {
		t.Fatalf("after first run: foo=%q want %q", got, "original")
	}

	// Second render: foo=changed. create-mode means create-skip-
	// if-exists, so the cluster value should NOT update.
	writeKustomization(t, dir, map[string]string{
		"kustomization.yaml": "resources:\n- cm.yaml\n",
		"cm.yaml": fmt.Sprintf(`apiVersion: v1
kind: ConfigMap
metadata:
  name: %s
  labels:
    yolean.se/converge-mode: create
data:
  foo: changed
`, name),
	})

	runConverge(t, dir)
	if got := kubectlGet(t, "configmap", name, "-o", "jsonpath={.data.foo}"); got != "original" {
		t.Fatalf("create-mode must skip-if-exists; foo=%q want %q (would have flipped to 'changed' under apply semantics)", got, "original")
	}
}

// TestConvergeMode_ReplaceRecreates: yolean.se/converge-mode=replace
// runs `kubectl delete` followed by the plain-apply step, so
// the resource's UID changes between runs even when the spec
// hasn't changed. This is what makes the mode usable for Jobs
// (whose spec.template is immutable on a same-name re-apply).
func TestConvergeMode_ReplaceRecreates(t *testing.T) {
	setupCluster(t)
	name := "convergetest-cm-replace"
	dir := filepath.Join(t.TempDir(), "k")

	cm := fmt.Sprintf(`apiVersion: v1
kind: ConfigMap
metadata:
  name: %s
  labels:
    yolean.se/converge-mode: replace
data:
  marker: same
`, name)
	writeKustomization(t, dir, map[string]string{
		"kustomization.yaml": "resources:\n- cm.yaml\n",
		"cm.yaml":            cm,
	})
	t.Cleanup(func() {
		_ = exec.Command("kubectl", "--context="+contextName, "delete", "configmap", name, "--ignore-not-found").Run()
	})

	runConverge(t, dir)
	uid1 := kubectlGet(t, "configmap", name, "-o", "jsonpath={.metadata.uid}")
	if uid1 == "" {
		t.Fatal("first run did not create the ConfigMap")
	}

	runConverge(t, dir)
	uid2 := kubectlGet(t, "configmap", name, "-o", "jsonpath={.metadata.uid}")
	if uid2 == "" {
		t.Fatal("second run lost the ConfigMap")
	}
	if uid1 == uid2 {
		t.Fatalf("replace-mode should recreate the resource (UID change); both runs returned %q", uid1)
	}
}

// TestConvergeMode_MixedKustomization is the smoke test for the
// five-step plan running all five steps cleanly when a single
// kustomize tree carries multiple modes plus an unlabelled
// resource. Asserts each resource lands; the per-mode behaviour
// is covered by the dedicated tests above.
//
// Importantly this also exercises the "no objects passed to
// <verb>" stderr tolerance: we only label a subset of modes,
// and the un-used mode steps must not error the run out.
func TestConvergeMode_MixedKustomization(t *testing.T) {
	setupCluster(t)
	dir := filepath.Join(t.TempDir(), "k")

	body := `apiVersion: v1
kind: ConfigMap
metadata:
  name: convergetest-mixed-create
  labels:
    yolean.se/converge-mode: create
data:
  who: create-mode
---
apiVersion: v1
kind: ConfigMap
metadata:
  name: convergetest-mixed-ssforce
  labels:
    yolean.se/converge-mode: serverside-force
data:
  who: ssforce-mode
---
apiVersion: v1
kind: ConfigMap
metadata:
  name: convergetest-mixed-ss
  labels:
    yolean.se/converge-mode: serverside
data:
  who: ss-mode
---
apiVersion: v1
kind: ConfigMap
metadata:
  name: convergetest-mixed-plain
data:
  who: plain-mode
`
	writeKustomization(t, dir, map[string]string{
		"kustomization.yaml": "resources:\n- cms.yaml\n",
		"cms.yaml":           body,
	})
	t.Cleanup(func() {
		for _, n := range []string{
			"convergetest-mixed-create",
			"convergetest-mixed-ssforce",
			"convergetest-mixed-ss",
			"convergetest-mixed-plain",
		} {
			_ = exec.Command("kubectl", "--context="+contextName, "delete", "configmap", n, "--ignore-not-found").Run()
		}
	})

	runConverge(t, dir)

	for name, wantValue := range map[string]string{
		"convergetest-mixed-create":  "create-mode",
		"convergetest-mixed-ssforce": "ssforce-mode",
		"convergetest-mixed-ss":      "ss-mode",
		"convergetest-mixed-plain":   "plain-mode",
	} {
		got := kubectlGet(t, "configmap", name, "-o", "jsonpath={.data.who}")
		if got != wantValue {
			t.Errorf("%s: data.who=%q want %q", name, got, wantValue)
		}
	}
}
