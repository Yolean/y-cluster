package serve

import (
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// yKustomizeBasesDir is the conventional subdirectory in a source.
const yKustomizeBasesDir = "y-kustomize-bases"

// ykRoute is a resolved path → file mapping with metadata used to emit
// the openapi spec.
type ykRoute struct {
	Path        string // e.g. /v1/blobs/setup-bucket-job/base-for-annotations.yaml
	FilePath    string // absolute filesystem path
	ContentType string // detected at scan time, used by openapi
}

// ykBackend serves a frozen map of /v1 routes from scanned sources.
type ykBackend struct {
	cfg    *Config
	routes map[string]ykRoute
	order  []string // sorted paths, for openapi stability
}

// newYKustomizeLocalBackend scans every source dir and builds a route
// table. Duplicate routes across sources are a fatal error with both
// source paths in the message.
func newYKustomizeLocalBackend(cfg *Config) (*ykBackend, error) {
	if cfg.Type != TypeYKustomizeLocal {
		return nil, fmt.Errorf("not a y-kustomize-local config: %s", cfg.Type)
	}
	sources := cfg.ResolvedSources()
	if len(sources) == 0 {
		return nil, fmt.Errorf("no sources")
	}

	routes := map[string]ykRoute{}
	origin := map[string]string{} // route → source dir (for dup error)

	for _, src := range sources {
		info, err := os.Stat(src)
		if err != nil {
			return nil, fmt.Errorf("source %s: %w", src, err)
		}
		if !info.IsDir() {
			return nil, fmt.Errorf("source %s is not a directory", src)
		}
		basesDir := filepath.Join(src, yKustomizeBasesDir)
		basesInfo, err := os.Stat(basesDir)
		if err != nil {
			return nil, fmt.Errorf("source %s: missing %s/", src, yKustomizeBasesDir)
		}
		if !basesInfo.IsDir() {
			return nil, fmt.Errorf("source %s: %s is not a directory", src, yKustomizeBasesDir)
		}

		scanned, err := scanYKustomizeBases(basesDir)
		if err != nil {
			return nil, fmt.Errorf("scan %s: %w", basesDir, err)
		}
		for _, r := range scanned {
			if prev, dup := origin[r.Path]; dup {
				return nil, fmt.Errorf("duplicate route %s from %s and %s", r.Path, prev, src)
			}
			routes[r.Path] = r
			origin[r.Path] = src
		}
	}

	order := make([]string, 0, len(routes))
	for p := range routes {
		order = append(order, p)
	}
	sort.Strings(order)

	return &ykBackend{cfg: cfg, routes: routes, order: order}, nil
}

// scanYKustomizeBases walks {basesDir}/{group}/{name}/{file} and returns
// the resulting routes. Files outside the {group}/{name}/ layer, or
// non-file leaves, are ignored.
func scanYKustomizeBases(basesDir string) ([]ykRoute, error) {
	groups, err := os.ReadDir(basesDir)
	if err != nil {
		return nil, err
	}
	var out []ykRoute
	for _, g := range groups {
		if !g.IsDir() {
			continue
		}
		groupPath := filepath.Join(basesDir, g.Name())
		names, err := os.ReadDir(groupPath)
		if err != nil {
			return nil, err
		}
		for _, n := range names {
			if !n.IsDir() {
				continue
			}
			namePath := filepath.Join(groupPath, n.Name())
			files, err := os.ReadDir(namePath)
			if err != nil {
				return nil, err
			}
			for _, f := range files {
				if f.IsDir() {
					continue
				}
				filePath := filepath.Join(namePath, f.Name())
				route := fmt.Sprintf("/v1/%s/%s/%s", g.Name(), n.Name(), f.Name())
				out = append(out, ykRoute{
					Path:        route,
					FilePath:    filePath,
					ContentType: DetectContentType(f.Name()),
				})
			}
		}
	}
	return out, nil
}

// ServeHTTP implements http.Handler. Only /v1/** paths are served; other
// paths fall through to 404 so the parent mux can route /health and
// /openapi.yaml.
func (b *ykBackend) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		MethodNotAllowed(w, http.MethodGet, http.MethodHead)
		return
	}
	if !strings.HasPrefix(r.URL.Path, "/v1/") {
		http.NotFound(w, r)
		return
	}
	route, ok := b.routes[r.URL.Path]
	if !ok {
		http.NotFound(w, r)
		return
	}
	body, err := os.ReadFile(route.FilePath)
	if err != nil {
		http.Error(w, "read: "+err.Error(), http.StatusInternalServerError)
		return
	}
	WriteAsset(w, r, route.FilePath, body)
}

// Routes returns the sorted list of served paths (stable order).
func (b *ykBackend) Routes() []string { return b.order }

// RouteContentType returns the content type a route will be served with.
func (b *ykBackend) RouteContentType(path string) string {
	return b.routes[path].ContentType
}
