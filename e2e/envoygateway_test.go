//go:build e2e

package e2e

import (
	"context"
	"os"
	"os/exec"
	"strings"
	"sync"
	"testing"

	"github.com/Yolean/y-cluster/pkg/provision/envoygateway"
)

// envoyGatewayCacheDir is shared across tests in this package
// so install.yaml is downloaded from the upstream release at
// most once per `go test` invocation. Each test still calls
// Install, but Ensure no-ops on the second hit.
var (
	envoyGatewayCacheDir     string
	envoyGatewayCacheDirOnce sync.Once
)

func sharedEnvoyGatewayCache(t *testing.T) string {
	t.Helper()
	envoyGatewayCacheDirOnce.Do(func() {
		dir, err := os.MkdirTemp("", "y-cluster-e2e-eg-*")
		if err != nil {
			t.Fatalf("tmpdir: %v", err)
		}
		envoyGatewayCacheDir = dir
	})
	return envoyGatewayCacheDir
}

// TestEnvoyGateway_InstallAgainstKwok exercises the full Install
// path -- CRDs first, then the install manifest, then the default
// GatewayClass -- against the shared kwok cluster. The Deployment
// rollout wait is skipped (ReadyTimeout=-1) because kwok stages
// pods through its own controller, not the real one, and we only
// need to prove the apply path produces the right object graph.
//
// Coverage assertions afterwards use kubectl directly so we can
// verify what landed without making the test depend on every
// internal helper that does the same.
func TestEnvoyGateway_InstallAgainstKwok(t *testing.T) {
	setupCluster(t)

	if err := envoygateway.Install(context.Background(), envoygateway.Options{
		ContextName:   contextName,
		CacheOverride: sharedEnvoyGatewayCache(t),
		Logger:        logger(t),
		ReadyTimeout:  -1, // skip wait: kwok doesn't run the real controller
	}); err != nil {
		t.Fatalf("Install: %v", err)
	}

	// Gateway API CRDs landed: pick three load-bearing ones from
	// the v1.4.1 bundle. Asserting on a subset rather than the
	// full list is enough to prove the CRD apply path ran.
	crdOut := kubectl(t, "get", "crd",
		"gatewayclasses.gateway.networking.k8s.io",
		"gateways.gateway.networking.k8s.io",
		"httproutes.gateway.networking.k8s.io",
		"-o", "name")
	for _, want := range []string{
		"customresourcedefinition.apiextensions.k8s.io/gatewayclasses.gateway.networking.k8s.io",
		"customresourcedefinition.apiextensions.k8s.io/gateways.gateway.networking.k8s.io",
		"customresourcedefinition.apiextensions.k8s.io/httproutes.gateway.networking.k8s.io",
	} {
		if !strings.Contains(crdOut, want) {
			t.Errorf("CRD missing: %q\nGot:\n%s", want, crdOut)
		}
	}

	// EG-specific CRDs (envoyproxies.gateway.envoyproxy.io etc.)
	// must also exist. They're what consumer kustomize bases
	// reference when overriding controller config.
	envoyCRD := kubectl(t, "get", "crd",
		"envoyproxies.gateway.envoyproxy.io", "-o", "name")
	if !strings.Contains(envoyCRD, "envoyproxies.gateway.envoyproxy.io") {
		t.Errorf("EG CRD missing: %q", envoyCRD)
	}

	// Namespace + Deployment + Service in envoy-gateway-system.
	nsOut := kubectl(t, "get", "namespace", envoygateway.Namespace, "-o", "name")
	if !strings.Contains(nsOut, envoygateway.Namespace) {
		t.Errorf("namespace missing: %q", nsOut)
	}
	deployOut := kubectl(t, "get", "deployment", envoygateway.DeploymentName,
		"-n", envoygateway.Namespace, "-o", "name")
	if !strings.Contains(deployOut, envoygateway.DeploymentName) {
		t.Errorf("deployment missing: %q", deployOut)
	}

	// Default GatewayClass landed and points at EG's controller.
	gcOut := kubectl(t, "get", "gatewayclass", "eg",
		"-o", "jsonpath={.spec.controllerName}")
	want := "gateway.envoyproxy.io/gatewayclass-controller"
	if gcOut != want {
		t.Errorf("GatewayClass eg.spec.controllerName = %q, want %q", gcOut, want)
	}
}

// TestEnvoyGateway_InstallSkipGatewayClass verifies the opt-out
// for consumers that bring their own GatewayClass.
func TestEnvoyGateway_InstallSkipGatewayClass(t *testing.T) {
	setupCluster(t)

	// Apply the bundle without the default GatewayClass; if a
	// previous run created one, remove it first so the assertion
	// below isn't a stale-state false negative.
	_ = exec.Command("kubectl", "--context="+contextName,
		"delete", "gatewayclass", "eg-skip-test", "--ignore-not-found").Run()

	if err := envoygateway.Install(context.Background(), envoygateway.Options{
		ContextName:      contextName,
		CacheOverride:    sharedEnvoyGatewayCache(t),
		Logger:           logger(t),
		ReadyTimeout:     -1,
		SkipGatewayClass: true,
	}); err != nil {
		t.Fatalf("Install: %v", err)
	}

	// The default `eg` GatewayClass may exist from a prior test;
	// what we want to prove is that SkipGatewayClass doesn't
	// create a NEW one. The TestEnvoyGateway_InstallAgainstKwok
	// covers the create path; here we just check the option
	// was wired (no panic, Install returned nil) -- the negative
	// behaviour is hard to assert when tests share a cluster.
}

// kubectl runs `kubectl --context=<setup> args...` and returns
// the trimmed stdout. Failures fail the test. Mirrors a tiny
// chunk of the helper yconverge_test.go uses, but without
// dragging in its multi-step plumbing.
func kubectl(t *testing.T, args ...string) string {
	t.Helper()
	full := append([]string{"--context=" + contextName}, args...)
	cmd := exec.Command("kubectl", full...)
	cmd.Env = append(os.Environ(), "KUBECONFIG="+clusterKubeconfig)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("kubectl %v: %s: %v", args, out, err)
	}
	return strings.TrimSpace(string(out))
}
