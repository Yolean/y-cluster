package images

import (
	"context"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/google/go-containerregistry/pkg/name"
	"github.com/google/go-containerregistry/pkg/registry"
	"github.com/google/go-containerregistry/pkg/v1/empty"
	"github.com/google/go-containerregistry/pkg/v1/layout"
	"github.com/google/go-containerregistry/pkg/v1/random"
	"github.com/google/go-containerregistry/pkg/v1/remote"
)

func TestCache_MalformedRefErrors(t *testing.T) {
	t.Setenv("Y_CLUSTER_CACHE_DIR", t.TempDir())
	_, err := Cache(context.Background(), "::not a ref::", "", nil)
	if err == nil {
		t.Fatal("expected parse error")
	}
}

func TestCache_UnreachableRegistryPropagates(t *testing.T) {
	t.Setenv("Y_CLUSTER_CACHE_DIR", t.TempDir())
	// 127.0.0.1:1 is reserved-not-listening on every host we care
	// about; HEAD will fail with a transport error which Cache
	// must surface, not swallow.
	_, err := Cache(context.Background(), "127.0.0.1:1/foo:bar", "", nil)
	if err == nil {
		t.Fatal("expected transport error")
	}
	if !strings.Contains(err.Error(), "HEAD") {
		t.Fatalf("error should wrap HEAD: %v", err)
	}
}

