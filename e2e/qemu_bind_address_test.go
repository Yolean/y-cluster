//go:build e2e && kvm

package e2e

import (
	"context"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"go.uber.org/zap"

	"github.com/Yolean/y-cluster/pkg/provision/qemu"
)

// TestQemu_ForwardsBindLoopbackByDefault: with the default
// network.bindAddress, the VM's ssh forward and every port forward
// listen on 127.0.0.1 only. On a host with a public address the
// alternative publishes the guest's sshd and the k3s API.
func TestQemu_ForwardsBindLoopbackByDefault(t *testing.T) {
	if _, err := os.Stat("/dev/kvm"); err != nil {
		t.Skip("QEMU tests require /dev/kvm")
	}
	if err := qemu.CheckPrerequisites(); err != nil {
		t.Skip(err)
	}
	if _, err := exec.LookPath("ss"); err != nil {
		t.Skip("ss (iproute2) not on PATH")
	}

	logger, _ := zap.NewDevelopment()
	cfg := e2eQEMURuntime()
	cfg.Name = "y-cluster-e2e-bindaddr"
	cfg.Context = "y-cluster-e2e-bindaddr"
	cfg.CacheDir = e2eQEMUCacheDir(t)
	cfg.Memory = "2048"
	cfg.CPUs = "2"
	cfg.SSHPort = "2231"
	cfg.PortForwards = e2eUniqueForwards("26460", "28460")
	cfg.Kubeconfig = os.Getenv("KUBECONFIG")
	if cfg.Kubeconfig == "" {
		t.Skip("KUBECONFIG must be set")
	}
	// The gateway is not what is under test.
	cfg.Gateway.Skip = true

	if cfg.BindAddress != "127.0.0.1" {
		t.Fatalf("default bind address: got %q, want 127.0.0.1", cfg.BindAddress)
	}

	ctx := context.Background()
	if _, err := qemu.Provision(ctx, cfg, logger); err != nil {
		t.Fatalf("Provision: %v", err)
	}
	t.Cleanup(func() { _ = qemu.TeardownConfig(cfg, false, logger) })

	pidBytes, err := os.ReadFile(filepath.Join(cfg.CacheDir, cfg.Name+".pid"))
	if err != nil {
		t.Fatalf("read pidfile: %v", err)
	}
	pid := strings.TrimSpace(string(pidBytes))

	// Every listening socket the qemu process owns, as "addr:port".
	out, err := exec.Command("ss", "-ltnpH").CombinedOutput()
	if err != nil {
		t.Fatalf("ss: %s: %v", out, err)
	}
	listening := map[string]bool{}
	for _, line := range strings.Split(string(out), "\n") {
		if !strings.Contains(line, "pid="+pid+",") {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) < 4 {
			t.Fatalf("unexpected ss line: %q", line)
		}
		listening[fields[3]] = true
	}
	want := []string{cfg.SSHPort}
	for _, pf := range cfg.PortForwards {
		want = append(want, pf.Host)
	}
	for _, port := range want {
		if !listening["127.0.0.1:"+port] {
			t.Errorf("forward %s is not listening on 127.0.0.1; qemu listens on %v", port, listening)
		}
	}
	for addr := range listening {
		if !strings.HasPrefix(addr, "127.0.0.1:") {
			t.Errorf("qemu listens on %s; every forward must be loopback-only by default", addr)
		}
	}

	// The same thing from the outside: the host's own non-loopback
	// address must refuse the forwarded ports.
	if hostIP := firstNonLoopbackIPv4(); hostIP == "" {
		t.Log("host has no non-loopback IPv4 address; skipping the dial check")
	} else {
		for _, port := range want {
			addr := net.JoinHostPort(hostIP, port)
			if c, err := net.DialTimeout("tcp4", addr, 2*time.Second); err == nil {
				_ = c.Close()
				t.Errorf("%s accepted a connection; the forward is exposed beyond loopback", addr)
			}
		}
	}

	// And the cluster is still reachable the way y-cluster wrote it.
	kubectl := exec.Command("kubectl", "--context="+cfg.Context, "get", "nodes", "-o", "name")
	kubectl.Env = append(os.Environ(), "KUBECONFIG="+cfg.Kubeconfig)
	if out, err := kubectl.CombinedOutput(); err != nil {
		t.Errorf("kubectl get nodes through the loopback forward: %s: %v", out, err)
	} else if !strings.Contains(string(out), "node/") {
		t.Errorf("kubectl get nodes returned no node: %s", out)
	}
}

func firstNonLoopbackIPv4() string {
	addrs, err := net.InterfaceAddrs()
	if err != nil {
		return ""
	}
	for _, a := range addrs {
		ipnet, ok := a.(*net.IPNet)
		if !ok || ipnet.IP.IsLoopback() || ipnet.IP.To4() == nil {
			continue
		}
		return fmt.Sprint(ipnet.IP)
	}
	return ""
}
