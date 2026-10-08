package cache

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// leftovers lists what besides want is in dir: a finished or failed
// download must not leave temporary files behind.
func leftovers(t *testing.T, dir string, want ...string) []string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	expected := map[string]bool{}
	for _, w := range want {
		expected[w] = true
	}
	var extra []string
	for _, e := range entries {
		if !expected[e.Name()] {
			extra = append(extra, e.Name())
		}
	}
	return extra
}

func TestDownload_WritesBodyAndNothingElse(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("payload\n"))
	}))
	defer srv.Close()

	dir := t.TempDir()
	dest := filepath.Join(dir, "out.bin")
	if err := Download(context.Background(), srv.URL+"/ok", dest); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(dest)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "payload\n" {
		t.Errorf("body: %q", got)
	}
	if extra := leftovers(t, dir, "out.bin"); len(extra) > 0 {
		t.Errorf("temporary files left behind: %v", extra)
	}
	info, err := os.Stat(dest)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o644 {
		t.Errorf("mode = %o, want 644", info.Mode().Perm())
	}
}

func TestDownload_Non200LeavesNoFile(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(http.NotFound))
	defer srv.Close()

	dir := t.TempDir()
	err := Download(context.Background(), srv.URL+"/x", filepath.Join(dir, "out.bin"))
	if err == nil || !strings.Contains(err.Error(), "404") {
		t.Fatalf("want 404 error, got %v", err)
	}
	if extra := leftovers(t, dir); len(extra) > 0 {
		t.Errorf("files left behind: %v", extra)
	}
}

// A transfer that breaks off mid-body is the case the temporary file
// exists for: dest must not appear, or the next run would take the
// truncated file for a cache hit.
func TestDownload_TruncatedBodyLeavesNoFile(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Length", "1000")
		_, _ = w.Write([]byte("only the beginning"))
	}))
	defer srv.Close()

	dir := t.TempDir()
	err := Download(context.Background(), srv.URL+"/big", filepath.Join(dir, "out.bin"))
	if err == nil {
		t.Fatal("want an error for a body shorter than its Content-Length")
	}
	if extra := leftovers(t, dir); len(extra) > 0 {
		t.Errorf("files left behind: %v", extra)
	}
}

func TestDownload_ReplacesExistingFile(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("new"))
	}))
	defer srv.Close()

	dest := filepath.Join(t.TempDir(), "out.bin")
	if err := os.WriteFile(dest, []byte("old and longer"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := Download(context.Background(), srv.URL, dest); err != nil {
		t.Fatal(err)
	}
	got, _ := os.ReadFile(dest)
	if string(got) != "new" {
		t.Errorf("body: %q", got)
	}
}

func TestDownloadSHA256_AcceptsMatchingDigest(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("payload\n"))
	}))
	defer srv.Close()

	dir := t.TempDir()
	dest := filepath.Join(dir, "artifact")
	// sha256 of "payload\n"
	want := "d4e4877bac978b7952f0d544fc52ebff5411d351d129f1f056fa43f11da9af2b"
	if err := DownloadSHA256(context.Background(), srv.URL, dest, strings.ToUpper(want)); err != nil {
		t.Fatal(err)
	}
	if got, _ := os.ReadFile(dest); string(got) != "payload\n" {
		t.Fatalf("dest = %q", got)
	}
	if extra := leftovers(t, dir, "artifact"); len(extra) > 0 {
		t.Fatalf("left behind: %v", extra)
	}
}

func TestDownloadSHA256_MismatchLeavesNoFile(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("tampered\n"))
	}))
	defer srv.Close()

	dir := t.TempDir()
	dest := filepath.Join(dir, "artifact")
	err := DownloadSHA256(context.Background(), srv.URL, dest, "d4e4877bac978b7952f0d544fc52ebff5411d351d129f1f056fa43f11da9af2b")
	if err == nil || !strings.Contains(err.Error(), "SHA-256 is") {
		t.Fatalf("want a digest mismatch error, got %v", err)
	}
	if extra := leftovers(t, dir); len(extra) > 0 {
		t.Fatalf("a mismatched download must leave nothing: %v", extra)
	}
}

func TestDownloadSHA256_RejectsMalformedDigest(t *testing.T) {
	if err := DownloadSHA256(context.Background(), "http://127.0.0.1:1/x", filepath.Join(t.TempDir(), "x"), "abc"); err == nil {
		t.Fatal("a short digest must be refused before any request")
	}
}
