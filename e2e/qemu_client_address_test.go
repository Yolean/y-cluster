//go:build e2e && kvm

package e2e

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	"go.uber.org/zap"

	"github.com/Yolean/y-cluster/pkg/echo"
	"github.com/Yolean/y-cluster/pkg/provision/qemu"
)

// slirpClientAddress is the source address qemu user-mode networking
// gives every connection that arrives through a host port forward.
const slirpClientAddress = "10.0.2.2"

// TestQemu_BackendSeesConnectionSourceAddress: the in-cluster path
// (k3s ServiceLB -> Envoy Gateway's LoadBalancer Service -> envoy)
// hands a backend the address the connection had when it reached the
// node. Under user-mode networking that is slirp's 10.0.2.2, which is
// all this network mode can ever offer; the assertion is that the
// cluster does not replace it with a pod address on top.
//
// Observed when this breaks: X-Forwarded-For carries the klipper-lb
// pod's 10.42.x.y address, because the connection went through
// klipper's MASQUERADE instead of kube-proxy's externalTrafficPolicy
// Local rule. That is also what the first seconds after the Gateway
// comes up look like, before ServiceLB has published the node IP, so
// the test waits for the answer to settle rather than take the first.
func TestQemu_BackendSeesConnectionSourceAddress(t *testing.T) {
	if _, err := os.Stat("/dev/kvm"); err != nil {
		t.Skip("QEMU tests require /dev/kvm")
	}
	if err := qemu.CheckPrerequisites(); err != nil {
		t.Skip(err)
	}

	logger, _ := zap.NewDevelopment()
	cfg := e2eQEMURuntime()
	cfg.Name = "y-cluster-e2e-clientaddr"
	cfg.Context = "y-cluster-e2e-clientaddr"
	cfg.CacheDir = e2eQEMUCacheDir(t)
	cfg.Memory = "4096"
	cfg.CPUs = "2"
	cfg.SSHPort = "2233"
	const httpPort = "28480"
	cfg.PortForwards = e2eUniqueForwards("26480", httpPort)
	cfg.Kubeconfig = os.Getenv("KUBECONFIG")
	if cfg.Kubeconfig == "" {
		t.Skip("KUBECONFIG must be set")
	}

	ctx := context.Background()
	if _, err := qemu.Provision(ctx, cfg, logger); err != nil {
		t.Fatalf("Provision: %v", err)
	}
	t.Cleanup(func() { _ = qemu.TeardownConfig(cfg, false, logger) })

	manifest, err := echo.Render(echo.Options{})
	if err != nil {
		t.Fatalf("render echo: %v", err)
	}
	apply := exec.Command("kubectl", "--context="+cfg.Context, "apply", "--server-side", "-f", "-")
	apply.Env = append(os.Environ(), "KUBECONFIG="+cfg.Kubeconfig)
	apply.Stdin = strings.NewReader(string(manifest))
	if out, err := apply.CombinedOutput(); err != nil {
		t.Fatalf("apply echo: %s: %v", out, err)
	}

	url := fmt.Sprintf("http://127.0.0.1:%s/q/envoy/echo", httpPort)
	var last string
	deadline := time.Now().Add(5 * time.Minute)
	settled := 0
	for time.Now().Before(deadline) {
		xff, err := forwardedFor(url)
		if err != nil {
			last = err.Error()
			settled = 0
		} else {
			last = xff
			if xff == slirpClientAddress {
				settled++
				if settled == 5 {
					return
				}
			} else {
				settled = 0
			}
		}
		time.Sleep(3 * time.Second)
	}
	t.Fatalf("backend never consistently saw %s as X-Forwarded-For; last answer: %s", slirpClientAddress, last)
}

// forwardedFor requests the echo backend and returns the
// X-Forwarded-For value envoy handed it.
func forwardedFor(url string) (string, error) {
	// A fresh connection per sample. A kept-alive connection opened
	// before ServiceLB published the node IP stays on the klipper
	// path (conntrack pins it) and keeps reporting the klipper pod's
	// address for as long as it lives.
	client := &http.Client{
		Timeout:   8 * time.Second,
		Transport: &http.Transport{DisableKeepAlives: true},
	}
	resp, err := client.Get(url)
	if err != nil {
		return "", err
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", err
	}
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("HTTP %d: %.200s", resp.StatusCode, body)
	}
	var parsed struct {
		Headers map[string][]string `json:"headers"`
	}
	if err := json.Unmarshal(body, &parsed); err != nil {
		return "", fmt.Errorf("parse echo response: %w: %.200s", err, body)
	}
	return strings.Join(parsed.Headers["x-forwarded-for"], ","), nil
}
