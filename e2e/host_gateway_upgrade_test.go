//go:build e2e && kvm

package e2e

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Yolean/y-cluster/pkg/provision/envoygateway"
	"github.com/Yolean/y-cluster/pkg/provision/qemu"
)

// Real Envoy Gateway releases, pinned with the sha256 of their
// install.yaml as a cluster's config pins them. The test starts a
// minor below y-cluster's own release, so that the minor upgrade has
// a real release to move to; v1.9.2 is a newer patch of the pin.
const (
	gatewayOlderMinor       = "v1.8.5"
	gatewayOlderMinorSHA256 = "0639894d98328025b9626e5169fc11a19d107ed99bc1dd7054983978110369c9"
	gatewayNewerPatch       = "v1.9.2"
	gatewayNewerPatchSHA256 = "0412a72907e57ff9b73c56a7bf6df5190bf0f6e4f8bb4bba34e38630bbab5778"
	// The proxy image of v1.9.x is distroless-v1.39.1; this is a later
	// Envoy patch, as an operator would set it for a fix that no
	// Envoy Gateway release ships yet.
	gatewayProxyImage = "docker.io/envoyproxy/envoy:distroless-v1.39.5@sha256:9d84a586119a27b50437dbb01eccd4722832741539b1ed53c8e9d6da6067a6e0"
)

