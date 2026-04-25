// Package traverse walks kustomization directory trees and reports
// structural metadata: the set of local directories visited and the
// resolved namespace at each level.
package traverse

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"sigs.k8s.io/kustomize/api/types"
	"sigs.k8s.io/yaml"
)

// Result holds the output of a kustomization tree walk.
type Result struct {
	// Namespace resolved from the kustomization tree.
	// Empty string if no namespace is declared.
	Namespace string

	// Dirs lists all local directories visited, in depth-first order.
	// Paths are absolute. The target directory is always last.
	Dirs []string
}

// WarnFunc is called for non-fatal issues during traversal.
type WarnFunc func(format string, a ...any)

// Walk traverses the kustomization tree rooted at dir and returns
// the list of local directories visited (depth-first, deduplicated)
// and the resolved namespace.
func Walk(dir string, warn WarnFunc) (*Result, error) {
	abs, err := filepath.Abs(dir)
	if err != nil {
		return nil, fmt.Errorf("resolve path %s: %w", dir, err)
	}

	k, _, err := LoadKustomization(abs)
	if err != nil {
		return nil, err
	}
	if k == nil {
		return nil, fmt.Errorf("no kustomization file in %s", dir)
	}

	dirs, err := collectDirs(abs, warn)
	if err != nil {
		return nil, err
	}

	ns := resolveNamespace(abs)

	return &Result{
		Namespace: ns,
		Dirs:      dirs,
	}, nil
}

// RelDirs returns the directory list as paths relative to the given root.
func (r *Result) RelDirs(root string) ([]string, error) {
	abs, err := filepath.Abs(root)
	if err != nil {
		return nil, err
	}
	rels := make([]string, 0, len(r.Dirs))
	for _, d := range r.Dirs {
		rel, err := filepath.Rel(abs, d)
		if err != nil {
			return nil, err
		}
		rels = append(rels, rel)
	}
	return rels, nil
}

// LoadKustomization reads the kustomization file in dir, returning the
// parsed struct, the file path that was read, and any error.
// Returns (nil, "", nil) if no kustomization file exists.
func LoadKustomization(dir string) (*types.Kustomization, string, error) {
	for _, name := range kustomizationFiles {
		p := filepath.Join(dir, name)
		data, err := os.ReadFile(p)
		if err != nil {
			if os.IsNotExist(err) {
				continue
			}
			return nil, p, err
		}
		var k types.Kustomization
		if err := yaml.Unmarshal(data, &k); err != nil {
			return nil, p, fmt.Errorf("parse %s: %w", p, err)
		}
		k.FixKustomization()
		return &k, p, nil
	}
	return nil, "", nil
}

// HasKustomization checks if dir contains a kustomization file.
func HasKustomization(dir string) bool {
	for _, name := range kustomizationFiles {
		if _, err := os.Stat(filepath.Join(dir, name)); err == nil {
			return true
		}
	}
	return false
}

var kustomizationFiles = []string{
	"kustomization.yaml",
	"kustomization.yml",
	"Kustomization",
}

// IsRemote detects refs that are not local filesystem paths.
// Remote refs have a scheme (://), git@ prefix, or a first path
// segment that looks like a domain (contains a dot, has a slash
// after it). A bare filename like "deployment.yaml" is not remote.
func IsRemote(entry string) bool {
	if strings.Contains(entry, "://") {
		return true
	}
	if strings.HasPrefix(entry, "git@") {
		return true
	}
	i := strings.Index(entry, "/")
	if i < 0 {
		return false // no slash = local file or dir, never a domain
	}
	first := entry[:i]
	return strings.Contains(first, ".") && !strings.HasPrefix(first, ".")
}

// LocalBases returns the absolute paths of resources/components entries
// that resolve to local directories containing a kustomization file.
func LocalBases(dir string, k *types.Kustomization, warn WarnFunc) []string {
	var bases []string
	entries := append([]string{}, k.Resources...)
	entries = append(entries, k.Components...)
	for _, e := range entries {
		if IsRemote(e) {
			continue
		}
		abs := filepath.Clean(filepath.Join(dir, e))
		info, err := os.Stat(abs)
		if err != nil {
			if warn != nil {
				warn("skipping unresolvable ref %q from %s", e, dir)
			}
			continue
		}
		if !info.IsDir() {
			continue
		}
		if !HasKustomization(abs) {
			continue
		}
		bases = append(bases, abs)
	}
	return bases
}

func collectDirs(rootAbs string, warn WarnFunc) ([]string, error) {
	visited := map[string]bool{}
	var results []string
	if err := walkTree(rootAbs, visited, &results, warn); err != nil {
		return nil, err
	}
	return results, nil
}

func walkTree(dirAbs string, visited map[string]bool, results *[]string, warn WarnFunc) error {
	if visited[dirAbs] {
		return nil
	}
	visited[dirAbs] = true
	k, _, err := LoadKustomization(dirAbs)
	if err != nil {
		return err
	}
	if k == nil {
		return nil
	}
	for _, base := range LocalBases(dirAbs, k, warn) {
		if err := walkTree(base, visited, results, warn); err != nil {
			return err
		}
	}
	*results = append(*results, dirAbs)
	return nil
}

func resolveNamespace(dirAbs string) string {
	k, _, err := LoadKustomization(dirAbs)
	if err != nil || k == nil {
		return ""
	}
	if k.Namespace != "" {
		return k.Namespace
	}
	bases := LocalBases(dirAbs, k, nil)
	if len(bases) == 1 {
		return resolveNamespace(bases[0])
	}
	return ""
}
