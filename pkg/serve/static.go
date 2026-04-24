package serve

import (
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"

	"go.uber.org/zap"
	"sigs.k8s.io/yaml"
)

// staticBackend serves a directory tree under an optional `root`
// prefix. See SERVE_FEATURE.md §"usecase: Static assets".
type staticBackend struct {
	cfg    *Config
	logger *zap.Logger

	absDir string // fully resolved filesystem root
	root   string // URL prefix, normalized to "/" or "/<segment>[/<segment>]*/"
	routes []specRoute
}

// newStaticBackend resolves the dir (relative to the config's own
// directory), verifies the directory exists, snapshots the file
// layout for the openapi spec, and returns a ready handler.
func newStaticBackend(cfg *Config, logger *zap.Logger) (*staticBackend, error) {
	if cfg.Type != TypeStatic {
		return nil, fmt.Errorf("not a static config: %s", cfg.Type)
	}
	sc := cfg.Static
	if sc == nil {
		return nil, fmt.Errorf("static block missing after validate") // defensive
	}

	absDir := sc.Dir
	if !filepath.IsAbs(absDir) {
		absDir = filepath.Join(cfg.Dir, absDir)
	}
	absDir = filepath.Clean(absDir)
	info, err := os.Stat(absDir)
	if err != nil {
		return nil, fmt.Errorf("static.dir %s: %w", sc.Dir, err)
	}
	if !info.IsDir() {
		return nil, fmt.Errorf("static.dir %s is not a directory", sc.Dir)
	}

	root := normalizeRoot(sc.Root)

	b := &staticBackend{
		cfg:    cfg,
		logger: logger,
		absDir: absDir,
		root:   root,
	}
	b.routes, err = b.scan()
	if err != nil {
		return nil, fmt.Errorf("scan %s: %w", absDir, err)
	}
	return b, nil
}

// normalizeRoot returns "/" if the input is empty or "/", otherwise a
// string that begins and ends with "/" and has no internal "..".
func normalizeRoot(r string) string {
	if r == "" || r == "/" {
		return "/"
	}
	cleaned := path.Clean("/" + strings.TrimSuffix(r, "/"))
	if cleaned == "/" {
		return "/"
	}
	return cleaned + "/"
}

// scan walks absDir and returns one specRoute per file, using the
// detected content type for each. Directories are skipped; symlinks
// are followed by default (Walk's default). Files under `absDir`
// that would land outside the root (only possible with very unusual
// symlinks) are silently skipped.
func (b *staticBackend) scan() ([]specRoute, error) {
	var out []specRoute
	err := filepath.Walk(b.absDir, func(p string, info os.FileInfo, werr error) error {
		if werr != nil {
			return werr
		}
		if info.IsDir() {
			return nil
		}
		rel, err := filepath.Rel(b.absDir, p)
		if err != nil {
			return nil // defensive; Walk should never hand us a path outside absDir
		}
		urlPath := b.root + filepath.ToSlash(rel)
		out = append(out, specRoute{
			Path:        urlPath,
			ContentType: b.contentTypeFor(rel),
		})
		return nil
	})
	if err != nil {
		return nil, err
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Path < out[j].Path })
	return out, nil
}

// contentTypeFor returns the detected content type for an asset, with
// the yamlToJson override applied if the file would be served as
// application/yaml.
func (b *staticBackend) contentTypeFor(rel string) string {
	ct := DetectContentType(rel)
	if b.cfg.Static.YAMLToJSON && ct == yamlMIME {
		return "application/json"
	}
	return ct
}

// specRoutes returns the openapi-ready snapshot.
func (b *staticBackend) specRoutes() []specRoute {
	return b.routes
}

// ServeHTTP implements http.Handler.
func (b *staticBackend) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		MethodNotAllowed(w, http.MethodGet, http.MethodHead)
		return
	}

	urlPath := r.URL.Path
	// Gate everything by the configured root.
	if !strings.HasPrefix(urlPath, b.root) {
		http.NotFound(w, r)
		return
	}

	rel := strings.TrimPrefix(urlPath, b.root)
	// Clean against path traversal; if the cleaned rel tries to
	// escape, refuse.
	if rel != "" {
		cleaned := path.Clean("/" + rel)
		if !strings.HasPrefix(cleaned, "/") {
			http.NotFound(w, r)
			return
		}
		rel = strings.TrimPrefix(cleaned, "/")
	}

	fsPath := filepath.Join(b.absDir, filepath.FromSlash(rel))
	// Belt and braces: ensure the join stayed inside absDir.
	absPath, err := filepath.Abs(fsPath)
	if err != nil {
		http.Error(w, "resolve: "+err.Error(), http.StatusInternalServerError)
		return
	}
	if !strings.HasPrefix(absPath, b.absDir) {
		http.NotFound(w, r)
		return
	}

	info, err := os.Stat(absPath)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	if info.IsDir() {
		b.handleDir(w, r, urlPath)
		return
	}

	body, err := os.ReadFile(absPath)
	if err != nil {
		http.Error(w, "read: "+err.Error(), http.StatusInternalServerError)
		return
	}

	filename := filepath.Base(absPath)
	if b.cfg.Static.YAMLToJSON && DetectContentType(filename) == yamlMIME {
		jsonBody, err := yamlToMinifiedJSON(body)
		if err != nil {
			b.logger.Error("yamlToJson transform failed",
				zap.String("path", urlPath),
				zap.Error(err),
			)
			http.Error(w, "yamlToJson: "+err.Error(), http.StatusInternalServerError)
			return
		}
		WriteAssetAs(w, r, jsonBody, "application/json")
		return
	}

	WriteAsset(w, r, filename, body)
}

// handleDir implements the dirTrailingSlash policy. A bare directory
// path always ends in 404 (no listing); the redirect mode adds a
// 302 hop when the trailing slash is missing.
func (b *staticBackend) handleDir(w http.ResponseWriter, r *http.Request, urlPath string) {
	if b.cfg.Static.DirTrailingSlash == "redirect" && !strings.HasSuffix(urlPath, "/") {
		target := urlPath + "/"
		if r.URL.RawQuery != "" {
			target += "?" + r.URL.RawQuery
		}
		// Fragments are client-side-only; HTTP servers never see them.
		// Preserve via the browser's own redirect handling.
		http.Redirect(w, r, target, http.StatusFound)
		return
	}
	http.NotFound(w, r)
}

// yamlToMinifiedJSON converts a YAML byte slice to minified JSON.
// sigs.k8s.io/yaml already round-trips via JSON internally, so the
// output is canonical JSON with no extra whitespace -- that matches
// the "minify the json" requirement in SERVE_FEATURE.md.
func yamlToMinifiedJSON(src []byte) ([]byte, error) {
	j, err := yaml.YAMLToJSON(src)
	if err != nil {
		return nil, err
	}
	// YAMLToJSON emits compact JSON already, but re-marshalling
	// normalizes whitespace in case upstream ever adds any.
	var v any
	if err := json.Unmarshal(j, &v); err != nil {
		return nil, err
	}
	return json.Marshal(v)
}
