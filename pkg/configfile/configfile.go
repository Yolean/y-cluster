// Package configfile is the shared loading primitive for y-cluster
// CLI subcommands that take `-c <dir>` and read a single YAML file
// inside that directory.
//
// Subcommands provide their own typed config struct and (optionally)
// a Validator and a DirAware implementation. configfile owns the
// strict YAML decode, the abs-path resolution, and the wrapping of
// every error with the offending path so users see a useful message
// without each subcommand re-implementing it.
package configfile

import (
	"fmt"
	"os"
	"path/filepath"

	"sigs.k8s.io/yaml"

	"github.com/Yolean/y-cluster/pkg/envsubst"
)

// Validator is implemented by config types whose state can be
// self-checked. Errors are surfaced as-is to the operator.
type Validator interface {
	Validate() error
}

// DirAware is implemented by config types that need to know the
// absolute path of the config directory after load -- typically to
// resolve relative paths declared inside the YAML.
type DirAware interface {
	SetDir(string)
}

// Defaulter is implemented by config types that fill zero-valued
// fields with declared defaults after unmarshal. Called between
// SetDir and Validate so the validator sees a defaulted state.
type Defaulter interface {
	ApplyDefaults()
}

// Load reads <dir>/<filename> with strict YAML decoding into target.
// Strict decoding rejects unknown fields, which catches typos before
// the runtime quietly ignores them.
//
// After unmarshal, Load calls target.SetDir(abs) if target implements
// DirAware, then target.Validate() if target implements Validator.
// Validation errors are wrapped with the file path.
func Load[T any](dir, filename string, target *T) error {
	abs, err := filepath.Abs(dir)
	if err != nil {
		return fmt.Errorf("resolve %s: %w", dir, err)
	}
	info, err := os.Stat(abs)
	if err != nil {
		return fmt.Errorf("config dir %s: %w", dir, err)
	}
	if !info.IsDir() {
		return fmt.Errorf("config path is not a directory: %s", abs)
	}
	path := filepath.Join(abs, filename)
	data, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("read %s: %w", path, err)
	}
	if err := yaml.UnmarshalStrict(data, target); err != nil {
		return fmt.Errorf("parse %s: %w", path, err)
	}
	// Env substitution runs before defaults/validation so tagged
	// fields are filled with concrete values when the validator
	// inspects them. Untagged occurrences of ${...} fail loud
	// here, before defaults could mask the user's intent.
	if err := envsubst.Apply(target, envsubst.OSEnv); err != nil {
		return fmt.Errorf("%s: %w", path, err)
	}
	if d, ok := any(target).(DirAware); ok {
		d.SetDir(abs)
	}
	if d, ok := any(target).(Defaulter); ok {
		d.ApplyDefaults()
	}
	if v, ok := any(target).(Validator); ok {
		if err := v.Validate(); err != nil {
			return fmt.Errorf("%s: %w", path, err)
		}
	}
	return nil
}
