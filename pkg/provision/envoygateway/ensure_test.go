package envoygateway

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
)

// withInstallURL swaps the package-level URL template for the
// duration of a test, restoring it on cleanup. We don't run real
// network calls in unit tests.
func withInstallURL(t *testing.T, u string) {
	t.Helper()
	prev := installURL
	installURL = u
	t.Cleanup(func() { installURL = prev })
}

func TestEnsure_DownloadsThenCaches(t *testing.T) {
	body := "fake install yaml body for envoy-gateway test\n"
	var hits int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&hits, 1)
		if !strings.Contains(r.URL.Path, "v1.7.2") {
			t.Errorf("URL path missing version: %q", r.URL.Path)
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(body))
	}))
	defer srv.Close()
	withInstallURL(t, srv.URL+"/%s/install.yaml")

	cacheDir := t.TempDir()
	ctx := context.Background()

	// First call: downloads.
	path, err := Ensure(ctx, EnsureOptions{
		Version:       "v1.7.2",
		CacheOverride: cacheDir,
	})
	if err != nil {
		t.Fatal(err)
	}
	wantPath := filepath.Join(cacheDir, "envoygateway", "v1.7.2", "install.yaml")
	if path != wantPath {
		t.Fatalf("path: %q want %q", path, wantPath)
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != body {
		t.Fatalf("body: %q", got)
	}
	if hits != 1 {
		t.Fatalf("hits after first call: %d, want 1", hits)
	}

	// Second call: cached, no network.
	path2, err := Ensure(ctx, EnsureOptions{
		Version:       "v1.7.2",
		CacheOverride: cacheDir,
	})
	if err != nil {
		t.Fatal(err)
	}
	if path2 != path {
		t.Fatalf("second-call path differs: %q vs %q", path2, path)
	}
	if hits != 1 {
		t.Fatalf("hits after second call: %d, want still 1 (cached)", hits)
	}
}

// TestEnsure_DefaultsToVersionConstant verifies that an empty
// EnsureOptions.Version routes to the package's pinned Version.
func TestEnsure_DefaultsToVersionConstant(t *testing.T) {
	var seenPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seenPath = r.URL.Path
		_, _ = w.Write([]byte("ok"))
	}))
	defer srv.Close()
	withInstallURL(t, srv.URL+"/%s/install.yaml")

	if _, err := Ensure(context.Background(), EnsureOptions{
		CacheOverride: t.TempDir(),
	}); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(seenPath, Version) {
		t.Fatalf("expected URL to include pinned Version %q, got %q", Version, seenPath)
	}
}

// TestEnsure_DownloadFailureLeavesNoStaleFile -- a 500 must NOT
// leave a partial install.yaml on disk; otherwise the next
// (perhaps successful) provision would treat the corrupt
// remnant as cached and skip the re-download. The atomic
// .tmp+rename in download() is what protects this.
func TestEnsure_DownloadFailureLeavesNoStaleFile(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "boom", http.StatusInternalServerError)
	}))
	defer srv.Close()
	withInstallURL(t, srv.URL+"/%s/install.yaml")

	cacheDir := t.TempDir()
	_, err := Ensure(context.Background(), EnsureOptions{
		Version:       "v1.7.2",
		CacheOverride: cacheDir,
	})
	if err == nil {
		t.Fatal("expected download error")
	}
	dst := filepath.Join(cacheDir, "envoygateway", "v1.7.2", "install.yaml")
	if _, err := os.Stat(dst); err == nil {
		t.Fatalf("install.yaml should not exist after failed download: %s", dst)
	}
}
