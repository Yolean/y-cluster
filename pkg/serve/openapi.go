package serve

import (
	"bytes"
	"fmt"
	"net/http"
	"strings"
)

// OpenAPISpec is the minimal live spec per port. Generated at start,
// served from /openapi.yaml. Intentionally hand-rolled YAML to avoid a
// new dependency and produce byte-stable output for golden tests.
type openAPISpec struct {
	Title   string
	Type    BackendType
	Version string
	Routes  []specRoute
}

type specRoute struct {
	Path        string
	ContentType string
}

func newOpenAPISpec(title string, typ BackendType, version string, routes []specRoute) openAPISpec {
	return openAPISpec{Title: title, Type: typ, Version: version, Routes: routes}
}

// Render writes the OpenAPI 3.1 YAML to the writer.
func (s openAPISpec) Render() []byte {
	var b bytes.Buffer
	b.WriteString("openapi: 3.1.0\n")
	b.WriteString("info:\n")
	b.WriteString(fmt.Sprintf("  title: %s\n", yamlEscape(s.Title)))
	b.WriteString(fmt.Sprintf("  x-type: %s\n", string(s.Type)))
	b.WriteString(fmt.Sprintf("  version: %s\n", yamlEscape(s.Version)))
	b.WriteString("paths:\n")
	for _, r := range s.Routes {
		b.WriteString(fmt.Sprintf("  %s:\n", yamlEscape(r.Path)))
		b.WriteString("    get:\n")
		b.WriteString("      responses:\n")
		b.WriteString("        \"200\":\n")
		b.WriteString("          content:\n")
		b.WriteString(fmt.Sprintf("            %s: {}\n", yamlEscape(r.ContentType)))
	}
	return b.Bytes()
}

// yamlEscape is enough for our domain: quote if the value contains any
// character that would otherwise start a YAML construct.
func yamlEscape(s string) string {
	if s == "" {
		return `""`
	}
	needsQuote := strings.ContainsAny(s, ":#{}[],&*!|>'\"%@`\n\t") ||
		strings.HasPrefix(s, "-") ||
		strings.HasPrefix(s, "?") ||
		strings.HasPrefix(s, " ") ||
		strings.HasSuffix(s, " ")
	if !needsQuote {
		return s
	}
	// Double-quote and escape " and \
	esc := strings.ReplaceAll(s, `\`, `\\`)
	esc = strings.ReplaceAll(esc, `"`, `\"`)
	return `"` + esc + `"`
}

// OpenAPIHandler serves a pre-rendered spec. The spec is snapshotted at
// backend construction per SERVE_FEATURE.md §"Scope limitation".
func OpenAPIHandler(spec []byte) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			MethodNotAllowed(w, http.MethodGet, http.MethodHead)
			return
		}
		WriteAsset(w, r, "openapi.yaml", spec)
	}
}
