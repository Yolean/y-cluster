//go:build e2e

// Package e2e holds the tests that need more than the Go toolchain:
// a docker daemon (kwok and k3s-in-docker clusters), /dev/kvm (the
// qemu provisioner), multipass, or a Hetzner token. Build tags select
// what a run covers: `e2e` alone is what CI runs with docker; `kvm`,
// `multipass` and `hetzner` add the tests for those.
package e2e

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"testing"
)

var (
	binaryOnce sync.Once
	binaryPath string
	binaryErr  error
)

// buildBinary compiles cmd/y-cluster from the working tree, once per
// test process, for the tests that drive the CLI the way a user does.
// TestMain removes it again through removeBuiltBinary.
func buildBinary(t *testing.T) string {
	t.Helper()
	binaryOnce.Do(func() {
		dir, err := os.MkdirTemp("", "y-cluster-e2e-bin-*")
		if err != nil {
			binaryErr = err
			return
		}
		out := filepath.Join(dir, "y-cluster")
		cmd := exec.Command("go", "build", "-o", out, "./cmd/y-cluster")
		cmd.Dir = ".."
		if outb, err := cmd.CombinedOutput(); err != nil {
			binaryErr = fmt.Errorf("build: %s: %w", outb, err)
			return
		}
		binaryPath = out
	})
	if binaryErr != nil {
		t.Fatal(binaryErr)
	}
	return binaryPath
}

// removeBuiltBinary deletes what buildBinary made. The directory
// cannot be a t.TempDir: it outlives the test that happened to ask
// first.
func removeBuiltBinary() {
	if binaryPath != "" {
		_ = os.RemoveAll(filepath.Dir(binaryPath))
	}
}
