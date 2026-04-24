package serve

import (
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func seedYKBases(t *testing.T, root string, files map[string]string) {
	t.Helper()
	for rel, body := range files {
		abs := filepath.Join(root, rel)
		if err := os.MkdirAll(filepath.Dir(abs), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(abs, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

func cfgWithSources(t *testing.T, sources ...string) *Config {
	t.Helper()
	out := &Config{
		Dir:  t.TempDir(),
		Port: 1,
		Type: TypeYKustomizeLocal,
	}
	for _, s := range sources {
		out.Sources = append(out.Sources, YKustomizeLocalSource{Dir: s})
	}
	return out
}

func TestYK_SingleSource(t *testing.T) {
	src := t.TempDir()
	seedYKBases(t, src, map[string]string{
		"y-kustomize-bases/blobs/setup-bucket-job/base-for-annotations.yaml": "kind: Job\n",
		"y-kustomize-bases/blobs/setup-bucket-job/values.yaml":               "bucket: builds\n",
	})
	b, err := newYKustomizeLocalBackend(cfgWithSources(t, src))
	if err != nil {
		t.Fatal(err)
	}
	got := b.Routes()
	want := []string{
		"/v1/blobs/setup-bucket-job/base-for-annotations.yaml",
		"/v1/blobs/setup-bucket-job/values.yaml",
	}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("got %v want %v", got, want)
	}
}

func TestYK_TwoSourcesMerge(t *testing.T) {
	a := t.TempDir()
	b := t.TempDir()
	seedYKBases(t, a, map[string]string{
		"y-kustomize-bases/blobs/setup-bucket-job/base-for-annotations.yaml": "A\n",
	})
	seedYKBases(t, b, map[string]string{
		"y-kustomize-bases/kafka/setup-topic-job/base-for-annotations.yaml": "B\n",
	})
	back, err := newYKustomizeLocalBackend(cfgWithSources(t, a, b))
	if err != nil {
		t.Fatal(err)
	}
	if len(back.Routes()) != 2 {
		t.Fatalf("routes: %v", back.Routes())
	}
}

func TestYK_DuplicateAcrossSources(t *testing.T) {
	a := t.TempDir()
	b := t.TempDir()
	for _, d := range []string{a, b} {
		seedYKBases(t, d, map[string]string{
			"y-kustomize-bases/blobs/setup-bucket-job/x.yaml": "k\n",
		})
	}
	_, err := newYKustomizeLocalBackend(cfgWithSources(t, a, b))
	if err == nil || !strings.Contains(err.Error(), "duplicate route") {
		t.Fatalf("want duplicate error, got %v", err)
	}
	if !strings.Contains(err.Error(), a) || !strings.Contains(err.Error(), b) {
		t.Fatalf("dup error should mention both sources: %v", err)
	}
}

func TestYK_MissingBasesDir(t *testing.T) {
	src := t.TempDir() // no y-kustomize-bases/
	_, err := newYKustomizeLocalBackend(cfgWithSources(t, src))
	if err == nil || !strings.Contains(err.Error(), "missing") {
		t.Fatalf("want missing error, got %v", err)
	}
}

func TestYK_SourceIsFile(t *testing.T) {
	f := filepath.Join(t.TempDir(), "file")
	os.WriteFile(f, []byte("x"), 0o644)
	_, err := newYKustomizeLocalBackend(cfgWithSources(t, f))
	if err == nil || !strings.Contains(err.Error(), "not a directory") {
		t.Fatalf("want not-a-directory error, got %v", err)
	}
}

func TestYK_BasesIsFile(t *testing.T) {
	src := t.TempDir()
	os.WriteFile(filepath.Join(src, "y-kustomize-bases"), []byte("x"), 0o644)
	_, err := newYKustomizeLocalBackend(cfgWithSources(t, src))
	if err == nil || !strings.Contains(err.Error(), "not a directory") {
		t.Fatalf("want not-a-directory error, got %v", err)
	}
}

func TestYK_NonFileLeavesIgnored(t *testing.T) {
	src := t.TempDir()
	seedYKBases(t, src, map[string]string{
		"y-kustomize-bases/blobs/setup-bucket-job/values.yaml":       "k\n",
		"y-kustomize-bases/blobs/setup-bucket-job/subdir/ignored.yaml": "k\n",
	})
	b, err := newYKustomizeLocalBackend(cfgWithSources(t, src))
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range b.Routes() {
		if strings.Contains(p, "/subdir/") {
			t.Fatalf("subdir leaked into route: %s", p)
		}
	}
}

func TestYK_ServeHTTP_200And304(t *testing.T) {
	src := t.TempDir()
	seedYKBases(t, src, map[string]string{
		"y-kustomize-bases/blobs/setup-bucket-job/values.yaml": "bucket: builds\n",
	})
	b, err := newYKustomizeLocalBackend(cfgWithSources(t, src))
	if err != nil {
		t.Fatal(err)
	}
	s := httptest.NewServer(b)
	defer s.Close()

	resp, err := http.Get(s.URL + "/v1/blobs/setup-bucket-job/values.yaml")
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if string(body) != "bucket: builds\n" {
		t.Fatalf("body %q", body)
	}
	if resp.Header.Get("Content-Type") != yamlMIME {
		t.Fatalf("content-type: %s", resp.Header.Get("Content-Type"))
	}
	etag := resp.Header.Get("ETag")

	req, _ := http.NewRequest(http.MethodGet, s.URL+"/v1/blobs/setup-bucket-job/values.yaml", nil)
	req.Header.Set("If-None-Match", etag)
	resp2, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp2.Body.Close()
	if resp2.StatusCode != http.StatusNotModified {
		t.Fatalf("304 expected, got %d", resp2.StatusCode)
	}
}

func TestYK_ServeHTTP_404AndMethod(t *testing.T) {
	src := t.TempDir()
	seedYKBases(t, src, map[string]string{
		"y-kustomize-bases/blobs/setup-bucket-job/values.yaml": "k\n",
	})
	b, _ := newYKustomizeLocalBackend(cfgWithSources(t, src))
	s := httptest.NewServer(b)
	defer s.Close()

	// Unknown path
	resp, _ := http.Get(s.URL + "/v1/nope")
	resp.Body.Close()
	if resp.StatusCode != 404 {
		t.Fatalf("want 404, got %d", resp.StatusCode)
	}
	// Outside /v1/
	resp, _ = http.Get(s.URL + "/somethingelse")
	resp.Body.Close()
	if resp.StatusCode != 404 {
		t.Fatalf("outside /v1/: want 404, got %d", resp.StatusCode)
	}
	// POST
	resp, _ = http.Post(s.URL+"/v1/blobs/setup-bucket-job/values.yaml", "text/plain", strings.NewReader("x"))
	resp.Body.Close()
	if resp.StatusCode != http.StatusMethodNotAllowed {
		t.Fatalf("POST: %d", resp.StatusCode)
	}
}

func TestYK_ReadFileError(t *testing.T) {
	src := t.TempDir()
	seedYKBases(t, src, map[string]string{
		"y-kustomize-bases/blobs/setup-bucket-job/values.yaml": "k\n",
	})
	b, _ := newYKustomizeLocalBackend(cfgWithSources(t, src))
	// Remove the file after scan
	os.Remove(filepath.Join(src, "y-kustomize-bases/blobs/setup-bucket-job/values.yaml"))
	s := httptest.NewServer(b)
	defer s.Close()
	resp, _ := http.Get(s.URL + "/v1/blobs/setup-bucket-job/values.yaml")
	resp.Body.Close()
	if resp.StatusCode != 500 {
		t.Fatalf("want 500, got %d", resp.StatusCode)
	}
}

func TestYK_WrongType(t *testing.T) {
	c := &Config{Port: 1, Type: TypeStatic, Dir: t.TempDir()}
	if _, err := newYKustomizeLocalBackend(c); err == nil {
		t.Fatal("want error")
	}
}

func TestYK_NoSources(t *testing.T) {
	c := &Config{Port: 1, Type: TypeYKustomizeLocal, Dir: t.TempDir()}
	if _, err := newYKustomizeLocalBackend(c); err == nil {
		t.Fatal("want error")
	}
}
