//go:build e2e

package cluster

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

const (
	kwokImage         = "registry.k8s.io/kwok/cluster:v0.7.0-k8s.v1.33.0"
	kwokContainerName = "y-cluster-e2e-kwok"
	kwokContextName   = "y-cluster-e2e-kwok"
)

var (
	kwokOnce     sync.Once
	kwokInstance *Cluster
	kwokSetupErr error
)

// Kwok returns a process-wide kwok cluster, bringing it up on the
// first call. Skips the test (rather than failing) when Docker is
// unavailable so a developer with neither Docker nor /dev/kvm gets
// a clean skip instead of a failure they can't reproduce.
//
// Re-entrancy: subsequent calls in the same `go test` invocation
// return the same Cluster. Each caller still gets its own
// KUBECONFIG env hook (via t.Cleanup) so the var is unset after
// the calling test's cleanup runs.
//
// Teardown is process-wide: the harness's TestMain (registered
// via TeardownAll) removes the container after all tests finish.
func Kwok(t *testing.T) *Cluster {
	t.Helper()
	kwokOnce.Do(setupKwok)
	if kwokSetupErr != nil {
		t.Skipf("kwok unavailable: %v", kwokSetupErr)
	}

	prev, hadPrev := os.LookupEnv("KUBECONFIG")
	os.Setenv("KUBECONFIG", kwokInstance.Kubeconfig)
	t.Cleanup(func() {
		if hadPrev {
			os.Setenv("KUBECONFIG", prev)
		} else {
			os.Unsetenv("KUBECONFIG")
		}
	})
	return kwokInstance
}

// TeardownAll removes any clusters this package brought up. Call
// from a test package's TestMain after m.Run() so a green or red
// run both leave the host clean.
//
// Each backend's teardown is a best-effort `docker rm -f` (or
// equivalent) — failures are intentionally swallowed because
// TestMain runs after the test framework can no longer report
// them, and a leftover container is preferable to obscuring a
// real test failure.
func TeardownAll() {
	_ = exec.Command("docker", "rm", "-f", kwokContainerName).Run()
	teardownLocalRegistry()
}

func setupKwok() {
	ctx := context.Background()

	if err := exec.CommandContext(ctx, "docker", "info").Run(); err != nil {
		kwokSetupErr = fmt.Errorf("docker not available: %w", err)
		return
	}

	// Best-effort cleanup of a previous run that crashed before
	// TeardownAll could fire. Ignore errors — `rm -f` of a
	// nonexistent container is fine.
	_ = exec.CommandContext(ctx, "docker", "rm", "-f", kwokContainerName).Run()

	cmd := exec.CommandContext(ctx, "docker", "run", "-d",
		"--name", kwokContainerName,
		"-p", "0:8080",
		kwokImage)
	if out, err := cmd.CombinedOutput(); err != nil {
		kwokSetupErr = fmt.Errorf("start kwok: %s: %w", out, err)
		return
	}

	portOut, err := exec.CommandContext(ctx, "docker", "port", kwokContainerName, "8080").Output()
	if err != nil {
		kwokSetupErr = fmt.Errorf("docker port: %w", err)
		return
	}
	parts := strings.Split(strings.TrimSpace(string(portOut)), ":")
	port := parts[len(parts)-1]

	dir, err := os.MkdirTemp("", "y-cluster-e2e-kwok-*")
	if err != nil {
		kwokSetupErr = fmt.Errorf("kubeconfig tmpdir: %w", err)
		return
	}
	kubeconfigPath := filepath.Join(dir, "kubeconfig")
	content := fmt.Sprintf(`apiVersion: v1
kind: Config
clusters:
- cluster:
    server: http://127.0.0.1:%s
  name: %s
contexts:
- context:
    cluster: %s
    user: %s
  name: %s
current-context: %s
users:
- name: %s
`, port, kwokContextName, kwokContextName, kwokContextName, kwokContextName, kwokContextName, kwokContextName)
	if err := os.WriteFile(kubeconfigPath, []byte(content), 0o600); err != nil {
		kwokSetupErr = fmt.Errorf("write kubeconfig: %w", err)
		return
	}

	// Wait until the apiserver is reachable AND the bootstrap
	// namespaces exist. The cheaper `kubectl get svc` answers
	// "No resources found" within ~500ms — but kwok's namespace
	// bootstrap takes longer; SSA against `default` returns 404
	// in the gap. Polling `kubectl get namespace default`
	// closes that race.
	deadline := time.Now().Add(30 * time.Second)
	for {
		probe := exec.CommandContext(ctx, "kubectl", "--context="+kwokContextName,
			"get", "namespace", "default")
		probe.Env = append(os.Environ(), "KUBECONFIG="+kubeconfigPath)
		if err := probe.Run(); err == nil {
			break
		}
		if time.Now().After(deadline) {
			kwokSetupErr = fmt.Errorf("kwok not ready after 30s")
			return
		}
		time.Sleep(500 * time.Millisecond)
	}

	kwokInstance = &Cluster{
		Backend:    BackendKwok,
		Context:    kwokContextName,
		Kubeconfig: kubeconfigPath,
	}
}
