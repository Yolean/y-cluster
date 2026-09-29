// Package cilium installs Cilium as the CNI of a cluster started without
// one: Talos nodes generated with no CNI (glesys), and k3s started with
// --flannel-backend=none (qemu). The manifests are rendered from the
// upstream Helm chart by render.sh and embedded, so provisioning needs
// no helm and every cluster gets the same manifest; the values are in
// render.sh. Cilium is here for what flannel cannot do: encrypt pod
// traffic between nodes (WireGuard) and enforce NetworkPolicy.
package cilium

import (
	"bytes"
	"context"
	_ "embed"
	"fmt"
	"os"
	"os/exec"
	"regexp"
	"strings"
	"time"

	"go.uber.org/zap"
)

// Variant selects the manifest rendered for a node distribution.
type Variant string

const (
	// Talos nodes: no module loading, cgroup v2 mounted by the OS.
	Talos Variant = "talos"
	// K3s nodes: portmap chaining for the hostPorts of k3s ServiceLB,
	// see K3sPortmapCommand.
	K3s Variant = "k3s"
)

//go:embed cilium-talos.yaml
var talosManifest []byte

//go:embed cilium-k3s.yaml
var k3sManifest []byte

// K3sPortmapCommand makes k3s's bundled portmap plugin available where
// containerd looks for CNI plugins once k3s runs without flannel
// (/opt/cni/bin, containerd's default). The target is k3s's own
// per-plugin symlink, which k3s repoints on upgrade; the plugin is a
// multi-call binary that dispatches on the name it is run as.
const K3sPortmapCommand = "sudo mkdir -p /opt/cni/bin && sudo ln -sfn /var/lib/rancher/k3s/data/cni/portmap /opt/cni/bin/portmap"

// Manifest returns the embedded manifest of a variant.
func Manifest(v Variant) ([]byte, error) {
	switch v {
	case Talos:
		return talosManifest, nil
	case K3s:
		return k3sManifest, nil
	default:
		return nil, fmt.Errorf("no cilium manifest for variant %q", v)
	}
}

// Version is the chart (and Cilium) version the manifests were rendered
// from, read off one of them so it cannot drift from render.sh.
var Version = func() string {
	m := regexp.MustCompile(`quay\.io/cilium/cilium:v([0-9.]+)`).FindSubmatch(k3sManifest)
	if m == nil {
		return "unknown"
	}
	return string(m[1])
}()

// Namespace is where the chart installs; kube-system as upstream
// recommends, since Cilium is the node's networking.
const Namespace = "kube-system"

// Install applies the manifest with server-side apply and waits for
// the cilium DaemonSet to roll out and every node to be Ready, which
// is when pods can be scheduled. Idempotent.
func Install(ctx context.Context, variant Variant, contextName string, nodes int, timeout time.Duration, logger *zap.Logger) error {
	if logger == nil {
		logger = zap.NewNop()
	}
	manifest, err := Manifest(variant)
	if err != nil {
		return err
	}
	logger.Info("applying cilium manifest", zap.String("version", Version), zap.String("variant", string(variant)), zap.String("namespace", Namespace))
	apply := exec.CommandContext(ctx, "kubectl", "--context="+contextName,
		"apply", "--server-side", "--force-conflicts", "--field-manager=y-cluster", "-f", "-")
	apply.Stdin = bytes.NewReader(manifest)
	apply.Stdout = os.Stdout
	apply.Stderr = os.Stderr
	if err := apply.Run(); err != nil {
		return fmt.Errorf("kubectl apply cilium: %w", err)
	}
	logger.Info("waiting for cilium rollout", zap.Duration("timeout", timeout))
	rollout := exec.CommandContext(ctx, "kubectl", "--context="+contextName, "-n", Namespace,
		"rollout", "status", "daemonset/cilium", "--timeout="+timeout.String())
	rollout.Stdout = os.Stdout
	rollout.Stderr = os.Stderr
	if err := rollout.Run(); err != nil {
		return fmt.Errorf("cilium rollout: %w", err)
	}
	return WaitNodesReady(ctx, contextName, nodes, timeout, logger)
}

// WaitNodesReady polls until at least n nodes report Ready.
func WaitNodesReady(ctx context.Context, contextName string, n int, timeout time.Duration, logger *zap.Logger) error {
	logger.Info("waiting for nodes to be Ready", zap.Int("nodes", n), zap.Duration("timeout", timeout))
	deadline := time.Now().Add(timeout)
	for {
		out, err := exec.CommandContext(ctx, "kubectl", "--context="+contextName, "get", "nodes",
			"-o", `jsonpath={range .items[*]}{.metadata.name}={.status.conditions[?(@.type=="Ready")].status}{"\n"}{end}`).Output()
		ready := 0
		if err == nil {
			for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
				if strings.HasSuffix(line, "=True") {
					ready++
				}
			}
			if ready >= n {
				return nil
			}
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("%d of %d nodes Ready after %s (last: %v %s)", ready, n, timeout, err, strings.TrimSpace(string(out)))
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(5 * time.Second):
		}
	}
}
