package yconverge

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

// ResolveDeps performs a topological sort of the dependency graph rooted
// at the given kustomize directory. It reads CUE imports from yconverge.cue
// files to discover dependencies. Returns the list of directories to
// converge, in dependency order (deps first, target last).
//
// cueRoot is the CUE module root (the directory containing cue.mod).
// CUE import paths are resolved relative to it. The module name is
// read from cue.mod/module.cue so the same machinery works for any
// CUE module, not just yolean.se/ystack.
// targetDir is the kustomize base to converge.
func ResolveDeps(cueRoot, targetDir string) ([]string, error) {
	abs, err := filepath.Abs(targetDir)
	if err != nil {
		return nil, fmt.Errorf("resolve target: %w", err)
	}
	rootAbs, err := filepath.Abs(cueRoot)
	if err != nil {
		return nil, fmt.Errorf("resolve cue root: %w", err)
	}

	modulePath, err := ParseCueModuleName(rootAbs)
	if err != nil {
		return nil, fmt.Errorf("read cue module name: %w", err)
	}

	visited := make(map[string]bool)
	var order []string
	if err := resolveDepsWalk(rootAbs, modulePath, abs, visited, &order); err != nil {
		return nil, err
	}
	return order, nil
}

func resolveDepsWalk(cueRoot, modulePath, dir string, visited map[string]bool, order *[]string) error {
	if visited[dir] {
		return nil
	}
	visited[dir] = true

	cueFile := filepath.Join(dir, "yconverge.cue")
	imports, err := ParseImports(cueFile, modulePath)
	if err != nil {
		return fmt.Errorf("parse imports %s: %w", cueFile, err)
	}

	for _, imp := range imports {
		depDir := filepath.Join(cueRoot, imp)
		depAbs, err := filepath.Abs(depDir)
		if err != nil {
			return fmt.Errorf("resolve dep %s: %w", imp, err)
		}
		if err := resolveDepsWalk(cueRoot, modulePath, depAbs, visited, order); err != nil {
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

// moduleNamePattern matches the `module: "..."` declaration at the
// top of cue.mod/module.cue. CUE allows extra fields (language,
// deps, etc.) so we match the line, not the whole file.
var moduleNamePattern = regexp.MustCompile(`(?m)^\s*module:\s*"([^"]+)"`)

// ParseCueModuleName reads cue.mod/module.cue under cueRoot and
// returns the declared module path (e.g. "yolean.se/ystack").
func ParseCueModuleName(cueRoot string) (string, error) {
	path := filepath.Join(cueRoot, "cue.mod", "module.cue")
	data, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	m := moduleNamePattern.FindStringSubmatch(string(data))
	if len(m) < 2 {
		return "", fmt.Errorf(`module declaration not found in %s`, path)
	}
	return m[1], nil
}

func fileExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

func contains(slice []string, s string) bool {
	for _, v := range slice {
		if v == s {
			return true
		}
	}
	return false
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
