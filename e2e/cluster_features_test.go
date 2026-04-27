//go:build e2e

// e2e coverage for y-cluster's cluster-discovery subcommands
// (detect / ctr / crictl). These need a real cluster with a
// reachable containerd, so the helpers in this file are called
// from each provisioner-specific e2e test (docker_test.go,
// qemu_test.go) — once per backend, against the cluster that
// test just provisioned.
//
// kwok-backed tests don't exercise this surface: kwok is a fake
// apiserver with no node, so detect would fail by design. The
// command is meant for the real-cluster path where ystack used
// to shell out via y-cluster-local-{detect,ctr,crictl}.
package e2e

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Yolean/y-cluster/e2e/cluster"
)

// assertClusterFeatures runs detect / ctr / crictl against an
// already-provisioned cluster reachable via `--context=ctxName`.
// expectedBackend is "docker" or "qemu" depending on which
// provisioner the calling test brought up. Fails the test on any
// unexpected output or non-zero exit.
//
// The binary is taken from buildServeBinary (which builds the
// whole y-cluster binary, not just serve) so we don't pay the
// cost of compiling twice in the same `go test` invocation.
func assertClusterFeatures(t *testing.T, ctxName, expectedBackend string) {
	t.Helper()
	bin := buildServeBinary(t)

	// 1. `y-cluster detect` prints the backend name.
	out := runYCluster(t, bin, "detect", "--context="+ctxName)
	if got := strings.TrimSpace(out); got != expectedBackend {
		t.Fatalf("detect: got %q want %q", got, expectedBackend)
	}

	// 2. `y-cluster detect <backend>` matches → "up".
	out = runYCluster(t, bin, "detect", "--context="+ctxName, expectedBackend)
	if got := strings.TrimSpace(out); got != "up" {
		t.Fatalf("detect %s: got %q want up", expectedBackend, got)
	}

	// 3. `y-cluster detect <wrong-backend>` fails non-zero.
	other := "qemu"
	if expectedBackend == "qemu" {
		other = "docker"
	}
	if _, err := runYClusterRaw(t, bin, "detect", "--context="+ctxName, other); err == nil {
		t.Fatalf("detect %s should have errored on a %s cluster", other, expectedBackend)
	}

	// 4. `ctr version` works through the routed transport.
	out = runYCluster(t, bin, "ctr", "--context="+ctxName, "--", "version")
	if !strings.Contains(out, "Client:") {
		t.Fatalf("ctr version: missing Client: header in %q", out)
	}

	// 5. `crictl version` returns runtime info. crictl writes its
	// runtime version line in different shapes across versions
	// (RuntimeName / RuntimeApiVersion etc.), so we only assert
	// the substring "Version".
	out = runYCluster(t, bin, "crictl", "--context="+ctxName, "--", "version")
	if !strings.Contains(out, "Version") {
		t.Fatalf("crictl version: missing Version line in %q", out)
	}

	// 6. `images load <archive>` imports a local OCI archive and
	// the loaded ref shows up under `ctr image ls -n k8s.io`.
	// Synthetic archive — no registry needed for this leg.
	archive := filepath.Join(t.TempDir(), "fixture.tar")
	cluster.SaveFixtureArchive(t, archive, "y-cluster.local/e2e-load", "v1")
	if loadOut, err := runYClusterRaw(t, bin, "images", "load", "--context="+ctxName, archive); err != nil {
		t.Fatalf("images load %s: %v\n%s", archive, err, loadOut)
	}
	out = runYCluster(t, bin, "ctr", "--context="+ctxName, "--", "-n", "k8s.io", "image", "ls", "-q")
	if !strings.Contains(out, "y-cluster.local/e2e-load:v1") {
		t.Fatalf("loaded image not in `ctr image ls`:\n%s", out)
	}

	// 7. CI5: airgap proof. Deploy a Pod referencing the loaded
	// image with imagePullPolicy: Never — kubelet won't reach for
	// any registry, and emits state.waiting.reason=ErrImageNeverPull
	// when the image isn't on the node. Pull resolution succeeding
	// (state advancing to running or terminated) proves load made
	// the bytes available to kubelet.
	assertAirgapPod(t, ctxName)
}

