package serve

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestDetectContentType_YAML(t *testing.T) {
	for _, ext := range []string{"foo.yaml", "bar.yml", "x.YAML"} {
		if DetectContentType(ext) != yamlMIME {
			t.Fatalf("%s → %s, want %s", ext, DetectContentType(ext), yamlMIME)
		}
	}
}

func TestDetectContentType_Fallback(t *testing.T) {
	if got := DetectContentType("noext"); got != "application/octet-stream" {
		t.Fatalf("fallback: %s", got)
	}
	if got := DetectContentType("x.json"); !strings.HasPrefix(got, "application/json") {
		t.Fatalf("json: %s", got)
	}
}

func TestComputeETag_Deterministic(t *testing.T) {
	a := ComputeETag([]byte("hello"))
	b := ComputeETag([]byte("hello"))
	if a != b {
		t.Fatalf("not deterministic: %s %s", a, b)
	}
	if !strings.HasPrefix(a, `W/"`) {
		t.Fatalf("expected weak ETag, got %s", a)
	}
	if c := ComputeETag([]byte("world")); c == a {
		t.Fatal("different input produced same ETag")
	}
}

func TestWriteAsset_GET(t *testing.T) {
	body := []byte("key: value\n")
	h := func(w http.ResponseWriter, r *http.Request) {
		WriteAsset(w, r, "foo.yaml", body)
	}
	srv := httptest.NewServer(http.HandlerFunc(h))
	defer srv.Close()

	resp, err := http.Get(srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	got, _ := io.ReadAll(resp.Body)
	if string(got) != string(body) {
		t.Fatalf("body: %q", got)
	}
	if resp.Header.Get("ETag") == "" {
		t.Fatal("missing ETag")
	}
	if !strings.Contains(resp.Header.Get("Cache-Control"), "no-cache") {
		t.Fatalf("cache-control: %s", resp.Header.Get("Cache-Control"))
	}
	if ct := resp.Header.Get("Content-Type"); ct != yamlMIME {
		t.Fatalf("content-type: %s", ct)
	}
}

func TestWriteAsset_ConditionalGET(t *testing.T) {
	body := []byte("x")
	h := func(w http.ResponseWriter, r *http.Request) {
		WriteAsset(w, r, "foo.txt", body)
	}
	srv := httptest.NewServer(http.HandlerFunc(h))
	defer srv.Close()

	resp1, err := http.Get(srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	resp1.Body.Close()
	etag := resp1.Header.Get("ETag")

	req, _ := http.NewRequest(http.MethodGet, srv.URL, nil)
	req.Header.Set("If-None-Match", etag)
	resp2, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp2.Body.Close()
	if resp2.StatusCode != http.StatusNotModified {
		t.Fatalf("status: %d", resp2.StatusCode)
	}

	req.Header.Set("If-None-Match", "*")
	resp3, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp3.Body.Close()
	if resp3.StatusCode != http.StatusNotModified {
		t.Fatalf("star: %d", resp3.StatusCode)
	}

	req.Header.Set("If-None-Match", `W/"other", `+etag)
	resp4, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp4.Body.Close()
	if resp4.StatusCode != http.StatusNotModified {
		t.Fatalf("list: %d", resp4.StatusCode)
	}

	req.Header.Set("If-None-Match", `W/"different"`)
	resp5, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp5.Body.Close()
	if resp5.StatusCode != http.StatusOK {
		t.Fatalf("no-match: %d", resp5.StatusCode)
	}
}

func TestWriteAsset_HEAD(t *testing.T) {
	body := []byte("hello")
	h := func(w http.ResponseWriter, r *http.Request) {
		WriteAsset(w, r, "foo.txt", body)
	}
	srv := httptest.NewServer(http.HandlerFunc(h))
	defer srv.Close()

	req, _ := http.NewRequest(http.MethodHead, srv.URL, nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	got, _ := io.ReadAll(resp.Body)
	if len(got) != 0 {
		t.Fatalf("HEAD body: %q", got)
	}
	if resp.Header.Get("Content-Length") != "5" {
		t.Fatalf("content-length: %s", resp.Header.Get("Content-Length"))
	}
	if resp.Header.Get("ETag") == "" {
		t.Fatal("missing ETag on HEAD")
	}
}

func TestMethodNotAllowed(t *testing.T) {
	h := func(w http.ResponseWriter, r *http.Request) {
		MethodNotAllowed(w, http.MethodGet, http.MethodHead)
	}
	srv := httptest.NewServer(http.HandlerFunc(h))
	defer srv.Close()
	req, _ := http.NewRequest(http.MethodPost, srv.URL, nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusMethodNotAllowed {
		t.Fatalf("status: %d", resp.StatusCode)
	}
	if allow := resp.Header.Get("Allow"); !strings.Contains(allow, "GET") {
		t.Fatalf("Allow: %s", allow)
	}
}

func TestMatchesETag_Empty(t *testing.T) {
	if matchesETag("", `W/"x"`) {
		t.Fatal("empty header must not match")
	}
}
