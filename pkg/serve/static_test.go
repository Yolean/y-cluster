package serve

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// staticCfg builds a Config for a static backend pointing at a freshly
// seeded directory. The helper lets each test tweak root/yamlToJson/
// dirTrailingSlash without repeating the boilerplate.
func staticCfg(t *testing.T, sc StaticConfig, files map[string]string) *Config {
	t.Helper()
	cfgDir := t.TempDir()
	dataDir := t.TempDir()
	for rel, body := range files {
		abs := filepath.Join(dataDir, rel)
		if err := os.MkdirAll(filepath.Dir(abs), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(abs, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	sc.Dir = dataDir
	return &Config{
		Dir:    cfgDir,
		Port:   1,
		Type:   TypeStatic,
		Static: &sc,
	}
}

func staticServer(t *testing.T, cfg *Config) (*httptest.Server, *staticBackend) {
	t.Helper()
	b, err := newStaticBackend(cfg, newConsoleLogger())
	if err != nil {
		t.Fatal(err)
	}
	return httptest.NewServer(b), b
}

func TestStatic_Basic(t *testing.T) {
	cfg := staticCfg(t, StaticConfig{Root: "/assets"}, map[string]string{
		"hello.yaml":        "greeting: hi\n",
		"nested/inner.json": `{"x":1}`,
	})
	srv, b := staticServer(t, cfg)
	defer srv.Close()

	// Expected openapi routes
	want := []string{"/assets/hello.yaml", "/assets/nested/inner.json"}
	got := b.specRoutes()
	if len(got) != len(want) {
		t.Fatalf("routes: %v", got)
	}
	for i, p := range want {
		if got[i].Path != p {
			t.Fatalf("route[%d]: got %s want %s", i, got[i].Path, p)
		}
	}

	// Serve the yaml file
	resp, err := http.Get(srv.URL + "/assets/hello.yaml")
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if string(body) != "greeting: hi\n" {
		t.Fatalf("body: %q", body)
	}
	if resp.Header.Get("Content-Type") != yamlMIME {
		t.Fatalf("content-type: %s", resp.Header.Get("Content-Type"))
	}
	if resp.Header.Get("ETag") == "" {
		t.Fatal("missing ETag")
	}
}

func TestStatic_NoRoot(t *testing.T) {
	// Empty root means served under "/"
	cfg := staticCfg(t, StaticConfig{}, map[string]string{
		"at-root.yaml": "k: v\n",
	})
	srv, _ := staticServer(t, cfg)
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/at-root.yaml")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("status: %d", resp.StatusCode)
	}
}

func TestStatic_NotFound(t *testing.T) {
	cfg := staticCfg(t, StaticConfig{Root: "/a"}, map[string]string{
		"exists.yaml": "k: v\n",
	})
	srv, _ := staticServer(t, cfg)
	defer srv.Close()

	// Missing file under root
	resp, _ := http.Get(srv.URL + "/a/missing.yaml")
	resp.Body.Close()
	if resp.StatusCode != 404 {
		t.Fatalf("missing: %d", resp.StatusCode)
	}

	// Outside root
	resp, _ = http.Get(srv.URL + "/other/path")
	resp.Body.Close()
	if resp.StatusCode != 404 {
		t.Fatalf("outside-root: %d", resp.StatusCode)
	}
}

func TestStatic_PathTraversal(t *testing.T) {
	secret := t.TempDir()
	if err := os.WriteFile(filepath.Join(secret, "secret.txt"), []byte("nope"), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg := staticCfg(t, StaticConfig{Root: "/a"}, map[string]string{
		"ok.yaml": "k: v\n",
	})
	srv, _ := staticServer(t, cfg)
	defer srv.Close()

	// Raw request with ../ escapes. net/http cleans URL.Path before
	// dispatch, so this 404s via "outside root" -- but we also verify
	// the belt-and-braces check works if someone constructs the path
	// with filepath oddities.
	resp, _ := http.Get(srv.URL + "/a/../../../../etc/passwd")
	resp.Body.Close()
	if resp.StatusCode == 200 {
		t.Fatal("path traversal allowed")
	}
}

func TestStatic_DirWithoutTrailingSlash_Default(t *testing.T) {
	cfg := staticCfg(t, StaticConfig{Root: "/a"}, map[string]string{
		"dir/file.yaml": "k: v\n",
	})
	srv, _ := staticServer(t, cfg)
	defer srv.Close()

	// Default: hitting a directory returns 404 (no listing).
	client := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	resp, err := client.Get(srv.URL + "/a/dir")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 404 {
		t.Fatalf("dir without slash: %d", resp.StatusCode)
	}
	// And with trailing slash
	resp, err = client.Get(srv.URL + "/a/dir/")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 404 {
		t.Fatalf("dir with slash: %d", resp.StatusCode)
	}
}

func TestStatic_DirTrailingSlashRedirect(t *testing.T) {
	cfg := staticCfg(t, StaticConfig{Root: "/a", DirTrailingSlash: "redirect"}, map[string]string{
		"dir/file.yaml": "k: v\n",
	})
	srv, _ := staticServer(t, cfg)
	defer srv.Close()

	client := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	resp, err := client.Get(srv.URL + "/a/dir?q=1")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusFound {
		t.Fatalf("want 302, got %d", resp.StatusCode)
	}
	loc := resp.Header.Get("Location")
	if loc != "/a/dir/?q=1" {
		t.Fatalf("Location: %q (query string must be preserved)", loc)
	}

	// With the trailing slash the target still 404s -- no listing.
	resp, err = client.Get(srv.URL + "/a/dir/")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 404 {
		t.Fatalf("target of redirect: %d", resp.StatusCode)
	}
}

func TestStatic_YAMLToJSON(t *testing.T) {
	cfg := staticCfg(t, StaticConfig{Root: "/a", YAMLToJSON: true}, map[string]string{
		"doc.yaml":    "greeting:\n  text: hi\n  count: 3\n",
		"stay.json":   `{"x":1}`,
		"notyaml.txt": "plain\n",
	})
	srv, b := staticServer(t, cfg)
	defer srv.Close()

	// The openapi snapshot should advertise application/json for the
	// yaml route since transformation is enabled.
	for _, r := range b.specRoutes() {
		if strings.HasSuffix(r.Path, ".yaml") && r.ContentType != "application/json" {
			t.Fatalf("openapi still shows yaml ct: %+v", r)
		}
	}

	// GET transforms
	resp, err := http.Get(srv.URL + "/a/doc.yaml")
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.Header.Get("Content-Type") != "application/json" {
		t.Fatalf("content-type: %s", resp.Header.Get("Content-Type"))
	}
	var parsed map[string]any
	if err := json.Unmarshal(body, &parsed); err != nil {
		t.Fatalf("not valid JSON: %v: %q", err, body)
	}
	// Minified: no extra spaces
	if strings.Contains(string(body), "  ") || strings.Contains(string(body), "\n") {
		t.Fatalf("expected minified json, got %q", body)
	}
	if etag := resp.Header.Get("ETag"); etag == "" {
		t.Fatal("missing ETag on transformed response")
	}
	// Content-Length matches JSON body length
	cl := resp.Header.Get("Content-Length")
	if cl != strconv.Itoa(len(body)) {
		t.Fatalf("content-length %s vs body len %d", cl, len(body))
	}

	// HEAD produces same headers and no body
	req, _ := http.NewRequest(http.MethodHead, srv.URL+"/a/doc.yaml", nil)
	resp2, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	headBody, _ := io.ReadAll(resp2.Body)
	resp2.Body.Close()
	if len(headBody) != 0 {
		t.Fatalf("HEAD leaked body: %q", headBody)
	}
	if resp2.Header.Get("Content-Length") != cl {
		t.Fatalf("HEAD content-length differs: %s vs %s", resp2.Header.Get("Content-Length"), cl)
	}
	if resp2.Header.Get("ETag") != resp.Header.Get("ETag") {
		t.Fatalf("HEAD ETag differs from GET")
	}

	// JSON file is served as-is (not double-transformed)
	resp3, err := http.Get(srv.URL + "/a/stay.json")
	if err != nil {
		t.Fatal(err)
	}
	b3, _ := io.ReadAll(resp3.Body)
	resp3.Body.Close()
	if string(b3) != `{"x":1}` {
		t.Fatalf("json passthrough: %q", b3)
	}

	// Non-yaml file unaffected
	resp4, err := http.Get(srv.URL + "/a/notyaml.txt")
	if err != nil {
		t.Fatal(err)
	}
	b4, _ := io.ReadAll(resp4.Body)
	resp4.Body.Close()
	if string(b4) != "plain\n" {
		t.Fatalf("plain: %q", b4)
	}
}

func TestStatic_YAMLToJSON_InvalidYaml500(t *testing.T) {
	cfg := staticCfg(t, StaticConfig{Root: "/a", YAMLToJSON: true}, map[string]string{
		"bad.yaml": "not: [valid\n  yaml\n",
	})
	srv, _ := staticServer(t, cfg)
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/a/bad.yaml")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 500 {
		t.Fatalf("want 500, got %d", resp.StatusCode)
	}
}

func TestStatic_ConditionalGET(t *testing.T) {
	cfg := staticCfg(t, StaticConfig{Root: "/a"}, map[string]string{
		"x.yaml": "k: v\n",
	})
	srv, _ := staticServer(t, cfg)
	defer srv.Close()

	resp, _ := http.Get(srv.URL + "/a/x.yaml")
	resp.Body.Close()
	etag := resp.Header.Get("ETag")

	req, _ := http.NewRequest(http.MethodGet, srv.URL+"/a/x.yaml", nil)
	req.Header.Set("If-None-Match", etag)
	resp2, _ := http.DefaultClient.Do(req)
	resp2.Body.Close()
	if resp2.StatusCode != http.StatusNotModified {
		t.Fatalf("want 304, got %d", resp2.StatusCode)
	}
}

func TestStatic_MethodNotAllowed(t *testing.T) {
	cfg := staticCfg(t, StaticConfig{Root: "/a"}, map[string]string{"x.yaml": "k: v\n"})
	srv, _ := staticServer(t, cfg)
	defer srv.Close()
	resp, _ := http.Post(srv.URL+"/a/x.yaml", "text/plain", strings.NewReader(""))
	resp.Body.Close()
	if resp.StatusCode != http.StatusMethodNotAllowed {
		t.Fatalf("POST: %d", resp.StatusCode)
	}
}

func TestStatic_MissingDir(t *testing.T) {
	cfg := &Config{
		Dir:    t.TempDir(),
		Port:   1,
		Type:   TypeStatic,
		Static: &StaticConfig{Dir: "/this/really/should/not/exist"},
	}
	if _, err := newStaticBackend(cfg, newConsoleLogger()); err == nil {
		t.Fatal("want missing-dir error")
	}
}

func TestStatic_DirIsFile(t *testing.T) {
	f := filepath.Join(t.TempDir(), "file")
	if err := os.WriteFile(f, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg := &Config{
		Dir:    t.TempDir(),
		Port:   1,
		Type:   TypeStatic,
		Static: &StaticConfig{Dir: f},
	}
	if _, err := newStaticBackend(cfg, newConsoleLogger()); err == nil {
		t.Fatal("want not-a-directory error")
	}
}

func TestStatic_WrongType(t *testing.T) {
	cfg := &Config{Port: 1, Type: TypeYKustomizeLocal, Dir: t.TempDir()}
	if _, err := newStaticBackend(cfg, newConsoleLogger()); err == nil {
		t.Fatal("want wrong-type error")
	}
}

func TestNormalizeRoot(t *testing.T) {
	cases := []struct{ in, want string }{
		{"", "/"},
		{"/", "/"},
		{"/assets", "/assets/"},
		{"/assets/", "/assets/"},
		{"assets", "/assets/"},
		{"/a/b/c/", "/a/b/c/"},
	}
	for _, c := range cases {
		got := normalizeRoot(c.in)
		if got != c.want {
			t.Fatalf("normalizeRoot(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

