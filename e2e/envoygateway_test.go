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
// path -- CRDs first, then the install manifest, then the
// controller-resources strategic-merge patch, then the EnvoyProxy
// CR, then the default GatewayClass with parametersRef --
// against the shared kwok cluster. The Deployment rollout wait
// is skipped (ReadyTimeout=-1) because kwok stages pods through
// its own controller, not the real one, and we only need to
// prove the apply path produces the right object graph.
//
// Resource requests are non-zero by design: they exercise the
// kubectl-patch branch (controller) and the EnvoyProxy CR
// branch (proxy) which production CommonConfig defaults flow
// through. Pre-fix the patch step failed with kubectl's
// client-side schema validation -- PR-CI didn't catch it
// because this test was calling Install without the resource
// fields set, short-circuiting both branches.
//
// Coverage assertions afterwards use kubectl directly so we can
// verify what landed without making the test depend on every
// internal helper that does the same.
func TestEnvoyGateway_InstallAgainstKwok(t *testing.T) {
	setupCluster(t)

	const (
		ctrlCPU  = "10m"
		ctrlMem  = "64Mi"
		proxyCPU = "10m"
		proxyMem = "128Mi"
	)
	if err := envoygateway.Install(context.Background(), envoygateway.Options{
		ContextName:          contextName,
		CacheOverride:        sharedEnvoyGatewayCache(t),
		Logger:               logger(t),
		ReadyTimeout:         -1,          // skip wait: kwok doesn't run the real controller
		GatewayClassName:     "y-cluster", // matches the production default
		DNSHintIP:            "127.0.0.1", // simulates qemu/docker host-loopback case
		ControllerCPURequest: ctrlCPU,
		ControllerMemRequest: ctrlMem,
		ProxyCPURequest:      proxyCPU,
		ProxyMemRequest:      proxyMem,
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
	gcOut := kubectl(t, "get", "gatewayclass", "y-cluster",
		"-o", "jsonpath={.spec.controllerName}")
	want := "gateway.envoyproxy.io/gatewayclass-controller"
	if gcOut != want {
		t.Errorf("GatewayClass y-cluster.spec.controllerName = %q, want %q", gcOut, want)
	}

	// dns-hint-ip annotation landed: this is the contract ystack's
	// y-k8s-ingress-hosts (and any future host-side resolver tool)
	// reads to find the host-routable address without user-side
	// config. Pinned because consumers cite the exact annotation key.
	hintOut := kubectl(t, "get", "gatewayclass", "y-cluster",
		"-o", "jsonpath={.metadata.annotations."+strings.ReplaceAll(envoygateway.DNSHintIPAnnotation, ".", "\\.")+"}")
	if hintOut != "127.0.0.1" {
		t.Errorf("GatewayClass y-cluster annotation %s = %q, want 127.0.0.1",
			envoygateway.DNSHintIPAnnotation, hintOut)
	}

	// Controller-resources strategic-merge patch landed. The
	// kubectl-patch step would fail outright pre-fix (kubectl
	// rejected the partial SSA-apply manifest), so reaching this
	// assertion at all proves the patch verb works; reading the
	// requests back proves the merge target is the right
	// container and the values stuck.
	cpuOut := kubectl(t, "get", "deployment", envoygateway.DeploymentName,
		"-n", envoygateway.Namespace,
		"-o", `jsonpath={.spec.template.spec.containers[?(@.name=="envoy-gateway")].resources.requests.cpu}`)
	if cpuOut != ctrlCPU {
		t.Errorf("controller resources.requests.cpu = %q, want %q (patch step landed?)", cpuOut, ctrlCPU)
	}
	memOut := kubectl(t, "get", "deployment", envoygateway.DeploymentName,
		"-n", envoygateway.Namespace,
		"-o", `jsonpath={.spec.template.spec.containers[?(@.name=="envoy-gateway")].resources.requests.memory}`)
	if memOut != ctrlMem {
		t.Errorf("controller resources.requests.memory = %q, want %q (patch step landed?)", memOut, ctrlMem)
	}

	// EnvoyProxy CR was applied with the proxy resource values.
	// Pin both axes so a future schema rename inside the CR
	// surfaces as a failure rather than a silent no-op.
	proxyCPUOut := kubectl(t, "get", "envoyproxy", envoygateway.EnvoyProxyName,
		"-n", envoygateway.Namespace,
		"-o", "jsonpath={.spec.provider.kubernetes.envoyDeployment.container.resources.requests.cpu}")
	if proxyCPUOut != proxyCPU {
		t.Errorf("EnvoyProxy proxy.cpu = %q, want %q", proxyCPUOut, proxyCPU)
	}
	proxyMemOut := kubectl(t, "get", "envoyproxy", envoygateway.EnvoyProxyName,
		"-n", envoygateway.Namespace,
		"-o", "jsonpath={.spec.provider.kubernetes.envoyDeployment.container.resources.requests.memory}")
	if proxyMemOut != proxyMem {
		t.Errorf("EnvoyProxy proxy.memory = %q, want %q", proxyMemOut, proxyMem)
	}

	// GatewayClass parametersRef points at our EnvoyProxy CR so
	// Gateways under the class inherit the tuned resources
	// without any per-Gateway boilerplate.
	refKind := kubectl(t, "get", "gatewayclass", "y-cluster",
		"-o", "jsonpath={.spec.parametersRef.kind}")
	if refKind != "EnvoyProxy" {
		t.Errorf("GatewayClass parametersRef.kind = %q, want EnvoyProxy", refKind)
	}
	refName := kubectl(t, "get", "gatewayclass", "y-cluster",
		"-o", "jsonpath={.spec.parametersRef.name}")
	if refName != envoygateway.EnvoyProxyName {
		t.Errorf("GatewayClass parametersRef.name = %q, want %q", refName, envoygateway.EnvoyProxyName)
	}
}

// TestEnvoyGateway_InstallEmptyClassNameSkipsApply verifies that
// passing GatewayClassName="" makes Install skip the GatewayClass
// apply (controller still installs). This is the test-only path
// for "controller without a default GatewayClass"; the production
// CommonConfig.GatewayConfig is all-or-nothing per cluster
// config, but the underlying Options field stays expressive.
func TestEnvoyGateway_InstallEmptyClassNameSkipsApply(t *testing.T) {
	setupCluster(t)

	if err := envoygateway.Install(context.Background(), envoygateway.Options{
		ContextName:      contextName,
		CacheOverride:    sharedEnvoyGatewayCache(t),
		Logger:           logger(t),
		ReadyTimeout:     -1,
		GatewayClassName: "", // explicit: do not apply a GatewayClass
	}); err != nil {
		t.Fatalf("Install: %v", err)
	}

	// Tests share the kwok cluster, so a previously-created
	// GatewayClass may still be present from another test; we
	// can't assert "no GatewayClass exists". What we can assert
	// is that Install did not error -- proving the empty-name
	// path is wired and doesn't crash on the missing resource.
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
