package yconverge

import (
	"fmt"
	"path/filepath"
	"strings"
)

// ResolveDeps performs a topological sort of the dependency graph rooted
// at the given kustomize directory. It reads CUE imports from yconverge.cue
// files to discover dependencies. Returns the list of directories to
// converge, in dependency order (deps first, target last).
//
// baseDir is the root directory from which CUE import paths are resolved.
// For ystack, this is the ystack root (the CUE module root).
// targetDir is the kustomize base to converge.
func ResolveDeps(baseDir, targetDir string) ([]string, error) {
	abs, err := filepath.Abs(targetDir)
	if err != nil {
		return nil, fmt.Errorf("resolve target: %w", err)
	}
	baseAbs, err := filepath.Abs(baseDir)
	if err != nil {
		return nil, fmt.Errorf("resolve base: %w", err)
	}

	visited := make(map[string]bool)
	var order []string
	if err := resolveDepsWalk(baseAbs, abs, visited, &order); err != nil {
		return nil, err
	}
	return order, nil
}

func resolveDepsWalk(baseDir, dir string, visited map[string]bool, order *[]string) error {
	if visited[dir] {
		return nil
	}
	visited[dir] = true

	cueFile := filepath.Join(dir, "yconverge.cue")
	imports, err := ParseImports(cueFile)
	if err != nil {
		return fmt.Errorf("parse imports %s: %w", cueFile, err)
	}

	for _, imp := range imports {
		depDir := filepath.Join(baseDir, imp)
		depAbs, err := filepath.Abs(depDir)
		if err != nil {
			return fmt.Errorf("resolve dep %s: %w", imp, err)
		}
		if err := resolveDepsWalk(baseDir, depAbs, visited, order); err != nil {
			return err
		}
	}

	*order = append(*order, dir)
	return nil
}

// FindCueModuleRoot walks up from dir looking for cue.mod/module.cue.
// Returns the directory containing cue.mod, or empty string if not found.
func FindCueModuleRoot(dir string) string {
	abs, err := filepath.Abs(dir)
	if err != nil {
		return ""
	}
	for {
		if fileExists(filepath.Join(abs, "cue.mod", "module.cue")) {
			return abs
		}
		parent := filepath.Dir(abs)
		if parent == abs {
			return ""
		}
		abs = parent
	}
}

func fileExists(path string) bool {
	_, err := filepath.Abs(path)
	if err != nil {
		return false
	}
	info, err := filepath.Glob(path)
	if err != nil || len(info) == 0 {
		return false
	}
	return true
}

// RelPath returns path relative to base, or the original path if
// the relative path would require going above base.
func RelPath(base, path string) string {
	rel, err := filepath.Rel(base, path)
	if err != nil || strings.HasPrefix(rel, "..") {
		return path
	}
	return rel
}
