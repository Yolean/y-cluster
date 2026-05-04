//go:build e2e && docker

// docker e2e. Provisions a k3s cluster via the new
// docker provisioner, asserts that the merged kubeconfig works,
// then tears down. Gated on `docker info` succeeding.
package e2e

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"go.uber.org/zap"

	"github.com/Yolean/y-cluster/pkg/provision/config"
	"github.com/Yolean/y-cluster/pkg/provision/docker"
	"github.com/Yolean/y-cluster/pkg/yconverge"
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

	// Cold-start regression guard: remove both candidate k3s images
	// from the daemon so docker.Provision exercises the auto-pull
	// path on every CI run, not just the first one. Without this,
	// the test would pass on a host that already had the image and
	// silently miss a regression of the "ContainerCreate doesn't
	// auto-pull" bug. We don't fail on rmi errors -- the image may
	// genuinely be absent on a fresh host.
	for _, img := range []string{
		config.MirrorImage(cfg.K3s.Version),
		config.UpstreamImage(cfg.K3s.Version),
	} {
		_ = exec.Command("docker", "rmi", "-f", img).Run()
	}

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

// TestDocker_Stop is the regression guard for pkg/provision/docker.Stop.
// Stop is documented to terminate the container while preserving it
// (no remove) so a follow-up `docker container start` could resume the
// same instance -- the docker half of the appliance lifecycle that
// qemu's TestQemu_StopStart already exercises.
//
// The Stop function shipped in PR #19 with no e2e gate. This proves:
//   - the call returns nil for a normally-running container
//   - the container exists with State.Status == "exited" afterwards
//     (NOT removed; that's Teardown's job)
//   - kubectl through the merged context fails to reach the apiserver
//     because the host port forward is gone
func TestDocker_Stop(t *testing.T) {
	skipIfNoDocker(t)

	logger, _ := zap.NewDevelopment()
	cfg := e2eDockerConfig("y-cluster-e2e-stop", "36444", "y-cluster-e2e-stop")

	kcfgPath := os.Getenv("KUBECONFIG")
	if kcfgPath == "" {
		t.Skip("KUBECONFIG must be set")
	}

	_ = exec.Command("docker", "rm", "-f", cfg.Name).Run()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	cluster, err := docker.Provision(ctx, *cfg, logger)
	if err != nil {
		t.Fatal(err)
	}
	// Teardown removes the container even after Stop -- ContainerRemove
	// is force=true. Cleanup is unconditional so a Stop-without-Teardown
	// regression doesn't leak a stopped container into the next test.
	t.Cleanup(func() { _ = cluster.Teardown(false) })

	// Stop the running container. docker.Stop preserves the container
	// for a follow-up start; it should NOT remove it.
	if err := docker.Stop(ctx, cfg.Name, logger); err != nil {
		t.Fatalf("docker.Stop: %v", err)
	}

	// Container exists and is exited (not removed).
	statusOut, err := exec.CommandContext(ctx, "docker", "inspect",
		"--format", "{{.State.Status}}", cfg.Name).CombinedOutput()
	if err != nil {
		t.Fatalf("docker inspect after Stop: %s: %v", statusOut, err)
	}
	if got := strings.TrimSpace(string(statusOut)); got != "exited" {
		t.Fatalf("State.Status after Stop: got %q, want %q (container should be preserved-but-stopped)", got, "exited")
	}

	// kubectl through the merged context cannot reach the apiserver --
	// the host port forward dies with the container. We accept any
	// non-zero exit; the message is daemon-version-specific.
	kc := exec.CommandContext(ctx, "kubectl", "--context="+cluster.Context(),
		"--request-timeout=3s", "get", "nodes")
	kc.Env = append(os.Environ(), "KUBECONFIG="+kcfgPath)
	if out, err := kc.CombinedOutput(); err == nil {
		t.Fatalf("kubectl get nodes succeeded after Stop; expected failure.\nOutput:\n%s", out)
	}
}

// TestDocker_ApplianceStateful converges the testdata/appliance-stateful
// fixture against a real docker-provisioned cluster and asserts the
// StatefulSet rolls out and its PVC binds. Closes the localstorage gap
// in PR #19: install_test.go covers the install function; nothing
// otherwise verifies that converging a stateful workload through
// kubectl-yconverge actually produces a Bound PVC and a Ready pod
// against k3s's bundled local-path provisioner.
//
// Doubles as the only consumer of testdata/appliance-stateful/, which
// the PR ships referenced only by an export.go comment otherwise.
func TestDocker_ApplianceStateful(t *testing.T) {
	skipIfNoDocker(t)

	logger, _ := zap.NewDevelopment()
	cfg := e2eDockerConfig("y-cluster-e2e-appstate", "36445", "y-cluster-e2e-appstate")

	kcfgPath := os.Getenv("KUBECONFIG")
	if kcfgPath == "" {
		t.Skip("KUBECONFIG must be set")
	}

	_ = exec.Command("docker", "rm", "-f", cfg.Name).Run()

	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Minute)
	defer cancel()

	cluster, err := docker.Provision(ctx, *cfg, logger)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cluster.Teardown(false) })

	// Resolve testdata path relative to this test file.
	wd, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	kustomizeDir := filepath.Join(wd, "..", "testdata", "appliance-stateful", "base")
	if _, err := os.Stat(kustomizeDir); err != nil {
		t.Fatalf("testdata path %s: %v", kustomizeDir, err)
	}

	// yconverge.Run resolves the namespace dep first (per the cue
	// file's _dep_ns import), applies the base, and runs the cue
	// rollout check on the StatefulSet (180s timeout in the fixture).
	// A non-nil error here means either dep ordering broke, kustomize
	// rendering broke, or the StatefulSet didn't roll out within 180s.
	if _, err := yconverge.Run(ctx, yconverge.Options{
		Context:      cluster.Context(),
		KustomizeDir: kustomizeDir,
	}, logger); err != nil {
		t.Fatalf("yconverge.Run: %v", err)
	}

	// The fixture's cue file checks rollout but not PVC binding. k3s's
	// local-path provisioner is dynamic, so an unbound PVC would block
	// pod scheduling and rollout would already have failed -- but
	// asserting Bound directly catches a regression where, say, a
	// future provisioner change leaves PVCs Pending while pods are
	// somehow Ready against an emptyDir fallback. Belt-and-braces.
	pvc := exec.CommandContext(ctx, "kubectl", "--context="+cluster.Context(),
		"-n", "appliance-stateful", "get", "pvc", "data-versitygw-0",
		"-o", "jsonpath={.status.phase}")
	pvc.Env = append(os.Environ(), "KUBECONFIG="+kcfgPath)
	pvcOut, err := pvc.CombinedOutput()
	if err != nil {
		t.Fatalf("kubectl get pvc: %s: %v", pvcOut, err)
	}
	if got := strings.TrimSpace(string(pvcOut)); got != "Bound" {
		t.Fatalf("PVC data-versitygw-0 phase: got %q, want %q", got, "Bound")
	}
}
