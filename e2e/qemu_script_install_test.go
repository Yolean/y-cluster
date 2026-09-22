//go:build e2e && kvm

package e2e

import (
	"context"
	"os"
	"os/exec"
	"strings"
	"testing"

	"go.uber.org/zap"

	"github.com/Yolean/y-cluster/pkg/provision/qemu"
)

// TestQemu_ScriptInstall provisions with k3s.install: script, where
// the node downloads k3s itself. Every other qemu e2e uses the airgap
// default, while multipass defaults to the script install and hetzner
// has no other: neither of those can run in this suite, so this is
// the one place pkg/provision/k3s's script path meets a real node.
//
// Needs outbound HTTPS from the guest to get.k3s.io and GitHub.
func TestQemu_ScriptInstall(t *testing.T) {
	if _, err := os.Stat("/dev/kvm"); err != nil {
		t.Skip("QEMU tests require /dev/kvm")
	}
	if err := qemu.CheckPrerequisites(); err != nil {
		t.Skip(err)
	}

	logger, _ := zap.NewDevelopment()
	cfg := e2eQEMURuntime()
	cfg.Name = "y-cluster-e2e-script"
	cfg.Context = "y-cluster-e2e-script"
	cfg.CacheDir = e2eQEMUCacheDir(t)
	cfg.Memory = "2048"
	cfg.CPUs = "2"
	cfg.SSHPort = "2236"
	cfg.PortForwards = e2eUniqueForwards("26466", "28466")
	cfg.Kubeconfig = os.Getenv("KUBECONFIG")
	if cfg.Kubeconfig == "" {
		t.Skip("KUBECONFIG must be set")
	}
	// The gateway is not what is under test.
	cfg.Gateway.Skip = true
	cfg.K3s.Install = "script"

	ctx := context.Background()
	cluster, err := qemu.Provision(ctx, cfg, logger)
	if err != nil {
		t.Fatalf("Provision: %v", err)
	}
	t.Cleanup(func() { _ = qemu.TeardownConfig(cfg, false, logger) })

	// The installer honoured the pinned version rather than taking
	// whatever the stable channel points at today.
	out, err := cluster.NodeExec(ctx, "k3s --version", nil)
	if err != nil {
		t.Fatalf("k3s --version: %s: %v", out, err)
	}
	if !strings.Contains(string(out), cfg.K3s.Version) {
		t.Errorf("node runs %q, want version %s", strings.TrimSpace(string(out)), cfg.K3s.Version)
	}

	// Nothing was copied in from the host: no airgap image tarball.
	out, err = cluster.NodeExec(ctx, "sudo sh -c 'ls /var/lib/rancher/k3s/agent/images/*.tar.zst 2>/dev/null | wc -l'", nil)
	if err != nil {
		t.Fatalf("list airgap images: %s: %v", out, err)
	}
	if strings.TrimSpace(string(out)) != "0" {
		t.Errorf("airgap tarballs on a script-installed node: %s", out)
	}

	// Provision returned, so the apiserver had answered /readyz on
	// the node. The merged kubeconfig has to reach it from the host.
	kubectl := exec.Command("kubectl", "--context="+cfg.Context, "get", "--raw=/readyz")
	if out, err := kubectl.CombinedOutput(); err != nil || strings.TrimSpace(string(out)) != "ok" {
		t.Fatalf("host kubectl /readyz: %s: %v", out, err)
	}
}
