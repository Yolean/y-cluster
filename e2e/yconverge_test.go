//go:build e2e

// Package e2e tests yconverge against a real cluster (kwok in Docker).
//
// Test bases model a three-tier application:
//   - e2e-db:       database config and service (foundation)
//   - e2e-backend:  backend that depends on db (CUE import)
//   - e2e-frontend: frontend that depends on backend (transitive)
//
// Each tier has base/ (the module) and optionally qa/ (a kustomize overlay).
// This structure tests both CUE-based dependency ordering (db before backend)
// and kustomize-based customization (qa overlay aggregates checks from base).
package e2e

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"sync"
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

func testdataDir(t *testing.T) string {
	t.Helper()
	abs, err := filepath.Abs("../testdata")
	if err != nil {
		t.Fatal(err)
	}
	return abs
}

func TestMain(m *testing.M) {
	code := m.Run()
	_ = exec.Command("docker", "rm", "-f", containerName).Run()
	os.Exit(code)
}

var clusterOnce sync.Once
var clusterKubeconfig string

func setupCluster(t *testing.T) {
	t.Helper()

	clusterOnce.Do(func() {
		ctx := context.Background()

		if err := exec.CommandContext(ctx, "docker", "info").Run(); err != nil {
			t.Skip("Docker not available")
		}

		_ = exec.CommandContext(ctx, "docker", "rm", "-f", containerName).Run()

		cmd := exec.CommandContext(ctx, "docker", "run", "-d",
			"--name", containerName,
			"-p", "0:8080",
			kwokImage)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("start kwok: %s: %v", out, err)
		}

		portOut, err := exec.CommandContext(ctx, "docker", "port", containerName, "8080").Output()
		if err != nil {
			t.Fatalf("get port: %v", err)
		}
		parts := strings.Split(strings.TrimSpace(string(portOut)), ":")
		port := parts[len(parts)-1]

		dir, err := os.MkdirTemp("", "y-cluster-e2e-*")
		if err != nil {
			t.Fatal(err)
		}
		clusterKubeconfig = filepath.Join(dir, "kubeconfig")
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
`, port, contextName, contextName, contextName, contextName, contextName, contextName)

		if err := os.WriteFile(clusterKubeconfig, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}

		deadline := time.Now().Add(30 * time.Second)
		for {
			cmd := exec.CommandContext(ctx, "kubectl", "--context="+contextName, "get", "svc")
			cmd.Env = append(os.Environ(), "KUBECONFIG="+clusterKubeconfig)
			if err := cmd.Run(); err == nil {
				break
			}
			if time.Now().After(deadline) {
				t.Fatal("kwok not ready after 30s")
			}
			time.Sleep(500 * time.Millisecond)
		}
	})

	os.Setenv("KUBECONFIG", clusterKubeconfig)
	t.Cleanup(func() { os.Unsetenv("KUBECONFIG") })
}

func logger(t *testing.T) *zap.Logger {
	t.Helper()
	l, err := zap.NewDevelopment()
	if err != nil {
		t.Fatal(err)
	}
	return l
}

// --- Ordering: CUE imports create separate convergence steps ---

func TestOrdering_DbBeforeBackend(t *testing.T) {
	setupCluster(t)
	td := testdataDir(t)

	result, err := yconverge.Run(context.Background(), yconverge.Options{
		Context:      contextName,
		KustomizeDir: filepath.Join(td, "e2e-backend/base"),
	}, logger(t))
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Steps) != 2 {
		t.Fatalf("expected 2 steps, got %v", basenames(result.Steps))
	}
	dbIdx := indexOfDir(result.Steps, "e2e-db")
	backendIdx := indexOfDir(result.Steps, "e2e-backend")
	if dbIdx < 0 || backendIdx < 0 {
		t.Fatalf("missing steps: %v", result.Steps)
	}
	if dbIdx >= backendIdx {
		t.Fatalf("db must come before backend: %v", result.Steps)
	}
}

func TestOrdering_TransitiveChain(t *testing.T) {
	setupCluster(t)
	td := testdataDir(t)

	// frontend → backend → db
	result, err := yconverge.Run(context.Background(), yconverge.Options{
		Context:      contextName,
		KustomizeDir: filepath.Join(td, "e2e-frontend/base"),
	}, logger(t))
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Steps) != 3 {
		t.Fatalf("expected 3 steps, got %v", basenames(result.Steps))
	}
	dbIdx := indexOfDir(result.Steps, "e2e-db")
	backendIdx := indexOfDir(result.Steps, "e2e-backend")
	frontendIdx := indexOfDir(result.Steps, "e2e-frontend")
	if dbIdx >= backendIdx || backendIdx >= frontendIdx {
		t.Fatalf("wrong order: db=%d backend=%d frontend=%d in %v",
			dbIdx, backendIdx, frontendIdx, result.Steps)
	}
}

func TestOrdering_PrintDepsNoCluster(t *testing.T) {
	td := testdataDir(t)

	result, err := yconverge.Run(context.Background(), yconverge.Options{
		Context:      "unused",
		KustomizeDir: filepath.Join(td, "e2e-frontend/base"),
		PrintDeps:    true,
	}, logger(t))
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Steps) != 3 {
		t.Fatalf("expected 3 steps, got %v", basenames(result.Steps))
	}
}

// --- Customization: kustomize overlays aggregate checks from base ---

func TestCustomization_QaOverlayAggregatesBaseChecks(t *testing.T) {
	setupCluster(t)
	td := testdataDir(t)

	// db/qa has no yconverge.cue — checks come from db/base via traversal
	_, err := yconverge.Run(context.Background(), yconverge.Options{
		Context:      contextName,
		KustomizeDir: filepath.Join(td, "e2e-db/qa"),
	}, logger(t))
	if err != nil {
		t.Fatal(err)
	}
}

func TestCustomization_BackendQaResolvesDbDependency(t *testing.T) {
	setupCluster(t)
	td := testdataDir(t)

	// backend/qa wraps backend/base which depends on db
	result, err := yconverge.Run(context.Background(), yconverge.Options{
		Context:      contextName,
		KustomizeDir: filepath.Join(td, "e2e-backend/qa"),
	}, logger(t))
	if err != nil {
		t.Fatal(err)
	}
	dbIdx := indexOfDir(result.Steps, "e2e-db")
	if dbIdx < 0 {
		t.Fatalf("db dependency not resolved from qa overlay: %v", result.Steps)
	}
}

// --- Idempotency ---

func TestIdempotent_ReapplySucceeds(t *testing.T) {
	setupCluster(t)
	td := testdataDir(t)
	log := logger(t)

	for i := 0; i < 2; i++ {
		_, err := yconverge.Run(context.Background(), yconverge.Options{
			Context:      contextName,
			KustomizeDir: filepath.Join(td, "e2e-db/base"),
		}, log)
		if err != nil {
			t.Fatalf("apply %d failed: %v", i+1, err)
		}
	}
}

// --- ChecksOnly ---

func TestChecksOnly_SkipsApply(t *testing.T) {
	setupCluster(t)
	td := testdataDir(t)
	log := logger(t)

	_, err := yconverge.Run(context.Background(), yconverge.Options{
		Context:      contextName,
		KustomizeDir: filepath.Join(td, "e2e-db/base"),
	}, log)
	if err != nil {
		t.Fatal(err)
	}

	_, err = yconverge.Run(context.Background(), yconverge.Options{
		Context:      contextName,
		KustomizeDir: filepath.Join(td, "e2e-db/base"),
		ChecksOnly:   true,
	}, log)
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

func indexOfDir(steps []string, segment string) int {
	for i, s := range steps {
		if strings.Contains(s, segment) {
			return i
		}
	}
	return -1
}
