//go:build e2e

package e2e

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"go.uber.org/zap"

	"github.com/Yolean/y-cluster/pkg/yconverge"
)

const (
	kwokImage     = "registry.k8s.io/kwok/cluster:v0.7.0-k8s.v1.33.0"
	containerName = "y-cluster-e2e"
	contextName   = "y-cluster-e2e"
)

// testdataDir returns the absolute path to testdata/.
func testdataDir(t *testing.T) string {
	t.Helper()
	abs, err := filepath.Abs("../testdata")
	if err != nil {
		t.Fatal(err)
	}
	return abs
}

// setupCluster creates a kwok cluster in Docker and returns a cleanup function.
// Writes a kubeconfig to a temp file and returns its path.
func setupCluster(t *testing.T) string {
	t.Helper()
	ctx := context.Background()

	// Check Docker is available
	if err := exec.CommandContext(ctx, "docker", "info").Run(); err != nil {
		t.Skip("Docker not available")
	}

	// Remove any leftover container
	_ = exec.CommandContext(ctx, "docker", "rm", "-f", containerName).Run()

	// Start kwok
	cmd := exec.CommandContext(ctx, "docker", "run", "-d",
		"--name", containerName,
		"-p", "0:8080",
		kwokImage)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("failed to start kwok: %s: %v", out, err)
	}

	t.Cleanup(func() {
		_ = exec.Command("docker", "rm", "-f", containerName).Run()
	})

	// Get mapped port
	portOut, err := exec.CommandContext(ctx, "docker", "port", containerName, "8080").Output()
	if err != nil {
		t.Fatalf("failed to get port: %v", err)
	}
	hostPort := strings.TrimSpace(string(portOut))
	// docker port returns "0.0.0.0:12345" or "[::]:12345"
	parts := strings.Split(hostPort, ":")
	port := parts[len(parts)-1]

	// Write kubeconfig
	kubeconfig := filepath.Join(t.TempDir(), "kubeconfig")
	kubeconfigContent := fmt.Sprintf(`apiVersion: v1
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
`, port, contextName, contextName, contextName, contextName, contextName, contextName)

	if err := os.WriteFile(kubeconfig, []byte(kubeconfigContent), 0o600); err != nil {
		t.Fatal(err)
	}
	os.Setenv("KUBECONFIG", kubeconfig)
	t.Cleanup(func() { os.Unsetenv("KUBECONFIG") })

	// Wait for API server
	deadline := time.Now().Add(30 * time.Second)
	for {
		cmd := exec.CommandContext(ctx, "kubectl", "--context="+contextName, "get", "ns")
		cmd.Env = append(os.Environ(), "KUBECONFIG="+kubeconfig)
		if err := cmd.Run(); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("kwok cluster not ready after 30s")
		}
		time.Sleep(500 * time.Millisecond)
	}

	return kubeconfig
}

func logger(t *testing.T) *zap.Logger {
	t.Helper()
	l, err := zap.NewDevelopment()
	if err != nil {
		t.Fatal(err)
	}
	return l
}

func TestYconverge_Namespace(t *testing.T) {
	setupCluster(t)
	td := testdataDir(t)

	result, err := yconverge.Run(context.Background(), yconverge.Options{
		Context:      contextName,
		KustomizeDir: filepath.Join(td, "e2e-namespace"),
	}, logger(t))
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Steps) != 1 {
		t.Fatalf("expected 1 step, got %d", len(result.Steps))
	}
}

func TestYconverge_Idempotent(t *testing.T) {
	setupCluster(t)
	td := testdataDir(t)
	log := logger(t)

	// First apply
	_, err := yconverge.Run(context.Background(), yconverge.Options{
		Context:      contextName,
		KustomizeDir: filepath.Join(td, "e2e-namespace"),
	}, log)
	if err != nil {
		t.Fatal(err)
	}

	// Second apply — must succeed (idempotent)
	_, err = yconverge.Run(context.Background(), yconverge.Options{
		Context:      contextName,
		KustomizeDir: filepath.Join(td, "e2e-namespace"),
	}, log)
	if err != nil {
		t.Fatalf("idempotent re-apply failed: %v", err)
	}
}

