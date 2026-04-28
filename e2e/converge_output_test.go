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

// TestConvergeMode_OutputForwardsKubectlLines is the third leg of
// the converge-mode coverage plan: prove that kubectl's per-
// resource output reaches the user verbatim. The whole point of
// switching from client-go SSA to kubectl shellouts was so a
// developer running `y-cluster yconverge` (or `kubectl yconverge`)
// sees the same line shapes they'd see running kubectl directly --
// the structured zap "applied" lines the previous client-go path
// emitted are not the same UX.
//
// We run the actual binary as a subprocess and capture its
// stdout (you can't capture os.Stdout from inside the test binary
// because pkg/yconverge writes there directly). Three assertions:
//
//   1. The plain-apply step (no label) produces
//      "configmap/<name> created" -- the verb kubectl client-side
//      apply emits on first run.
//   2. The serverside-force step's resource produces
//      "configmap/<name> serverside-applied" -- distinct verb
//      because that step uses --server-side.
//   3. "No resources found" must NOT appear: kubectl delete
//      prints that to stdout when the replace-mode selector
//      matches nothing (a fresh cluster, no replace label in the
//      fixture), and the wrapper's stdout-suppress list catches
//      it so a default yconverge run doesn't surface that noise.
//
// If any assertion fails, the wrapper has regressed: either the
// per-resource lines are being captured-and-dropped, or the
// suppression for empty-selector-match lines has loosened.
func TestConvergeMode_OutputForwardsKubectlLines(t *testing.T) {
	setupCluster(t)
	bin := buildServeBinary(t)

	dir := filepath.Join(t.TempDir(), "k")
	plainName := "convergeout-plain"
	forceName := "convergeout-ssforce"
	body := fmt.Sprintf(`apiVersion: v1
kind: ConfigMap
metadata:
  name: %s
data:
  x: "1"
---
apiVersion: v1
kind: ConfigMap
metadata:
  name: %s
  labels:
    yolean.se/converge-mode: serverside-force
data:
  x: "1"
`, plainName, forceName)
	writeKustomization(t, dir, map[string]string{
		"kustomization.yaml": "resources:\n- cms.yaml\n",
		"cms.yaml":           body,
	})
	t.Cleanup(func() {
		for _, n := range []string{plainName, forceName} {
			_ = exec.Command("kubectl", "--context="+contextName, "delete", "configmap", n, "--ignore-not-found").Run()
		}
	})

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, bin,
		"yconverge", "--context="+contextName, "-k", dir, "--skip-checks")
	cmd.Env = append(os.Environ(), "KUBECONFIG="+clusterKubeconfig)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("y-cluster yconverge: %v\noutput:\n%s", err, out)
	}
	got := string(out)

	// Plain-apply step (kubectl apply, no --server-side, on the
	// negative-selector fall-through) prints "created" on first
	// run. This is the normal kubectl client-side-apply verb.
	plainLine := fmt.Sprintf("configmap/%s created", plainName)
	if !strings.Contains(got, plainLine) {
		t.Errorf("output missing %q\nfull output:\n%s", plainLine, got)
	}

	// serverside-force step uses kubectl apply --server-side
	// --force-conflicts; verb is "serverside-applied".
	forceLine := fmt.Sprintf("configmap/%s serverside-applied", forceName)
	if !strings.Contains(got, forceLine) {
		t.Errorf("output missing %q\nfull output:\n%s", forceLine, got)
	}

	// "No resources found" is what kubectl delete prints (to stdout,
	// exit 0) when the replace-mode selector matches nothing. The
	// wrapper's stdout-suppress list is supposed to drop it so a
	// vanilla yconverge run isn't noisy.
	if strings.Contains(got, "No resources found") {
		t.Errorf("output should not surface kubectl delete's empty-match line\nfull output:\n%s", got)
	}
}
