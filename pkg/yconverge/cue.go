package yconverge

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"cuelang.org/go/cue"
	"cuelang.org/go/cue/cuecontext"
	"cuelang.org/go/cue/load"
)

// ParseChecks evaluates a yconverge.cue file and extracts the checks
// from step.checks. Returns an empty slice if no checks are defined.
func ParseChecks(cueDir string) ([]Check, error) {
	ctx := cuecontext.New()

	cfg := &load.Config{
		Dir: cueDir,
	}
	instances := load.Instances([]string{"."}, cfg)
	if len(instances) == 0 {
		return nil, fmt.Errorf("no CUE instances found in %s", cueDir)
	}
	inst := instances[0]
	if inst.Err != nil {
		return nil, fmt.Errorf("load CUE %s: %w", cueDir, inst.Err)
	}

	val := ctx.BuildInstance(inst)
	if err := val.Err(); err != nil {
		return nil, fmt.Errorf("build CUE %s: %w", cueDir, err)
	}

	checksVal := val.LookupPath(cue.ParsePath("step.checks"))
	if err := checksVal.Err(); err != nil {
		// step.checks not found — no checks defined
		return nil, nil
	}

	checksJSON, err := checksVal.MarshalJSON()
	if err != nil {
		return nil, fmt.Errorf("marshal checks from %s: %w", cueDir, err)
	}

	var checks []Check
	if err := json.Unmarshal(checksJSON, &checks); err != nil {
		return nil, fmt.Errorf("unmarshal checks from %s: %w", cueDir, err)
	}

	return checks, nil
}

// verifySchemaImport is the CUE-vendored path of the verify schema.
// It is shipped under cue.mod/pkg/yolean.se/ystack/yconverge/verify
// regardless of the consuming module's own name, so the import path
// is invariant across modules.
const verifySchemaImport = "yolean.se/ystack/yconverge/verify"

// ParseImports reads a yconverge.cue file and extracts dependency
// paths from CUE import statements within the given module. Returns
// filesystem-relative paths suitable for resolving to kustomize base
// directories.
//
// Example with module "yolean.se/ystack":
//
//	import "yolean.se/ystack/k3s/30-blobs:blobs" → "k3s/30-blobs"
func ParseImports(cueFile, modulePath string) ([]string, error) {
	data, err := os.ReadFile(cueFile)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}

	prefix := modulePath + "/"
	pattern := regexp.MustCompile(`"(` + regexp.QuoteMeta(prefix) + `[^"]+)"`)
	matches := pattern.FindAllStringSubmatch(string(data), -1)
	var deps []string
	for _, m := range matches {
		imp := m[1]
		if imp == verifySchemaImport {
			continue
		}
		// Strip the CUE package label (":name" suffix)
		path := imp
		if i := strings.LastIndex(path, ":"); i >= 0 {
			path = path[:i]
		}
		// Strip the module prefix
		path = strings.TrimPrefix(path, prefix)
		deps = append(deps, path)
	}
	return deps, nil
}

// FindCueFiles returns the paths of yconverge.cue files found in the
// given directories. Each directory is checked for a yconverge.cue file.
func FindCueFiles(dirs []string) []string {
	var found []string
	for _, dir := range dirs {
		p := filepath.Join(dir, "yconverge.cue")
		if _, err := os.Stat(p); err == nil {
			found = append(found, dir)
		}
	}
	return found
}