// assertAirgapPod is CI5: it applies a tiny Pod referencing the
// synthetic image assertClusterFeatures just loaded
// (y-cluster.local/e2e-load:v1), with imagePullPolicy: Never, and
// asserts kubelet resolves the image. The synthetic image has no
// runnable entrypoint, so we don't wait for Ready — once
// container state leaves Pending/Waiting, the pull worked.
//
// Failure modes the assertion distinguishes:
//   - state.waiting.reason=ErrImageNeverPull → load didn't reach
//     the node's containerd (the actual airgap regression we
//     care about catching).
//   - timeout with state still ContainerCreating/Pending → kubelet
//     hasn't observed the pod; usually a node-readiness issue.
func assertAirgapPod(t *testing.T, ctxName string) {
	t.Helper()
	podName := fmt.Sprintf("airgap-%d", time.Now().UnixNano())
	manifest := fmt.Sprintf(`apiVersion: v1
kind: Pod
metadata:
  name: %s
spec:
  restartPolicy: Never
  containers:
  - name: c
    image: y-cluster.local/e2e-load:v1
    imagePullPolicy: Never
    command: ["/airgap-marker"]
`, podName)
	manifestPath := filepath.Join(t.TempDir(), "airgap.yaml")
	if err := os.WriteFile(manifestPath, []byte(manifest), 0o644); err != nil {
		t.Fatal(err)
	}
	apply := exec.Command("kubectl", "--context="+ctxName, "apply", "-f", manifestPath)
	if out, err := apply.CombinedOutput(); err != nil {
		t.Fatalf("kubectl apply airgap pod: %s: %v", out, err)
	}
	t.Cleanup(func() {
		_ = exec.Command("kubectl", "--context="+ctxName, "delete", "pod", podName,
			"--ignore-not-found=true", "--force", "--grace-period=0").Run()
	})

	deadline := time.Now().Add(60 * time.Second)
	last := ""
	for time.Now().Before(deadline) {
		out, err := exec.Command("kubectl", "--context="+ctxName, "get", "pod", podName,
			"-o", `jsonpath={.status.containerStatuses[0].state.waiting.reason}|{.status.containerStatuses[0].state.running.startedAt}|{.status.containerStatuses[0].state.terminated.reason}`).Output()
		if err == nil {
			s := strings.TrimSpace(string(out))
			last = s
			parts := strings.SplitN(s, "|", 3)
			waitingReason := parts[0]
			running := parts[1]
			terminated := parts[2]
			if waitingReason == "ErrImageNeverPull" {
				t.Fatalf("airgap proof FAILED: kubelet says %q for %s — `images load` didn't reach the node",
					waitingReason, podName)
			}
			if running != "" || terminated != "" {
				return // pull resolved; image is in the node
			}
		}
		time.Sleep(500 * time.Millisecond)
	}
	t.Fatalf("airgap pod %s never progressed past pull resolution within 60s (last: %q)", podName, last)
}

// runYCluster runs the binary with a 60s ctx, fails the test on
// non-zero exit, and returns combined stdout+stderr. Use
// runYClusterRaw when the test specifically wants the error.
func runYCluster(t *testing.T, bin string, args ...string) string {
	t.Helper()
	out, err := runYClusterRaw(t, bin, args...)
	if err != nil {
		t.Fatalf("y-cluster %v: %v\n%s", args, err, out)
	}
	return out
}

func runYClusterRaw(t *testing.T, bin string, args ...string) (string, error) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, bin, args...)
	var buf bytes.Buffer
	cmd.Stdout = &buf
	cmd.Stderr = &buf
	err := cmd.Run()
	return buf.String(), err
}