// TestCache_RoundTripWithRandomImage runs the full happy path
// without any network egress: push a small randomly-built image
// into an in-process registry, Cache from it with a tempdir as
// the override root, and assert the returned digestRef matches
// the image content (not just "has @sha256: shape") and that
// the layout landed at the expected path.
//
// This also exercises the path the cmd-side `--cache=false`
// flow takes: Cache with a tempdir as cacheRoot writes to
// <tempdir>/images/<digest>/ and the caller cleans up. No
// separate Export() codepath needed.
func TestCache_RoundTripWithRandomImage(t *testing.T) {
	srv := httptest.NewServer(registry.New())
	defer srv.Close()
	u, err := url.Parse(srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	img, err := random.Image(1024, 1)
	if err != nil {
		t.Fatal(err)
	}
	ref, err := name.NewTag(u.Host + "/test/cacheroundtrip:v1")
	if err != nil {
		t.Fatal(err)
	}
	if err := remote.Write(ref, img); err != nil {
		t.Fatal(err)
	}

	root := t.TempDir()
	t.Setenv("Y_CLUSTER_CACHE_DIR", root)
	digestRef, err := Cache(context.Background(), ref.String(), "", nil)
	if err != nil {
		t.Fatalf("Cache: %v", err)
	}
	imgDigest, err := img.Digest()
	if err != nil {
		t.Fatal(err)
	}
	wantRef := ref.Context().Name() + "@" + imgDigest.String()
	if digestRef != wantRef {
		t.Errorf("digestRef: got %q, want %q", digestRef, wantRef)
	}
	layoutDir := filepath.Join(root, "images", imgDigest.String())
	if _, err := os.Stat(filepath.Join(layoutDir, "oci-layout")); err != nil {
		t.Errorf("oci-layout missing at expected path %s: %v", layoutDir, err)
	}
}

// TestCache_DigestPinnedInputPreservesDigest: digest-pinned
// input must skip the registry HEAD entirely. We can't directly
// assert "no HEAD" without a hooked transport, but the
// request trace in -v test output makes the absence visible if
// we're suspicious. Behavioral assertion: returned ref equals
// the input ref byte-for-byte.
func TestCache_DigestPinnedInputPreservesDigest(t *testing.T) {
	srv := httptest.NewServer(registry.New())
	defer srv.Close()
	u, err := url.Parse(srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	img, err := random.Image(512, 1)
	if err != nil {
		t.Fatal(err)
	}
	ref, err := name.NewTag(u.Host + "/test/cachedigestin:v1")
	if err != nil {
		t.Fatal(err)
	}
	if err := remote.Write(ref, img); err != nil {
		t.Fatal(err)
	}
	imgDigest, err := img.Digest()
	if err != nil {
		t.Fatal(err)
	}
	digestInput := ref.Context().Name() + "@" + imgDigest.String()

	t.Setenv("Y_CLUSTER_CACHE_DIR", t.TempDir())
	got, err := Cache(context.Background(), digestInput, "", nil)
	if err != nil {
		t.Fatalf("Cache: %v", err)
	}
	if got != digestInput {
		t.Errorf("digest-pinned input must round-trip verbatim: got %q want %q", got, digestInput)
	}
}

// TestResolveDigest_DigestInputIsPassThrough: ResolveDigest on
// a digest-pinned input must not touch the network. Against
// an unrouteable host a HEAD would error; success here proves
// no network IO occurred.
func TestResolveDigest_DigestInputIsPassThrough(t *testing.T) {
	const ref = "127.0.0.1:1/foo@sha256:1111111111111111111111111111111111111111111111111111111111111111"
	got, err := ResolveDigest(context.Background(), ref)
	if err != nil {
		t.Fatalf("ResolveDigest: %v", err)
	}
	if got != ref {
		t.Errorf("got %q, want %q", got, ref)
	}
}

// TestResolveDigest_TagInputHEADs: tag input must HEAD the
// registry; against an unreachable host that produces a
// transport error. Assert the error wraps the HEAD step.
func TestResolveDigest_TagInputHEADs(t *testing.T) {
	_, err := ResolveDigest(context.Background(), "127.0.0.1:1/foo:tag")
	if err == nil {
		t.Fatal("expected HEAD error against unreachable host")
	}
	if !strings.Contains(err.Error(), "HEAD") {
		t.Errorf("error should wrap the HEAD step: %v", err)
	}
}

// TestCache_FallbackFromRegistryK8s is the network-touching
// end-to-end assertion. registry.k8s.io/pause is the canonical
// tiny test image. -short skips.
func TestCache_FallbackFromRegistryK8s(t *testing.T) {
	if testing.Short() {
		t.Skip("skipped under -short (network pull from registry.k8s.io)")
	}
	t.Setenv("Y_CLUSTER_CACHE_DIR", t.TempDir())
	digestRef, err := Cache(context.Background(), "registry.k8s.io/pause:3.10", "", nil)
	if err != nil {
		t.Fatalf("Cache against registry.k8s.io: %v", err)
	}
	if !strings.HasPrefix(digestRef, "registry.k8s.io/pause@sha256:") {
		t.Errorf("digestRef shape: %q", digestRef)
	}
}

// Layout-existence edge cases — cheap to cover here without a
// real registry. End-to-end "warm cache → no-op → byte-equal
// layout" coverage lives in CI4e against a registry container.
func TestLayoutExists_Empty(t *testing.T) {
	dir := t.TempDir()
	ok, err := layoutExists(dir)
	if err != nil {
		t.Fatal(err)
	}
	if ok {
		t.Fatal("empty dir should not look like an OCI layout")
	}
}

// A pull that died after layout.Write but before its first manifest
// leaves oci-layout plus an empty index.json. That must not read as a
// cache hit, and the next Cache call has to pull over it.
func TestCache_PullsOverAKilledPull(t *testing.T) {
	srv := httptest.NewServer(registry.New())
	defer srv.Close()
	u, err := url.Parse(srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	img, err := random.Image(1024, 1)
	if err != nil {
		t.Fatal(err)
	}
	ref, err := name.NewTag(u.Host + "/test/killedpull:v1")
	if err != nil {
		t.Fatal(err)
	}
	if err := remote.Write(ref, img); err != nil {
		t.Fatal(err)
	}
	imgDigest, err := img.Digest()
	if err != nil {
		t.Fatal(err)
	}

	root := t.TempDir()
	t.Setenv("Y_CLUSTER_CACHE_DIR", root)
	layoutDir := filepath.Join(root, "images", imgDigest.String())
	if _, err := layout.Write(layoutDir, empty.Index); err != nil {
		t.Fatal(err)
	}
	if ok, err := layoutExists(layoutDir); err != nil || ok {
		t.Fatalf("a layout without manifests must not count as cached: ok=%v err=%v", ok, err)
	}

	if _, err := Cache(context.Background(), ref.String(), "", nil); err != nil {
		t.Fatalf("Cache: %v", err)
	}
	if ok, err := layoutExists(layoutDir); err != nil || !ok {
		t.Fatalf("layout not usable after re-pull: ok=%v err=%v", ok, err)
	}
	lp, err := layout.FromPath(layoutDir)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := lp.Image(imgDigest); err != nil {
		t.Fatalf("image not readable from the cache: %v", err)
	}
}

// Nothing of a failed pull may stay behind, neither under the final
// name nor as a staging directory.
func TestCache_FailedPullLeavesNothingBehind(t *testing.T) {
	srv := httptest.NewServer(registry.New())
	u, err := url.Parse(srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	img, err := random.Image(1024, 1)
	if err != nil {
		t.Fatal(err)
	}
	imgDigest, err := img.Digest()
	if err != nil {
		t.Fatal(err)
	}
	// Digest-pinned, so Cache needs no HEAD and fails in the fetch.
	ref := u.Host + "/test/never-pushed@" + imgDigest.String()
	srv.Close()

	root := t.TempDir()
	t.Setenv("Y_CLUSTER_CACHE_DIR", root)
	if _, err := Cache(context.Background(), ref, "", nil); err == nil {
		t.Fatal("expected the pull to fail")
	}
	entries, err := os.ReadDir(filepath.Join(root, "images"))
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		t.Errorf("left behind after a failed pull: %s", e.Name())
	}
}

func TestLayoutExists_TruncatedIndex(t *testing.T) {
	dir := t.TempDir()
	for name, content := range map[string]string{"oci-layout": `{"imageLayoutVersion":"1.0.0"}`, "index.json": `{"schemaVersion":2,"manif`} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if ok, err := layoutExists(dir); err != nil || ok {
		t.Fatalf("truncated index.json: ok=%v err=%v, want unusable without error", ok, err)
	}
}
