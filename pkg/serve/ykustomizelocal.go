package serve

import (
	"encoding/base64"
	"fmt"
	"net/http"
	"sort"
	"strings"

	"sigs.k8s.io/kustomize/api/krusty"
	"sigs.k8s.io/kustomize/kyaml/filesys"
	"sigs.k8s.io/yaml"
)

// ykInClusterSecretPrefix is reused here -- the local backend
// uses the same naming convention as the in-cluster backend so
// the two modes are interchangeable. (See ykustomizeincluster.go.)

// ForbiddenSecretKey is the data-key name the y-kustomize serve
// refuses, regardless of backend (local kustomize build or
// in-cluster Secret watch). HTTP kustomize resources can't be a
// directory or another kustomization, so a key with this name
// would mislead users into fetching it as a base.
const ForbiddenSecretKey = "kustomization.yaml"

// ykRoute is a resolved path -> body mapping with metadata used
// by the openapi spec.
type ykRoute struct {
	Path        string // e.g. /v1/blobs/setup-bucket-job/base-for-annotations.yaml
	Body        []byte // served verbatim
	ContentType string // detected from path suffix
}

// ykBackend serves a frozen map of /v1 routes built from
// `kustomize build` output. Bytes live in memory because the
// authoritative source is the kustomize build, not the
// filesystem -- a re-read from disk would be a different value
// (raw file vs the base64-decoded data key kustomize produced).
type ykBackend struct {
	cfg    *Config
	routes map[string]ykRoute
	order  []string // sorted paths, for openapi stability
}

// newYKustomizeLocalBackend runs `kustomize build` on each
// configured source directory, picks out the Secrets named
// `y-kustomize.{group}.{name}`, and serves each Secret's data
// keys at `/v1/{group}/{name}/{key}`.
//
// The local mode now mirrors the in-cluster mode exactly: same
// Secret naming, same URL shape, same `ForbiddenSecretKey`
// guard. The only difference is the source of truth -- a
// kustomize build vs a kubernetes watch.
func newYKustomizeLocalBackend(cfg *Config) (*ykBackend, error) {
	if cfg.Type != TypeYKustomizeLocal {
		return nil, fmt.Errorf("not a y-kustomize-local config: %s", cfg.Type)
	}
	sources := cfg.ResolvedSources()
	if len(sources) == 0 {
		return nil, fmt.Errorf("no sources")
	}

	routes := map[string]ykRoute{}
	origin := map[string]string{} // route -> source dir (for dup error)

	for _, src := range sources {
		secrets, err := buildKustomizeSecrets(src)
		if err != nil {
			return nil, err
		}
		for _, sec := range secrets {
			srcRoutes, err := secretRoutes(sec)
			if err != nil {
				return nil, fmt.Errorf("source %s: %w", src, err)
			}
			for _, r := range srcRoutes {
				if prev, dup := origin[r.Path]; dup {
					return nil, fmt.Errorf("duplicate route %s from %s and %s", r.Path, prev, src)
				}
				routes[r.Path] = r
				origin[r.Path] = src
			}
		}
	}

	order := make([]string, 0, len(routes))
	for p := range routes {
		order = append(order, p)
	}
	sort.Strings(order)

	return &ykBackend{cfg: cfg, routes: routes, order: order}, nil
}

// parsedSecret is the small subset of corev1.Secret we need
// after parsing kustomize output. We keep it private rather than
// importing corev1 -- the YAML parse is cheaper and avoids
// pulling the typed clientset into a backend that doesn't talk
// to an apiserver.
type parsedSecret struct {
	Name string
	Data map[string][]byte
}

