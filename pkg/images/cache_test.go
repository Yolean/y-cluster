package images

import (
	"context"
	"strings"
	"testing"
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
