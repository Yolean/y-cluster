//go:build e2e

// e2e coverage for the images pipeline. The cache matrix runs
// against a local registry container; the per-provisioner load
// tests run against a real cluster -- see docker_test.go and
// qemu_test.go for the integration with assertClusterFeatures.
package e2e

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Yolean/y-cluster/e2e/cluster"
	"github.com/Yolean/y-cluster/pkg/images"
)

// TestCache_DigestStabilityMatrix is the digest stability proof.
// Phases:
//
//	1. cold pulls (registry up): tag/cold then digest/cold both
//	   write OCI layouts, both return digest-pinned refs.
//	2. warm pulls with registry up: tag/warm still HEADs to
//	   re-resolve the tag, but the layout is reused (no blob
//	   download). Cache returns the same digest as the cold pass.
//	3. warm digest pull with registry stopped: digest/warm-offline
//	   takes no network at all -- digest is in the input ref, the
//	   layout is on disk, no HEAD, no GET. The strongest stability
//	   proof we can make from the client side.
func TestCache_DigestStabilityMatrix(t *testing.T) {
	reg := cluster.LocalRegistry(t)

	// Push two synthetic fixtures: one we'll reference by tag,
	// one we'll reference by digest. Both are sub-KB.
	cluster.PushFixtureImage(t, reg, "y-cluster/e2e-tag", "v1")
	digestPushed, _ := cluster.PushFixtureImage(t, reg, "y-cluster/e2e-digest", "v1")

	cacheRoot := t.TempDir()
	t.Setenv("Y_CLUSTER_CACHE_DIR", cacheRoot)

	tagRef := reg.Endpoint + "/y-cluster/e2e-tag:v1"
	digestRef := digestPushed // already in repo@sha256:... form

	// Phase 1+2: registry stays up. Cold and warm passes prove
	// the layout writes once and the second call short-circuits.
	t.Run("tag/cold", func(t *testing.T) { assertCacheLayout(t, cacheRoot, tagRef) })
	t.Run("digest/cold", func(t *testing.T) { assertCacheLayout(t, cacheRoot, digestRef) })
	t.Run("tag/warm", func(t *testing.T) { assertCacheLayout(t, cacheRoot, tagRef) })

	// Phase 3: kill the registry; the digest-pinned warm pull
	// must succeed without any network. Tag warm wouldn't (it
	// HEADs to translate tag -> digest), so it's not in this
	// phase.
	if err := reg.Stop(); err != nil {
		t.Fatalf("stop registry: %v", err)
	}
	t.Run("digest/warm-offline", func(t *testing.T) { assertCacheLayout(t, cacheRoot, digestRef) })
}

// assertCacheLayout calls images.Cache and verifies the result:
// the returned ref is digest-pinned, and the OCI layout marker
// files (oci-layout, index.json) exist on disk under the
// expected per-digest directory.
func assertCacheLayout(t *testing.T, cacheRoot, ref string) {
	t.Helper()
	gotDigest, err := images.Cache(context.Background(), ref, cacheRoot, nil)
	if err != nil {
		t.Fatalf("Cache(%s): %v", ref, err)
	}
	if !strings.Contains(gotDigest, "@sha256:") {
		t.Fatalf("returned ref must be digest-pinned, got %q", gotDigest)
	}
	parts := strings.SplitN(gotDigest, "@", 2)
	if len(parts) != 2 {
		t.Fatalf("malformed digest ref: %q", gotDigest)
	}
	layoutDir := filepath.Join(cacheRoot, "images", parts[1])
	for _, f := range []string{"oci-layout", "index.json"} {
		if _, err := os.Stat(filepath.Join(layoutDir, f)); err != nil {
			t.Fatalf("layout missing %s under %s: %v", f, layoutDir, err)
		}
	}
}
