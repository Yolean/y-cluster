package envoygateway

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
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

func sha256Hex(body string) string {
	sum := sha256.Sum256([]byte(body))
	return hex.EncodeToString(sum[:])
}

// cacheFiles lists every file under dir, so that a test can tell
// that a failed Ensure left nothing behind, temporary files included.
func cacheFiles(t *testing.T, dir string) []string {
	t.Helper()
	var files []string
	err := filepath.WalkDir(dir, func(path string, d os.DirEntry, err error) error {
		if err == nil && !d.IsDir() {
			files = append(files, path)
		}
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	return files
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
		InstallSHA256: sha256Hex(body),
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
		InstallSHA256: sha256Hex(body),
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
// EnsureOptions.Version routes to the package's pinned Version, and
// that the download is verified against VersionSHA256: the fake body
// is not the release's, so it is refused.
func TestEnsure_DefaultsToVersionConstant(t *testing.T) {
	var seenPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seenPath = r.URL.Path
		_, _ = w.Write([]byte("ok"))
	}))
	defer srv.Close()
	withInstallURL(t, srv.URL+"/%s/install.yaml")

	cacheDir := t.TempDir()
	_, err := Ensure(context.Background(), EnsureOptions{
		CacheOverride: cacheDir,
	})
	if err == nil || !strings.Contains(err.Error(), "want "+VersionSHA256) {
		t.Fatalf("want a digest mismatch against VersionSHA256, got %v", err)
	}
	if !strings.Contains(seenPath, Version) {
		t.Fatalf("expected URL to include pinned Version %q, got %q", Version, seenPath)
	}
	if files := cacheFiles(t, cacheDir); len(files) > 0 {
		t.Fatalf("a refused download left %v", files)
	}
}

func TestVersionSHA256Format(t *testing.T) {
	if len(VersionSHA256) != 64 || strings.Trim(VersionSHA256, "0123456789abcdef") != "" {
		t.Fatalf("VersionSHA256 %q is not 64 lowercase hex characters", VersionSHA256)
	}
}

// A release other than the constant needs its digest: Ensure refuses
// before any request.
func TestEnsure_OtherVersionNeedsDigest(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("unexpected request %s", r.URL.Path)
	}))
	defer srv.Close()
	withInstallURL(t, srv.URL+"/%s/install.yaml")

	_, err := Ensure(context.Background(), EnsureOptions{Version: "v1.9.2", CacheOverride: t.TempDir()})
	if err == nil || !strings.Contains(err.Error(), "v1.9.2") || !strings.Contains(err.Error(), "sha256") {
		t.Fatalf("want a missing digest error, got %v", err)
	}
}

// A download that fails its digest names both digests and leaves
// nothing in the cache, so the next run downloads again.
func TestEnsure_DigestMismatchLeavesNothing(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("not the release\n"))
	}))
	defer srv.Close()
	withInstallURL(t, srv.URL+"/%s/install.yaml")

	cacheDir := t.TempDir()
	want := sha256Hex("the release\n")
	_, err := Ensure(context.Background(), EnsureOptions{Version: "v1.9.2", InstallSHA256: want, CacheOverride: cacheDir})
	if err == nil || !strings.Contains(err.Error(), "want "+want) || !strings.Contains(err.Error(), sha256Hex("not the release\n")) {
		t.Fatalf("want a mismatch naming both digests, got %v", err)
	}
	if files := cacheFiles(t, cacheDir); len(files) > 0 {
		t.Fatalf("a refused download left %v", files)
	}
}

// A cached file that fails its digest is refused, with what to do,
// and neither replaced nor downloaded again.
func TestEnsure_CachedFileFailsDigest(t *testing.T) {
	var hits int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&hits, 1)
		_, _ = w.Write([]byte("the release\n"))
	}))
	defer srv.Close()
	withInstallURL(t, srv.URL+"/%s/install.yaml")

	cacheDir := t.TempDir()
	opts := EnsureOptions{Version: "v1.9.2", InstallSHA256: sha256Hex("the release\n"), CacheOverride: cacheDir}
	path, err := Ensure(context.Background(), opts)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("replaced\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	_, err = Ensure(context.Background(), opts)
	if err == nil || !strings.Contains(err.Error(), path) || !strings.Contains(err.Error(), "remove the file") {
		t.Fatalf("want a refusal of the cached file, got %v", err)
	}
	if hits != 1 {
		t.Fatalf("%d requests, want 1: a bad cache entry must not be downloaded again", hits)
	}
	if got, _ := os.ReadFile(path); string(got) != "replaced\n" {
		t.Fatalf("the cached file was changed to %q", got)
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
		InstallSHA256: sha256Hex("anything"),
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
