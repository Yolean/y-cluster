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
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"go.uber.org/zap"

	"github.com/Yolean/y-cluster/e2e/cluster"
	"github.com/Yolean/y-cluster/pkg/yconverge"
)

// contextName and clusterKubeconfig are populated by setupCluster
// from the shared cluster harness so other test files in this
// package can reach the kubeconfig + context without each
// importing e2e/cluster directly.
var (
	contextName       string
	clusterKubeconfig string
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
	cluster.TeardownAll()
	os.Exit(code)
}

// setupCluster brings up (or reuses) the shared kwok cluster and
// stashes its kubeconfig metadata in package-level vars so tests
// across files can use them. Skips the test when Docker is
// unavailable.
func setupCluster(t *testing.T) {
	t.Helper()
	c := cluster.Kwok(t)
	contextName = c.Context
	clusterKubeconfig = c.Kubeconfig
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

func TestOrdering_ChecksGateNextApply(t *testing.T) {
	setupCluster(t)
	td := testdataDir(t)

	// Delete the marker to ensure a clean slate
	exec.Command("kubectl", "--context="+contextName, "delete", "configmap", "db-check-marker", "--ignore-not-found").Run()

	// backend depends on db. The db check creates a marker ConfigMap.
	// The backend check verifies the marker exists, proving:
	//   1. db was applied
	//   2. db checks ran (creating the marker)
	//   3. THEN backend was applied
	//   4. backend checks ran (reading the marker)
	//
	// If both were bundled into one atomic apply, the marker would
	// not exist when backend's check runs.
	_, err := yconverge.Run(context.Background(), yconverge.Options{
		Context:      contextName,
		KustomizeDir: filepath.Join(td, "e2e-backend/base"),
	}, logger(t))
	if err != nil {
		t.Fatal(err)
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

// TestChecksOnly_PropagatesToDeps covers Q14 from
// QUESTIONS_TO_CLUSTER_MAINTAINERS.md: --checks-only on a target with
// dependencies must NOT re-apply the deps. We prove the propagation
// by deleting a dep's resource after a successful converge and then
// running --checks-only on the parent: with propagation the dep's
// check fails (resource missing) because no apply happens; without
// propagation the dep would silently get re-applied and the check
// would pass.
func TestChecksOnly_PropagatesToDeps(t *testing.T) {
	setupCluster(t)
	td := testdataDir(t)
	log := logger(t)

	// First: full converge of the chain (db is a dep of backend).
	if _, err := yconverge.Run(context.Background(), yconverge.Options{
		Context:      contextName,
		KustomizeDir: filepath.Join(td, "e2e-backend/base"),
	}, log); err != nil {
		t.Fatal(err)
	}

	// Delete the db's ConfigMap. Its yconverge check is `kubectl get
	// configmap db-config`, which now fails until the next apply.
	out, err := exec.Command("kubectl", "--context="+contextName,
		"delete", "configmap", "db-config", "--ignore-not-found=true").CombinedOutput()
	if err != nil {
		t.Fatalf("delete db-config: %s: %v", out, err)
	}

	// --checks-only on backend should propagate to db, and db's
	// check should fail because nothing re-applied the configmap.
	_, err = yconverge.Run(context.Background(), yconverge.Options{
		Context:      contextName,
		KustomizeDir: filepath.Join(td, "e2e-backend/base"),
		ChecksOnly:   true,
	}, log)
	if err == nil {
		t.Fatal("expected dep check to fail when --checks-only propagates and the dep's resource is missing")
	}
	if !strings.Contains(err.Error(), "db") {
		t.Fatalf("expected db check failure in error, got %v", err)
	}

	// Restore db-config so subsequent tests don't see the missing
	// resource if the suite is re-run.
	if _, err := yconverge.Run(context.Background(), yconverge.Options{
		Context:      contextName,
		KustomizeDir: filepath.Join(td, "e2e-db/base"),
	}, log); err != nil {
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