func TestYconverge_DependencyOrdering(t *testing.T) {
	setupCluster(t)
	td := testdataDir(t)

	// e2e-dependency → e2e-configmap → e2e-namespace (transitive)
	result, err := yconverge.Run(context.Background(), yconverge.Options{
		Context:      contextName,
		KustomizeDir: filepath.Join(td, "e2e-dependency"),
	}, logger(t))
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Steps) != 3 {
		t.Fatalf("expected 3 steps (namespace→configmap→dependency), got %d: %v",
			len(result.Steps), basenames(result.Steps))
	}
	if filepath.Base(result.Steps[0]) != "e2e-namespace" {
		t.Fatalf("expected e2e-namespace first, got %s", filepath.Base(result.Steps[0]))
	}
	if filepath.Base(result.Steps[1]) != "e2e-configmap" {
		t.Fatalf("expected e2e-configmap second, got %s", filepath.Base(result.Steps[1]))
	}
	if filepath.Base(result.Steps[2]) != "e2e-dependency" {
		t.Fatalf("expected e2e-dependency last, got %s", filepath.Base(result.Steps[2]))
	}
}

func TestYconverge_IndirectChecks(t *testing.T) {
	setupCluster(t)
	td := testdataDir(t)

	// e2e-namespace must exist first (e2e-indirect → e2e-configmap needs it)
	_, err := yconverge.Run(context.Background(), yconverge.Options{
		Context:      contextName,
		KustomizeDir: filepath.Join(td, "e2e-namespace"),
	}, logger(t))
	if err != nil {
		t.Fatal(err)
	}

	// e2e-indirect has no yconverge.cue — checks come from e2e-configmap base
	_, err = yconverge.Run(context.Background(), yconverge.Options{
		Context:      contextName,
		KustomizeDir: filepath.Join(td, "e2e-indirect"),
	}, logger(t))
	if err != nil {
		t.Fatal(err)
	}
}

func TestYconverge_NamespaceEnvVar(t *testing.T) {
	setupCluster(t)
	td := testdataDir(t)

	// Create the namespace first
	_, err := yconverge.Run(context.Background(), yconverge.Options{
		Context:      contextName,
		KustomizeDir: filepath.Join(td, "e2e-namespace"),
	}, logger(t))
	if err != nil {
		t.Fatal(err)
	}

	// The check verifies $NAMESPACE = "y-cluster-e2e"
	_, err = yconverge.Run(context.Background(), yconverge.Options{
		Context:      contextName,
		KustomizeDir: filepath.Join(td, "e2e-namespace-check"),
	}, logger(t))
	if err != nil {
		t.Fatal(err)
	}
}

func TestYconverge_PrintDeps(t *testing.T) {
	// No cluster needed for print-deps
	td := testdataDir(t)

	result, err := yconverge.Run(context.Background(), yconverge.Options{
		Context:      "unused",
		KustomizeDir: filepath.Join(td, "e2e-dependency"),
		PrintDeps:    true,
	}, logger(t))
	if err != nil {
		t.Fatal(err)
	}
	names := basenames(result.Steps)
	if len(names) != 3 {
		t.Fatalf("expected 3 deps, got %v", names)
	}
	if names[0] != "e2e-namespace" || names[1] != "e2e-configmap" || names[2] != "e2e-dependency" {
		t.Fatalf("unexpected order: %v", names)
	}
}

func TestYconverge_ChecksOnly(t *testing.T) {
	setupCluster(t)
	td := testdataDir(t)

	// Apply namespace first
	_, err := yconverge.Run(context.Background(), yconverge.Options{
		Context:      contextName,
		KustomizeDir: filepath.Join(td, "e2e-namespace"),
	}, logger(t))
	if err != nil {
		t.Fatal(err)
	}

	// Checks-only: verify without re-applying
	_, err = yconverge.Run(context.Background(), yconverge.Options{
		Context:      contextName,
		KustomizeDir: filepath.Join(td, "e2e-namespace"),
		ChecksOnly:   true,
	}, logger(t))
	if err != nil {
		t.Fatal(err)
	}
}

func basenames(paths []string) []string {
	var names []string
	for _, p := range paths {
		names = append(names, filepath.Base(p))
	}
	return names
}
