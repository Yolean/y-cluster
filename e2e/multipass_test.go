//go:build e2e && multipass

// multipass e2e. Provisions a real Ubuntu VM via multipass, installs
// k3s, asserts that the merged kubeconfig works, that envoy-gateway
// rolls out for real, and that pod-to-apiserver routing works (the
// regression guard against the --node-external-ip class of bug).
package e2e

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	"go.uber.org/zap"

	"github.com/Yolean/y-cluster/pkg/provision/config"
	"github.com/Yolean/y-cluster/pkg/provision/multipass"
)

// e2eMultipassConfig builds a defaults-applied MultipassConfig.
// CPUs / Memory / DiskSize stay at the defaults: e2e on a CI runner
// or a developer laptop should look like a real provision, not a
// scaled-down toy.
func e2eMultipassConfig(name, ctxName string) *config.MultipassConfig {
	c := &config.MultipassConfig{
		CommonConfig: config.CommonConfig{
			Provider: config.ProviderMultipass,
			Name:     name,
			Context:  ctxName,
		},
	}
	c.ApplyDefaults()
	return c
}

func skipIfNoMultipass(t *testing.T) {
	t.Helper()
	if err := multipass.CheckPrerequisites(); err != nil {
		t.Skipf("multipass tests require a working multipass daemon: %v", err)
	}
}

// purgeMultipassVM is the idempotent recovery path for leftover state
// from a previous failed run (or a test that t.Skip'd before
// Cleanup ran). multipass delete + purge are no-ops on a missing VM
// modulo the non-zero exit, so we ignore errors.
func purgeMultipassVM(name string) {
	_ = exec.Command("multipass", "stop", name).Run()
	_ = exec.Command("multipass", "delete", name).Run()
	_ = exec.Command("multipass", "purge").Run()
}

func TestMultipass_ProvisionTeardown(t *testing.T) {
	skipIfNoMultipass(t)

	kcfgPath := os.Getenv("KUBECONFIG")
	if kcfgPath == "" {
		t.Skip("KUBECONFIG must be set")
	}

	logger, _ := zap.NewDevelopment()
	cfg := e2eMultipassConfig("y-cluster-e2e-mp", "y-cluster-e2e-mp")

	purgeMultipassVM(cfg.Name)
	t.Cleanup(func() { purgeMultipassVM(cfg.Name) })

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Minute)
	defer cancel()

	rt := multipass.FromConfig(cfg)
	cluster, err := multipass.Provision(ctx, rt, logger)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cluster.Teardown(false) })

	if got := cluster.VMIP(); got == "" {
		t.Fatal("VMIP empty after Provision")
	}

	// Sanity: NodeExec works.
	out, err := cluster.NodeExec(ctx, "k3s --version | head -1", nil)
	if err != nil {
		t.Fatalf("NodeExec: %s: %v", out, err)
	}
	if !strings.Contains(string(out), "k3s") {
		t.Fatalf("k3s --version: %q", out)
	}

	// kubectl through the merged kubeconfig sees a Ready node.
	deadline := time.Now().Add(2 * time.Minute)
	for {
		kc := exec.CommandContext(ctx, "kubectl", "--context="+cluster.Context(),
			"get", "nodes", "--no-headers")
		kc.Env = append(os.Environ(), "KUBECONFIG="+kcfgPath)
		kout, err := kc.CombinedOutput()
		if err == nil && strings.Contains(string(kout), "Ready") {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("node never became Ready: %s", kout)
		}
		time.Sleep(2 * time.Second)
	}

	// Envoy Gateway rolled out for real. Provision waits on the
	// rollout itself, but explicitly confirming the deployment is
	// Available proves we didn't accidentally skip the wait via a
	// negative ReadyTimeout the way kwok-based tests do.
	out, err = exec.CommandContext(ctx, "kubectl",
		"--context="+cluster.Context(), "-n", "envoy-gateway-system",
		"get", "deployment", "envoy-gateway",
		"-o", "jsonpath={.status.availableReplicas}").CombinedOutput()
	if err != nil {
		t.Fatalf("get envoy-gateway deployment: %s: %v", out, err)
	}
	if got := strings.TrimSpace(string(out)); got != "1" {
		t.Fatalf("envoy-gateway availableReplicas: %q (want 1)", got)
	}

	// detect / ctr / crictl / images load against the running VM
	// via the merged kubeconfig context.
	assertClusterFeatures(t, cluster.Context(), "multipass")

	// CRITICAL: pod-to-apiserver smoke test. This is the regression
	// guard for the --node-external-ip=127.0.0.1 class of bug
	// (commit 34332d9, reverted): a misconfigured external IP
	// breaks pod-to-apiserver routing while node Ready and host-
	// side kubectl both stay green. Run a curl pod against
	// kubernetes.default/healthz and require body == "ok".
	assertPodToAPIServer(t, ctx, cluster.Context())

	if err := cluster.Teardown(false); err != nil {
		t.Fatal(err)
	}
	// After teardown with keepDisk=false the VM should be gone.
	if out, err := exec.Command("multipass", "info", cfg.Name).CombinedOutput(); err == nil {
		t.Fatalf("multipass VM %q still exists after teardown: %s", cfg.Name, out)
	}
}

