// Package cilium installs Cilium as the CNI of a cluster whose machine
// config was generated without one. The manifest is rendered from the
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

//go:embed cilium.yaml
var manifest []byte

// Version is the chart (and Cilium) version cilium.yaml was rendered
// from, read off the manifest so it cannot drift from render.sh.
var Version = func() string {
	m := regexp.MustCompile(`quay\.io/cilium/cilium:v([0-9.]+)`).FindSubmatch(manifest)
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
func Install(ctx context.Context, contextName string, nodes int, timeout time.Duration, logger *zap.Logger) error {
	if logger == nil {
		logger = zap.NewNop()
	}
	logger.Info("applying cilium manifest", zap.String("version", Version), zap.String("namespace", Namespace))
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