// TestHost_GatewayUpgrade moves Envoy Gateway on a k3s host between
// real releases with `y-cluster platform install`, as an operator's
// converge does, and checks the version rule on the way:
//
//  1. a configured release older than y-cluster's pin installs on a
//     new cluster, and --check finds nothing to change;
//  2. y-cluster's pin, a newer minor, is refused without
//     --gateway-allow-minor, by install and by --check, and changes
//     nothing; with the flag it upgrades, with a warning;
//  3. a newer patch upgrades without a flag;
//  4. the previous release again is refused, and changes nothing;
//  5. proxyImage replaces the proxy's image, and removing it restores
//     the release's own.
//
// After each move the controller and the proxy run what was
// configured, a second install changes nothing, and --check agrees.
// A Gateway of the y-cluster class makes Envoy Gateway run a proxy.
func TestHost_GatewayUpgrade(t *testing.T) {
	if _, err := os.Stat("/dev/kvm"); err != nil {
		t.Skip("QEMU tests require /dev/kvm")
	}
	if err := qemu.CheckPrerequisites(); err != nil {
		t.Skip(err)
	}
	if os.Getenv("KUBECONFIG") == "" {
		t.Skip("KUBECONFIG must be set")
	}
	const (
		name        = "y-cluster-e2e-host-gateway"
		contextName = "y-cluster-e2e-host-gateway"
		user        = "deployer"
	)
	bin := buildBinary(t)
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Minute)
	defer cancel()

	cfgDir := t.TempDir()
	configure := func(gateway string) {
		t.Helper()
		writeFile(t, filepath.Join(cfgDir, "y-cluster-provision.yaml"), `provider: host
context: `+contextName+`
gateway:
  dnsHintIP: 127.0.0.1
`+gateway)
	}
	configure("  version: " + gatewayOlderMinor + "\n  installSha256: " + gatewayOlderMinorSHA256 + "\n")

	host := bootHost(t, ctx, name, user)
	host.applyBundle(t, ctx, bin, cfgDir)
	host.importKubeconfig(t, ctx, bin, contextName)

	kubectl := func(args ...string) string {
		t.Helper()
		out, err := exec.CommandContext(ctx, "kubectl", append([]string{"--context=" + contextName}, args...)...).CombinedOutput()
		if err != nil {
			t.Fatalf("kubectl %s: %s: %v", strings.Join(args, " "), out, err)
		}
		return strings.TrimSpace(string(out))
	}
	controllerImage := func() string {
		return kubectl("-n", envoygateway.Namespace, "get", "deployment", envoygateway.DeploymentName,
			"-o", `jsonpath={.spec.template.spec.containers[?(@.name=="envoy-gateway")].image}`)
	}
	// The proxy fleet is the Deployment Envoy Gateway creates for the
	// class's merged Gateways.
	proxyImage := func() string {
		return kubectl("-n", envoygateway.Namespace, "get", "deployment",
			"-l", "gateway.envoyproxy.io/owning-gatewayclass=y-cluster",
			"-o", `jsonpath={.items[*].spec.template.spec.containers[?(@.name=="envoy")].image}`)
	}
	// converged waits for the controller and the proxy to run what was
	// configured and for the certgen hook Job to delete itself, then
	// checks that a second install changes nothing and --check agrees.
	converged := func(release, proxy string) {
		t.Helper()
		if got := controllerImage(); !strings.HasSuffix(got, "envoyproxy/gateway:"+release) {
			t.Fatalf("controller image %s, want release %s", got, release)
		}
		kubectl("-n", envoygateway.Namespace, "rollout", "status", "deployment/"+envoygateway.DeploymentName, "--timeout=5m")
		waitFor(t, ctx, 5*time.Minute, "proxy image "+proxy, func() (bool, string) {
			got := proxyImage()
			return got == proxy, got
		})
		proxyDeployment := kubectl("-n", envoygateway.Namespace, "get", "deployment",
			"-l", "gateway.envoyproxy.io/owning-gatewayclass=y-cluster", "-o", "name")
		kubectl("-n", envoygateway.Namespace, "rollout", "status", proxyDeployment, "--timeout=5m")
		waitFor(t, ctx, 3*time.Minute, "certgen Job to delete itself", func() (bool, string) {
			out := kubectl("-n", envoygateway.Namespace, "get", "job", "--ignore-not-found", "-o", "name")
			return out == "", out
		})
		before, _ := objectVersions(t, contextName)
		runCLI(t, bin, nil, "platform", "install", "-c", cfgDir)
		if after, _ := objectVersions(t, contextName); after != before {
			t.Errorf("%s: a second platform install changed objects:\nbefore:\n%s\nafter:\n%s", release, before, after)
		}
		if out, err := cliOutput(bin, nil, "platform", "install", "-c", cfgDir, "--check"); err != nil {
			t.Errorf("%s: --check on a converged cluster: %v\n%s", release, err, out)
		}
	}
	// refused runs install and --check, expects both to fail with the
	// rule and both releases named, and the cluster unchanged.
	refused := func(installed, configured, rule string) {
		t.Helper()
		before, _ := objectVersions(t, contextName)
		for _, args := range [][]string{
			{"platform", "install", "-c", cfgDir},
			{"platform", "install", "-c", cfgDir, "--check"},
		} {
			out, err := cliOutput(bin, nil, args...)
			if err == nil {
				t.Fatalf("%v: %s to %s was not refused:\n%s", args, installed, configured, out)
			}
			for _, s := range []string{installed + " is installed and " + configured + " is configured", rule, "nothing was changed"} {
				if !strings.Contains(string(out), s) {
					t.Errorf("%v: output does not say %q:\n%s", args, s, out)
				}
			}
			if strings.Contains(string(out), "would change") {
				t.Errorf("%v: a refused move reported as a diff:\n%s", args, out)
			}
		}
		if after, _ := objectVersions(t, contextName); after != before {
			t.Errorf("a refused move changed objects:\nbefore:\n%s\nafter:\n%s", before, after)
		}
	}

	// 1. A release older than the pin, on a new cluster.
	runCLI(t, bin, nil, "platform", "install", "-c", cfgDir)
	assertNodeReady(t, contextName, os.Getenv("KUBECONFIG"))
	applyStdin(t, contextName, `apiVersion: gateway.networking.k8s.io/v1
kind: Gateway
metadata:
  name: upgrade-e2e
  namespace: default
spec:
  gatewayClassName: y-cluster
  listeners:
  - name: http
    protocol: HTTP
    port: 80
`)
	converged(gatewayOlderMinor, "docker.io/envoyproxy/envoy:distroless-v1.38.4@sha256:b28fbee81528c5b6e8857412e5e0f48ea5baa0199cf73ab611aa7f88a808eba7")

	// 2. The pin, a newer minor: refused, then allowed.
	configure("")
	refused(gatewayOlderMinor, envoygateway.Version, "--gateway-allow-minor allows it")
	out, err := cliOutput(bin, nil, "platform", "install", "-c", cfgDir, "--gateway-allow-minor")
	if err != nil {
		t.Fatalf("minor upgrade with --gateway-allow-minor: %v\n%s", err, out)
	}
	if !strings.Contains(string(out), "minor upgrade: best-effort") {
		t.Errorf("a minor upgrade without its warning:\n%s", out)
	}
	pinnedProxy := "docker.io/envoyproxy/envoy:distroless-v1.39.1@sha256:eb2c01c13125d1629637cb4e4cce7207009fb7cc2c8027f9742758549d15b6f4"
	converged(envoygateway.Version, pinnedProxy)

	// 3. A newer patch, from the config.
	configure("  version: " + gatewayNewerPatch + "\n  installSha256: " + gatewayNewerPatchSHA256 + "\n")
	runCLI(t, bin, nil, "platform", "install", "-c", cfgDir)
	converged(gatewayNewerPatch, pinnedProxy)

	// 4. Back to the pin: a downgrade.
	configure("")
	refused(gatewayNewerPatch, envoygateway.Version, "a downgrade is refused")

	// 5. A proxy image ahead of the release, and back.
	configure("  version: " + gatewayNewerPatch + "\n  installSha256: " + gatewayNewerPatchSHA256 + "\n  proxyImage: " + gatewayProxyImage + "\n")
	runCLI(t, bin, nil, "platform", "install", "-c", cfgDir)
	converged(gatewayNewerPatch, gatewayProxyImage)
	configure("  version: " + gatewayNewerPatch + "\n  installSha256: " + gatewayNewerPatchSHA256 + "\n")
	runCLI(t, bin, nil, "platform", "install", "-c", cfgDir)
	converged(gatewayNewerPatch, pinnedProxy)
}

func applyStdin(t *testing.T, contextName, manifest string) {
	t.Helper()
	cmd := exec.Command("kubectl", "--context="+contextName, "apply", "-f", "-")
	cmd.Stdin = strings.NewReader(manifest)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("kubectl apply: %s: %v", out, err)
	}
}
