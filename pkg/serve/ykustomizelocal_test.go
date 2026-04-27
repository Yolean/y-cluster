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

// writeBase writes a tiny kustomize source dir that emits a
// single Secret named `y-kustomize.{group}.{name}` with the
// given data files. files maps basename -> body. Returns the
// source dir path.
func writeBase(t *testing.T, group, name string, files map[string]string) string {
	t.Helper()
	dir := t.TempDir()
	subdir := group + "-" + name
	if err := os.MkdirAll(filepath.Join(dir, subdir), 0o755); err != nil {
		t.Fatal(err)
	}
	var fileLines []string
	for fname, body := range files {
		full := filepath.Join(dir, subdir, fname)
		if err := os.WriteFile(full, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
		fileLines = append(fileLines, "  - "+subdir+"/"+fname)
	}
	kust := "apiVersion: kustomize.config.k8s.io/v1beta1\nkind: Kustomization\nsecretGenerator:\n" +
		"- name: y-kustomize." + group + "." + name + "\n" +
		"  options:\n    disableNameSuffixHash: true\n" +
		"  files:\n" + strings.Join(fileLines, "\n") + "\n"
	if err := os.WriteFile(filepath.Join(dir, "kustomization.yaml"), []byte(kust), 0o644); err != nil {
		t.Fatal(err)
	}
	return dir
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

func TestYK_SingleSource_RoutesFromSecretDataKeys(t *testing.T) {
	src := writeBase(t, "blobs", "setup-bucket-job", map[string]string{
		"base-for-annotations.yaml": "kind: Job\n",
		"values.yaml":               "bucket: builds\n",
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
	a := writeBase(t, "blobs", "setup-bucket-job", map[string]string{"x.yaml": "A\n"})
	b := writeBase(t, "kafka", "setup-topic-job", map[string]string{"x.yaml": "B\n"})
	back, err := newYKustomizeLocalBackend(cfgWithSources(t, a, b))
	if err != nil {
		t.Fatal(err)
	}
	if len(back.Routes()) != 2 {
		t.Fatalf("routes: %v", back.Routes())
	}
}

func TestYK_DuplicateRouteAcrossSourcesErrors(t *testing.T) {
	a := writeBase(t, "blobs", "setup-bucket-job", map[string]string{"x.yaml": "A\n"})
	b := writeBase(t, "blobs", "setup-bucket-job", map[string]string{"x.yaml": "B\n"})
	_, err := newYKustomizeLocalBackend(cfgWithSources(t, a, b))
	if err == nil || !strings.Contains(err.Error(), "duplicate route") {
		t.Fatalf("want duplicate error, got %v", err)
	}
	if !strings.Contains(err.Error(), a) || !strings.Contains(err.Error(), b) {
		t.Fatalf("dup error should mention both sources: %v", err)
	}
}

// TestYK_RejectsKustomizationYAMLDataKey covers the Y_CLUSTER_SERVE
// change-request rule: a data key named exactly "kustomization.yaml"
// must fail-fast with a clear message naming the offending key,
// because consumers might assume http://serve/v1/.../kustomization.yaml
// is itself a kustomize base (it isn't -- HTTP kustomize resources
// can't be a directory or another kustomization).
func TestYK_RejectsKustomizationYAMLDataKey(t *testing.T) {
	src := writeBase(t, "blobs", "setup-bucket-job", map[string]string{
		"kustomization.yaml": "resources: []\n",
	})
	_, err := newYKustomizeLocalBackend(cfgWithSources(t, src))
	if err == nil {
		t.Fatal("expected fail-fast error for kustomization.yaml data key")
	}
	if !strings.Contains(err.Error(), "kustomization.yaml") {
		t.Fatalf("error should name the offending key: %v", err)
	}
	if !strings.Contains(err.Error(), "reserved") {
		t.Fatalf("error should explain why: %v", err)
	}
}

func TestYK_NonSecretResourcesIgnored(t *testing.T) {
	// A Kustomization that emits a Secret AND a regular ConfigMap
	// resource. The ConfigMap must not produce routes.
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "src"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "src", "x.yaml"), []byte("k\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "cm.yaml"), []byte(`apiVersion: v1
kind: ConfigMap
metadata:
  name: unrelated
data:
  foo: bar
`), 0o644); err != nil {
		t.Fatal(err)
	}
	kust := `apiVersion: kustomize.config.k8s.io/v1beta1
kind: Kustomization
resources:
- cm.yaml
secretGenerator:
- name: y-kustomize.blobs.setup-bucket-job
  options:
    disableNameSuffixHash: true
  files:
  - src/x.yaml
`
	if err := os.WriteFile(filepath.Join(dir, "kustomization.yaml"), []byte(kust), 0o644); err != nil {
		t.Fatal(err)
	}
	b, err := newYKustomizeLocalBackend(cfgWithSources(t, dir))
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"/v1/blobs/setup-bucket-job/x.yaml"}
	if got := strings.Join(b.Routes(), ","); got != strings.Join(want, ",") {
		t.Fatalf("got %v want %v", b.Routes(), want)
	}
}

func TestYK_SecretsWithNonPrefixedNamesIgnored(t *testing.T) {
	// A Secret without the y-kustomize. prefix must be ignored
	// (the prefix is the contract; arbitrary Secrets that
	// happen to be in the build output are not turned into URLs).
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "src"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "src", "y.yaml"), []byte("k\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	kust := `apiVersion: kustomize.config.k8s.io/v1beta1
kind: Kustomization
secretGenerator:
- name: random-secret
  options:
    disableNameSuffixHash: true
  files:
  - src/y.yaml
- name: y-kustomize.blobs.x
  options:
    disableNameSuffixHash: true
  files:
  - src/y.yaml
`
	if err := os.WriteFile(filepath.Join(dir, "kustomization.yaml"), []byte(kust), 0o644); err != nil {
		t.Fatal(err)
	}
	b, err := newYKustomizeLocalBackend(cfgWithSources(t, dir))
	if err != nil {
		t.Fatal(err)
	}
	if len(b.Routes()) != 1 || b.Routes()[0] != "/v1/blobs/x/y.yaml" {
		t.Fatalf("got %v", b.Routes())
	}
}

func TestYK_ServeHTTP_200And304(t *testing.T) {
	src := writeBase(t, "blobs", "setup-bucket-job", map[string]string{
		"values.yaml": "bucket: builds\n",
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
	if etag == "" {
		t.Fatal("missing ETag")
	}

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
	src := writeBase(t, "blobs", "setup-bucket-job", map[string]string{
		"values.yaml": "k\n",
	})
	b, _ := newYKustomizeLocalBackend(cfgWithSources(t, src))
	s := httptest.NewServer(b)
	defer s.Close()

	resp, _ := http.Get(s.URL + "/v1/nope")
	resp.Body.Close()
	if resp.StatusCode != 404 {
		t.Fatalf("want 404, got %d", resp.StatusCode)
	}
	resp, _ = http.Get(s.URL + "/somethingelse")
	resp.Body.Close()
	if resp.StatusCode != 404 {
		t.Fatalf("outside /v1/: want 404, got %d", resp.StatusCode)
	}
	resp, _ = http.Post(s.URL+"/v1/blobs/setup-bucket-job/values.yaml", "text/plain", strings.NewReader("x"))
	resp.Body.Close()
	if resp.StatusCode != http.StatusMethodNotAllowed {
		t.Fatalf("POST: %d", resp.StatusCode)
	}
}

func TestYK_KustomizeBuildErrorPropagates(t *testing.T) {
	src := t.TempDir() // no kustomization.yaml
	_, err := newYKustomizeLocalBackend(cfgWithSources(t, src))
	if err == nil {
		t.Fatal("expected kustomize build error")
	}
	if !strings.Contains(err.Error(), "kustomize build") {
		t.Fatalf("error should be wrapped: %v", err)
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