// buildKustomizeSecrets runs `kustomize build` on dir and
// returns every Secret resource whose name starts with the
// y-kustomize. prefix (we ignore other Secrets because the
// convention is the contract). Other resource kinds in the
// build output are ignored entirely -- they're applied to the
// cluster, not served.
func buildKustomizeSecrets(dir string) ([]*parsedSecret, error) {
	fs := filesys.MakeFsOnDisk()
	k := krusty.MakeKustomizer(krusty.MakeDefaultOptions())
	rm, err := k.Run(fs, dir)
	if err != nil {
		return nil, fmt.Errorf("kustomize build %s: %w", dir, err)
	}
	yml, err := rm.AsYaml()
	if err != nil {
		return nil, fmt.Errorf("encode %s: %w", dir, err)
	}

	var out []*parsedSecret
	for _, doc := range splitYAMLDocs(yml) {
		var raw map[string]any
		if err := yaml.Unmarshal(doc, &raw); err != nil {
			return nil, fmt.Errorf("parse %s: %w", dir, err)
		}
		if raw == nil {
			continue
		}
		kind, _ := raw["kind"].(string)
		if kind != "Secret" {
			continue
		}
		meta, _ := raw["metadata"].(map[string]any)
		name, _ := meta["name"].(string)
		if !strings.HasPrefix(name, ykInClusterSecretPrefix) {
			continue
		}

		ps := &parsedSecret{Name: name, Data: map[string][]byte{}}
		// kustomize emits Secret data as base64-encoded under
		// `data:`; decoded plaintext lives under `stringData:` only
		// while building. By the time AsYaml() emits the manifest
		// the values are always base64.
		if data, ok := raw["data"].(map[string]any); ok {
			for k, v := range data {
				s, _ := v.(string)
				dec, err := base64.StdEncoding.DecodeString(s)
				if err != nil {
					return nil, fmt.Errorf("secret %s data[%q]: base64 decode: %w", name, k, err)
				}
				ps.Data[k] = dec
			}
		}
		out = append(out, ps)
	}
	return out, nil
}

// splitYAMLDocs splits a multi-document YAML stream by `---`.
// `\n---\n` is the kustomize / kubectl convention; surrounding
// whitespace is trimmed so a leading/trailing separator doesn't
// produce an empty doc.
func splitYAMLDocs(b []byte) [][]byte {
	const sep = "\n---\n"
	if strings.HasPrefix(string(b), "---\n") {
		b = append([]byte{'\n'}, b...)
	}
	parts := strings.Split(string(b), sep)
	out := make([][]byte, 0, len(parts))
	for _, p := range parts {
		p = strings.TrimSpace(p)
		if len(p) > 0 {
			out = append(out, []byte(p))
		}
	}
	return out
}

// secretRoutes turns a Secret named `y-kustomize.{group}.{name}`
// into ykRoutes at `/v1/{group}/{name}/{key}` for each data key.
// Returns ForbiddenSecretKey error if any data key is named
// `kustomization.yaml`.
func secretRoutes(sec *parsedSecret) ([]ykRoute, error) {
	suffix := strings.TrimPrefix(sec.Name, ykInClusterSecretPrefix)
	pathBase := "/v1/" + strings.Replace(suffix, ".", "/", 1)
	out := make([]ykRoute, 0, len(sec.Data))
	for key, val := range sec.Data {
		if key == ForbiddenSecretKey {
			return nil, fmt.Errorf(
				"secret %q data key %q is reserved: a key by that name "+
					"would mislead callers into fetching the URL as a kustomize base "+
					"(http kustomize resources can't be a directory or another kustomization); "+
					"rename the data key", sec.Name, key)
		}
		body := make([]byte, len(val))
		copy(body, val)
		out = append(out, ykRoute{
			Path:        pathBase + "/" + key,
			Body:        body,
			ContentType: DetectContentType(key),
		})
	}
	return out, nil
}

// ServeHTTP implements http.Handler. Only /v1/** paths are
// served; other paths fall through to 404 so the parent mux can
// route /health and /openapi.yaml.
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
	WriteAsset(w, r, route.Path, route.Body)
}

// Routes returns the sorted list of served paths (stable order).
func (b *ykBackend) Routes() []string { return b.order }

// RouteContentType returns the content type a route will be
// served with.
func (b *ykBackend) RouteContentType(path string) string {
	return b.routes[path].ContentType
}
