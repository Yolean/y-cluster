//go:build e2e && docker

// docker e2e. Provisions a k3s cluster via the new
// docker provisioner, asserts that the merged kubeconfig works,
// then tears down. Gated on `docker info` succeeding.
package e2e

import (
	"context"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	"go.uber.org/zap"

	"github.com/Yolean/y-cluster/pkg/provision/config"
	"github.com/Yolean/y-cluster/pkg/provision/docker"
)

// e2eDockerConfig builds a defaults-applied DockerConfig. The
// caller picks unique container names and host ports to avoid
// collisions when several tests run sequentially.
//
// PortForwards on the e2e path uses a single guest:6443 entry
// rather than the production default (which would also try to
// bind 80/443 and collide with anything else on the host). The
// image used at provision time is resolved from
// CommonConfig.K3s.Version: pkg/provision/docker.ResolveImage
// probes the y-cluster mirror and falls back to upstream
// rancher/k3s when the mirror has no manifest yet, so local e2e
// works without any environment override.
func e2eDockerConfig(name, apiPort, ctxName string) *config.DockerConfig {
	c := &config.DockerConfig{
		CommonConfig: config.CommonConfig{
			Provider:     config.ProviderDocker,
			Name:         name,
			Context:      ctxName,
			PortForwards: []config.PortForward{{Host: apiPort, Guest: "6443"}},
		},
	}
	c.ApplyDefaults()
	return c
}

func skipIfNoDocker(t *testing.T) {
	t.Helper()
	if err := exec.Command("docker", "info").Run(); err != nil {
		t.Skip("docker tests require a working Docker daemon")
	}
	if err := docker.CheckPrerequisites(); err != nil {
		t.Skipf("docker prerequisite failed: %v", err)
	}
}

func TestDocker_ProvisionTeardown(t *testing.T) {
	skipIfNoDocker(t)

	logger, _ := zap.NewDevelopment()
	cfg := e2eDockerConfig("y-cluster-e2e-k3s", "36443", "y-cluster-e2e-k3s")

	// Regression guard for the "docker provider only exposes 6443"
	// shape the previous APIPort-only schema imposed. Adding a
	// non-API forward and asserting docker actually binds it on
	// the host proves that PortForwards is wired through to the
	// container HostConfig and not silently dropped.
	cfg.PortForwards = append(cfg.PortForwards, config.PortForward{Host: "38080", Guest: "8080"})

	// Provision-time registries.yaml: configure both a mirror and
	// an auth credential so the e2e exercises the full Marshal +
	// Tar + CopyToContainer path. We use a private registry name
	// k3s won't actually try to pull from during the test, so the
	// "wrong credentials" don't matter -- we're proving the file
	// reached the right path with the right body.
	cfg.Registries = config.Registries{
		Mirrors: map[string]config.RegistryMirror{
			"y-cluster-e2e-mirror.invalid": {
				Endpoint: []string{"http://127.0.0.1:55555"},
			},
		},
		Configs: map[string]config.RegistryConfig{
			"y-cluster-e2e-mirror.invalid": {
				Auth: &config.RegistryAuth{
					Username: "oauth2accesstoken",
					Password: "literal-test-token",
				},
			},
		},
	}

	kcfgPath := os.Getenv("KUBECONFIG")
	if kcfgPath == "" {
		t.Skip("KUBECONFIG must be set")
	}

	// Clean any leftover container from a previous failed run.
	_ = exec.Command("docker", "rm", "-f", cfg.Name).Run()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	cluster, err := docker.Provision(ctx, *cfg, logger)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cluster.Teardown(false) })

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

	// detect / ctr / crictl must work against the running
	// cluster via the merged kubeconfig context.
	assertClusterFeatures(t, cluster.Context(), "docker")

	// PortForwards regression: the extra 38080->8080 forward we
	// added above must show up in `docker port`. We don't ping the
	// guest port (no service is listening on 8080 inside the
	// container); proving that docker accepted the binding is what
	// the fix is about.
	pout, err := exec.CommandContext(ctx, "docker", "port", cfg.Name, "8080/tcp").CombinedOutput()
	if err != nil {
		t.Fatalf("docker port: %s: %v", pout, err)
	}
	if !strings.Contains(string(pout), ":38080") {
		t.Fatalf("expected extra forward 8080/tcp -> :38080 on host; got %q", pout)
	}

	// Registries provision-time write: verify the staged file
	// landed at the expected path with the expected body. The
	// container has been running for a while at this point so
	// containerd has long since opened the file -- we just read
	// it back to confirm both Marshal and the CopyToContainer
	// wiring worked.
	regOut, err := cluster.NodeExec(ctx, "cat /etc/rancher/k3s/registries.yaml", nil)
	if err != nil {
		t.Fatalf("read registries.yaml: %s: %v", regOut, err)
	}
	for _, want := range []string{"y-cluster-e2e-mirror.invalid", "literal-test-token", "oauth2accesstoken"} {
		if !strings.Contains(string(regOut), want) {
			t.Fatalf("registries.yaml missing %q.\nGot:\n%s", want, regOut)
		}
	}
}