// assertPodToAPIServer is the regression guard: from inside the
// cluster, an in-pod curl to https://kubernetes.default/healthz must
// return "ok". Failures here typically mean kube-proxy / CNI / node
// IP wiring is broken, even when the host's kubectl works.
func assertPodToAPIServer(t *testing.T, ctx context.Context, ctxName string) {
	t.Helper()
	podName := fmt.Sprintf("apiserver-curl-%d", time.Now().UnixNano())
	manifest := fmt.Sprintf(`apiVersion: v1
kind: Pod
metadata:
  name: %s
spec:
  restartPolicy: Never
  containers:
  - name: c
    image: curlimages/curl:8.10.1
    command: ["sh", "-c"]
    args:
    - |
      curl -ksS --max-time 30 \
        -H "Authorization: Bearer $(cat /var/run/secrets/kubernetes.io/serviceaccount/token)" \
        https://kubernetes.default/healthz
`, podName)
	manifestPath := t.TempDir() + "/apiserver-curl.yaml"
	if err := os.WriteFile(manifestPath, []byte(manifest), 0o644); err != nil {
		t.Fatal(err)
	}
	apply := exec.CommandContext(ctx, "kubectl", "--context="+ctxName, "apply", "-f", manifestPath)
	if out, err := apply.CombinedOutput(); err != nil {
		t.Fatalf("kubectl apply curl pod: %s: %v", out, err)
	}
	t.Cleanup(func() {
		_ = exec.Command("kubectl", "--context="+ctxName, "delete", "pod", podName,
			"--ignore-not-found=true", "--force", "--grace-period=0").Run()
	})

	// Wait for the pod to terminate (success or fail).
	deadline := time.Now().Add(2 * time.Minute)
	for time.Now().Before(deadline) {
		phase, err := exec.Command("kubectl", "--context="+ctxName,
			"get", "pod", podName, "-o", "jsonpath={.status.phase}").Output()
		if err == nil {
			switch strings.TrimSpace(string(phase)) {
			case "Succeeded":
				goto done
			case "Failed":
				logs, _ := exec.Command("kubectl", "--context="+ctxName,
					"logs", podName).CombinedOutput()
				t.Fatalf("apiserver-curl pod failed; logs:\n%s", logs)
			}
		}
		time.Sleep(2 * time.Second)
	}
	t.Fatalf("apiserver-curl pod never reached Succeeded/Failed within 2 minutes")

done:
	logs, err := exec.Command("kubectl", "--context="+ctxName, "logs", podName).CombinedOutput()
	if err != nil {
		t.Fatalf("kubectl logs: %s: %v", logs, err)
	}
	body := strings.TrimSpace(string(logs))
	if body != "ok" {
		t.Fatalf("pod-to-apiserver curl body: %q (want \"ok\")", body)
	}
}

func TestMultipass_TeardownKeepDisk(t *testing.T) {
	skipIfNoMultipass(t)

	if os.Getenv("KUBECONFIG") == "" {
		t.Skip("KUBECONFIG must be set")
	}

	logger, _ := zap.NewDevelopment()
	cfg := e2eMultipassConfig("y-cluster-e2e-mp-keep", "y-cluster-e2e-mp-keep")

	purgeMultipassVM(cfg.Name)
	t.Cleanup(func() { purgeMultipassVM(cfg.Name) })

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Minute)
	defer cancel()

	rt := multipass.FromConfig(cfg)
	cluster, err := multipass.Provision(ctx, rt, logger)
	if err != nil {
		t.Fatal(err)
	}

	if err := cluster.Teardown(true); err != nil {
		t.Fatal(err)
	}
	// keepDisk=true means stopped, not deleted: `multipass info` still works.
	out, err := exec.Command("multipass", "info", cfg.Name).CombinedOutput()
	if err != nil {
		t.Fatalf("VM should still exist with keepDisk=true: %s: %v", out, err)
	}
	if !strings.Contains(string(out), "Stopped") {
		t.Fatalf("VM should be Stopped after keepDisk teardown, got:\n%s", out)
	}
}
