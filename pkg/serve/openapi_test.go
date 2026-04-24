package serve

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestOpenAPISpec_Render(t *testing.T) {
	s := newOpenAPISpec("y-cluster serve :8944", TypeYKustomizeLocal, "v1.2.3", []specRoute{
		{Path: "/v1/blobs/setup-bucket-job/values.yaml", ContentType: yamlMIME},
		{Path: "/v1/kafka/setup-topic-job/base-for-annotations.yaml", ContentType: yamlMIME},
	})
	got := string(s.Render())
	want := `openapi: 3.1.0
info:
  title: "y-cluster serve :8944"
  x-type: y-kustomize-local
  version: v1.2.3
paths:
  /v1/blobs/setup-bucket-job/values.yaml:
    get:
      responses:
        "200":
          content:
            application/yaml: {}
  /v1/kafka/setup-topic-job/base-for-annotations.yaml:
    get:
      responses:
        "200":
          content:
            application/yaml: {}
`
	if got != want {
		t.Fatalf("mismatch:\n---got---\n%s\n---want---\n%s", got, want)
	}
}

func TestYamlEscape(t *testing.T) {
	cases := []struct{ in, want string }{
		{"plain", "plain"},
		{"", `""`},
		{"a:b", `"a:b"`},
		{"with space", "with space"},
		{" leading", `" leading"`},
		{"trailing ", `"trailing "`},
		{"-starts-dash", `"-starts-dash"`},
		{`quote"here`, `"quote\"here"`},
		{`slash\here`, `slash\here`}, // backslash is legal in plain YAML scalars
	}
	for _, tc := range cases {
		if got := yamlEscape(tc.in); got != tc.want {
			t.Fatalf("escape(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestOpenAPIHandler(t *testing.T) {
	body := []byte("openapi: 3.1.0\n")
	s := httptest.NewServer(OpenAPIHandler(body))
	defer s.Close()

	resp, err := http.Get(s.URL)
	if err != nil {
		t.Fatal(err)
	}
	got, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if string(got) != string(body) {
		t.Fatalf("body: %q", got)
	}
	// HEAD
	req, _ := http.NewRequest(http.MethodHead, s.URL, nil)
	resp2, _ := http.DefaultClient.Do(req)
	resp2.Body.Close()
	if resp2.StatusCode != 200 {
		t.Fatalf("HEAD: %d", resp2.StatusCode)
	}
	// Bad method
	resp3, _ := http.Post(s.URL, "text/plain", strings.NewReader("x"))
	resp3.Body.Close()
	if resp3.StatusCode != http.StatusMethodNotAllowed {
		t.Fatalf("POST: %d", resp3.StatusCode)
	}
}
