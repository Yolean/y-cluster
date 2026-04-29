// Package serve implements `y-cluster serve`: a lightweight HTTP server for
// config assets (y-kustomize bases today; static dirs later). See
// SERVE_FEATURE.md for scope and SERVE_PLAN.md for design.
package serve

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"path/filepath"
	"sort"
	"time"

	"github.com/Yolean/y-cluster/pkg/configfile"
)

// ConfigFilename is the name of the YAML file every `-c` dir must contain.
const ConfigFilename = "y-cluster-serve.yaml"

// BackendType is the `type:` field in y-cluster-serve.yaml.
type BackendType string

const (
	TypeYKustomizeLocal     BackendType = "y-kustomize-local"
	TypeYKustomizeInCluster BackendType = "y-kustomize-incluster"
	TypeStatic              BackendType = "static"
)

// Config is the parsed y-cluster-serve.yaml plus the directory it came from.
type Config struct {
	// Dir is the absolute path of the `-c` directory; source paths
	// declared relative in YAML resolve against this.
	Dir string `json:"dir"`

	Port      int                        `json:"port" yaml:"port"`
	Type      BackendType                `json:"type" yaml:"type"`
	Static    *StaticConfig              `json:"static,omitempty" yaml:"static,omitempty"`
	Sources   []YKustomizeLocalSource    `json:"sources,omitempty" yaml:"sources,omitempty"`
	InCluster *YKustomizeInClusterConfig `json:"inCluster,omitempty" yaml:"inCluster,omitempty"`
}

// StaticConfig is declared so the schema round-trips, but the runtime
// backend is not implemented in the first release.
type StaticConfig struct {
	Dir              string `json:"dir" yaml:"dir"`
	Root             string `json:"root" yaml:"root"`
	YAMLToJSON       bool   `json:"yamlToJson,omitempty" yaml:"yamlToJson,omitempty"`
	DirTrailingSlash string `json:"dirTrailingSlash,omitempty" yaml:"dirTrailingSlash,omitempty"`
}

// YKustomizeLocalSource is one entry in `sources:` for type y-kustomize-local.
type YKustomizeLocalSource struct {
	Dir string `json:"dir" yaml:"dir"`
}

// YKustomizeInClusterConfig configures type y-kustomize-incluster: a
// backend that serves y-kustomize bases by watching Kubernetes
// Secrets named `y-kustomize.{group}.{name}` and mapping their data
// keys to `/v1/{group}/{name}/{file}` URLs. This replaces the
// y-kustomize service in ystack.
type YKustomizeInClusterConfig struct {
	// Namespace to watch. If empty: the pod's namespace when running
	// in-cluster, else the kubeconfig current-context namespace, else
	// "default".
	Namespace string `json:"namespace,omitempty" yaml:"namespace,omitempty"`

	// LabelSelector filters Secrets. Defaults to the ystack
	// convention `yolean.se/module-part=y-kustomize`.
	LabelSelector string `json:"labelSelector,omitempty" yaml:"labelSelector,omitempty"`

	// Kubeconfig is an optional explicit kubeconfig path. Empty uses
	// the default loader (in-cluster, then KUBECONFIG, then
	// ~/.kube/config).
	Kubeconfig string `json:"kubeconfig,omitempty" yaml:"kubeconfig,omitempty"`

	// Context is an optional kubeconfig context to override the
	// current-context. Empty uses the current-context.
	Context string `json:"context,omitempty" yaml:"context,omitempty"`

	// PollInterval controls how often the backend lists Secrets
	// from the apiserver to refresh its route table. Default 5s.
	// Smaller = lower mutation-to-served latency at the cost of
	// more apiserver round-trips; larger = the inverse.
	PollInterval time.Duration `json:"pollInterval,omitempty" yaml:"pollInterval,omitempty"`
}

// SetDir lets pkg/configfile attach the absolute config-dir path
// after a successful load; relative `sources[].dir` paths resolve
// against this. Part of the configfile.DirAware contract.
func (c *Config) SetDir(dir string) { c.Dir = dir }

// Validate satisfies configfile.Validator. The lower-cased
// validate() keeps the long, internal switch out of the public API.
func (c *Config) Validate() error { return c.validate() }

// LoadConfigDir reads `{dir}/y-cluster-serve.yaml`, validates it, and
// returns the parsed config with Dir set to the absolute of `dir`.
func LoadConfigDir(dir string) (*Config, error) {
	var c Config
	if err := configfile.Load(dir, ConfigFilename, &c); err != nil {
		return nil, err
	}
	return &c, nil
}

