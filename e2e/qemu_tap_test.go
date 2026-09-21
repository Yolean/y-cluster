//go:build e2e && kvm

package e2e

import (
	"context"
	"fmt"
	"net"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	"go.uber.org/zap"

	"github.com/Yolean/y-cluster/pkg/echo"
	"github.com/Yolean/y-cluster/pkg/provision/config"
	"github.com/Yolean/y-cluster/pkg/provision/qemu"
)

// TestQemu_TapMode needs a host prepared by its operator, which takes
// root once, so it skips unless told which device to use:
//
//	sudo scripts/qemu-tap-host-setup.sh ycl0 10.88.0.1/24
//	Y_CLUSTER_E2E_TAP_IFNAME=ycl0 Y_CLUSTER_E2E_TAP_GUEST_ADDRESS=10.88.0.2/24 \
//	  go test -tags 'e2e kvm' -run TestQemu_TapMode ./e2e/
//
// The guest needs outbound internet for the k3s airgap-less parts of
// provision (image pulls), i.e. the host has to masquerade the
// subnet; `qemu-tap-host-setup.sh nft` prints a ruleset that does.
//
// What it pins, next to TestQemu_BackendSeesConnectionSourceAddress
// for user mode: a backend behind the bundled Envoy Gateway sees the
// address the client really has. The client here is this host, whose
// address towards the guest is the gateway address on the tap device;
// in user mode the same request shows slirp's 10.0.2.2 whoever sent
// it.
func TestQemu_TapMode(t *testing.T) {
	ifname := os.Getenv("Y_CLUSTER_E2E_TAP_IFNAME")
	guestAddress := os.Getenv("Y_CLUSTER_E2E_TAP_GUEST_ADDRESS")
	if ifname == "" || guestAddress == "" {
		t.Skip("set Y_CLUSTER_E2E_TAP_IFNAME and Y_CLUSTER_E2E_TAP_GUEST_ADDRESS after preparing the host with scripts/qemu-tap-host-setup.sh")
	}
	if _, err := os.Stat("/dev/kvm"); err != nil {
		t.Skip("QEMU tests require /dev/kvm")
	}
	if err := qemu.CheckPrerequisites(); err != nil {
		t.Skip(err)
	}
	kubeconfigPath := os.Getenv("KUBECONFIG")
	if kubeconfigPath == "" {
		t.Skip("KUBECONFIG must be set")
	}

	c := &config.QEMUConfig{
		CommonConfig: config.CommonConfig{
			Provider: config.ProviderQEMU,
			Name:     "y-cluster-e2e-tap",
			Context:  "y-cluster-e2e-tap",
			Memory:   "4096",
			CPUs:     "2",
		},
		DiskSize: "40G",
		Network:  config.QEMUNetwork{Mode: config.QEMUNetworkModeTap, Ifname: ifname, GuestAddress: guestAddress},
	}
	c.ApplyDefaults()
	if err := c.Validate(); err != nil {
		t.Fatalf("config: %v", err)
	}
	cfg := qemu.FromConfig(c)
	cfg.CacheDir = e2eQEMUCacheDir(t)
	cfg.Kubeconfig = kubeconfigPath
	guestIP, gateway := c.Network.GuestIP(), c.Network.Gateway

	logger, _ := zap.NewDevelopment()
	ctx := context.Background()
	if _, err := qemu.Provision(ctx, cfg, logger); err != nil {
		t.Fatalf("Provision: %v", err)
	}
	t.Cleanup(func() { _ = qemu.TeardownConfig(cfg, false, logger) })

	kubectl := func(args ...string) ([]byte, error) {
		cmd := exec.Command("kubectl", append([]string{"--context=" + cfg.Context}, args...)...)
		cmd.Env = append(os.Environ(), "KUBECONFIG="+kubeconfigPath)
		return cmd.CombinedOutput()
	}

	// 1. The written kubeconfig reaches the apiserver on the guest's
	// own address, and its certificate is valid for that address.
	if out, err := kubectl("config", "view", "--minify", "-o", "jsonpath={.clusters[0].cluster.server}"); err != nil {
		t.Fatalf("kubectl config view: %s: %v", out, err)
	} else if want := "https://" + guestIP + ":6443"; string(out) != want {
		t.Errorf("kubeconfig server %q, want %q", out, want)
	}
	if out, err := kubectl("get", "nodes", "-o", "name"); err != nil || !strings.Contains(string(out), "node/") {
		t.Fatalf("kubectl get nodes: %s: %v", out, err)
	}

	// 2. The client address survives to the backend.
	manifest, err := echo.Render(echo.Options{})
	if err != nil {
		t.Fatal(err)
	}
	apply := exec.Command("kubectl", "--context="+cfg.Context, "apply", "--server-side", "-f", "-")
	apply.Env = append(os.Environ(), "KUBECONFIG="+kubeconfigPath)
	apply.Stdin = strings.NewReader(string(manifest))
	if out, err := apply.CombinedOutput(); err != nil {
		t.Fatalf("apply echo: %s: %v", out, err)
	}
	url := fmt.Sprintf("http://%s/q/envoy/echo", guestIP)
	var last string
	settled := 0
	for deadline := time.Now().Add(5 * time.Minute); time.Now().Before(deadline); time.Sleep(3 * time.Second) {
		xff, err := forwardedFor(url)
		if err != nil {
			last, settled = err.Error(), 0
			continue
		}
		last = xff
		if xff != gateway {
			settled = 0
			continue
		}
		if settled++; settled == 5 {
			break
		}
	}
	if settled < 5 {
		t.Errorf("backend never consistently saw this host's address %s as X-Forwarded-For; last answer: %s", gateway, last)
	}

	// 3. Stop and start rebuild the same attachment.
	if err := qemu.Stop(cfg.CacheDir, cfg.Name, logger); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	if _, err := qemu.Start(ctx, cfg.CacheDir, cfg.Name, logger); err != nil {
		t.Fatalf("Start: %v", err)
	}
	// Start returns once the guest answers ssh; the apiserver needs
	// a little longer and reports ServiceUnavailable meanwhile.
	var nodesOut []byte
	var nodesErr error
	for deadline := time.Now().Add(3 * time.Minute); time.Now().Before(deadline); time.Sleep(5 * time.Second) {
		if nodesOut, nodesErr = kubectl("get", "nodes", "-o", "name"); nodesErr == nil {
			break
		}
	}
	if nodesErr != nil {
		t.Errorf("kubectl get nodes after stop/start: %s: %v", nodesOut, nodesErr)
	}

	// 4. Teardown never touches the operator's device.
	if err := qemu.TeardownConfig(cfg, false, logger); err != nil {
		t.Fatalf("Teardown: %v", err)
	}
	if _, err := net.InterfaceByName(ifname); err != nil {
		t.Errorf("tap device %s is gone after teardown: %v", ifname, err)
	}
}