// LoadConfigDirs loads every `-c` dir, deduplicates by absolute path, and
// verifies that no two configs share a port.
func LoadConfigDirs(dirs []string) ([]*Config, error) {
	if len(dirs) == 0 {
		return nil, fmt.Errorf("at least one -c is required")
	}
	seen := make(map[string]bool, len(dirs))
	ports := make(map[int]string, len(dirs))
	out := make([]*Config, 0, len(dirs))
	for _, d := range dirs {
		c, err := LoadConfigDir(d)
		if err != nil {
			return nil, err
		}
		if seen[c.Dir] {
			continue // same `-c` passed twice is not an error
		}
		seen[c.Dir] = true
		if prev, dup := ports[c.Port]; dup {
			return nil, fmt.Errorf("port %d declared by both %s and %s", c.Port, prev, c.Dir)
		}
		ports[c.Port] = c.Dir
		out = append(out, c)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Port < out[j].Port })
	return out, nil
}

// ResolvedSources returns source dirs as absolute paths, resolved against
// Config.Dir. Only meaningful for y-kustomize-local configs.
func (c *Config) ResolvedSources() []string {
	out := make([]string, 0, len(c.Sources))
	for _, s := range c.Sources {
		p := s.Dir
		if !filepath.IsAbs(p) {
			p = filepath.Join(c.Dir, p)
		}
		out = append(out, filepath.Clean(p))
	}
	return out
}

// Digest returns a stable SHA-256 over the normalized config set. `ensure`
// compares this against the digest stored alongside the running daemon.
func Digest(cfgs []*Config) string {
	norm := make([]Config, 0, len(cfgs))
	for _, c := range cfgs {
		cp := *c
		if cp.Type == TypeYKustomizeLocal {
			srcs := c.ResolvedSources()
			cp.Sources = make([]YKustomizeLocalSource, len(srcs))
			for i, s := range srcs {
				cp.Sources[i] = YKustomizeLocalSource{Dir: s}
			}
		}
		norm = append(norm, cp)
	}
	sort.Slice(norm, func(i, j int) bool { return norm[i].Port < norm[j].Port })
	b, _ := json.Marshal(norm)
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

func (c *Config) validate() error {
	if c.Port < 1 || c.Port > 65535 {
		return fmt.Errorf("port %d out of range 1-65535", c.Port)
	}
	switch c.Type {
	case TypeYKustomizeLocal:
		if len(c.Sources) == 0 {
			return fmt.Errorf("type %s requires at least one source", c.Type)
		}
		for i, s := range c.Sources {
			if s.Dir == "" {
				return fmt.Errorf("sources[%d].dir is empty", i)
			}
		}
		if c.Static != nil {
			return fmt.Errorf("static config not allowed for type %s", c.Type)
		}
		if c.InCluster != nil {
			return fmt.Errorf("inCluster config not allowed for type %s", c.Type)
		}
	case TypeYKustomizeInCluster:
		if len(c.Sources) != 0 {
			return fmt.Errorf("sources not allowed for type %s", c.Type)
		}
		if c.Static != nil {
			return fmt.Errorf("static config not allowed for type %s", c.Type)
		}
		// InCluster is optional -- nil means "take every default",
		// which is what a pod mount of an almost-empty config should
		// do. Normalize to an empty struct so backends see a non-nil
		// pointer.
		if c.InCluster == nil {
			c.InCluster = &YKustomizeInClusterConfig{}
		}
	case TypeStatic:
		if c.Static == nil {
			return fmt.Errorf("type %s requires static block", c.Type)
		}
		if c.Static.Dir == "" {
			return fmt.Errorf("static.dir is empty")
		}
		if len(c.Sources) != 0 {
			return fmt.Errorf("sources not allowed for type %s", c.Type)
		}
		if c.InCluster != nil {
			return fmt.Errorf("inCluster config not allowed for type %s", c.Type)
		}
		switch c.Static.DirTrailingSlash {
		case "", "redirect":
		default:
			return fmt.Errorf("static.dirTrailingSlash %q is not a known mode", c.Static.DirTrailingSlash)
		}
	case "":
		return fmt.Errorf("type is required")
	default:
		return fmt.Errorf("unknown type %q", c.Type)
	}
	return nil
}
