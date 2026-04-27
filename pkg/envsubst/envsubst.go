// Package envsubst provides opt-in shell-style ${VAR} substitution
// for typed config structs.
//
// # Why opt-in
//
// y-cluster loads several YAML configs (provision, serve, future
// registries) into typed structs via pkg/configfile. Some fields
// genuinely need env-driven values -- the canonical case is the
// GCP access token in registries.yaml's auth.password -- but most
// fields shouldn't accept ${...} at all. A blanket text-level
// pre-pass would let users start putting ${VAR} in keys, enum
// fields, and other places we don't want to commit to supporting,
// turning a forward-compatibility hazard into a feature contract.
//
// So the package goes the other way: nothing is substitutable
// unless the schema says so via an `envsubst:"true"` struct tag.
// Apply walks a defaults-applied target after YAML unmarshal,
// substituting ${VAR} on tagged string leaves and erroring on
// any ${ found at an untagged position. The caller's struct is
// the policy.
//
// # Syntax
//
// Supported expansion forms:
//
//	${VAR}              -- required; errors if VAR is not set
//	${VAR:-default}     -- optional; uses default if VAR is unset
//	$$                  -- literal `$` (escape; no expansion)
//
// Variable names match [A-Za-z_][A-Za-z0-9_]*. `$VAR` (no braces)
// is intentionally not supported -- braces make scan-and-reject
// unambiguous and avoid surprises with shell-like word boundaries.
package envsubst

import (
	"fmt"
	"os"
	"reflect"
	"regexp"
	"strings"
)

// Tag is the struct tag key that marks a string field as
// substitutable. Only the literal value "true" enables it.
const Tag = "envsubst"

// LookupFunc resolves a variable name. The bool follows
// os.LookupEnv: false when the variable is unset.
type LookupFunc func(name string) (string, bool)

// OSEnv is a LookupFunc backed by os.LookupEnv.
func OSEnv(name string) (string, bool) { return os.LookupEnv(name) }

// envRefRE matches ${NAME} and ${NAME:-default}. The default body
// is non-greedy and excludes `}` so nested braces don't confuse the
// scan. We don't try to support escaped `}` in defaults; if you
// need a literal `}` in a default, set the variable instead.
var envRefRE = regexp.MustCompile(`\$\{([A-Za-z_][A-Za-z0-9_]*)(?::-([^}]*))?\}`)

// Expand applies ${VAR} and ${VAR:-default} substitutions in s.
// Returns an error naming the first undefined variable that has
// no default. `$$` is unescaped to a literal `$`.
//
// Single-pass: a substituted value is not re-scanned for further
// ${...} -- this keeps behavior predictable and matches what
// envsubst(1) does by default.
func Expand(s string, lookup LookupFunc) (string, error) {
	if !strings.Contains(s, "$") {
		return s, nil
	}
	if lookup == nil {
		lookup = OSEnv
	}

	// Two phases so $$ escapes survive the env-ref scan: we replace
	// $$ with a sentinel rune that can't appear in env-ref syntax,
	// run the regex, then put $ back.
	const sentinel = "\x00"
	work := strings.ReplaceAll(s, "$$", sentinel)

	var firstErr error
	out := envRefRE.ReplaceAllStringFunc(work, func(m string) string {
		match := envRefRE.FindStringSubmatch(m)
		name, def := match[1], match[2]
		val, ok := lookup(name)
		if ok {
			return val
		}
		// Group 2 is the default body. Distinguish "no default"
		// (group's submatch index is -1) from "default is empty
		// string" (e.g. ${VAR:-}). FindStringSubmatch returns ""
		// for both, so we re-check via the index pair.
		idx := envRefRE.FindStringSubmatchIndex(m)
		if idx[4] >= 0 { // capture-group 2 was present
			return def
		}
		if firstErr == nil {
			firstErr = fmt.Errorf("undefined variable %q", name)
		}
		return m
	})
	if firstErr != nil {
		return "", firstErr
	}
	return strings.ReplaceAll(out, sentinel, "$"), nil
}

// Apply walks v via reflection. v must be a non-nil pointer to a
// struct. Behavior per element:
//
//   - String field tagged envsubst:"true": Expand the value;
//     errors propagate with a path prefix.
//   - String field NOT tagged: if the value contains a ${...}
//     reference, return an error identifying the path. Plain
//     strings without `$` pass through.
//   - Slice/array of strings tagged envsubst:"true": Expand each
//     element.
//   - Map[string]string tagged envsubst:"true": Expand each value
//     (keys are never substituted, even on tagged maps).
//   - Nested struct / slice of structs / map of struct values:
//     recurse. The tag does NOT inherit -- each leaf field decides
//     for itself.
//   - All other types (numbers, bools, etc.) are left alone.
//
// The forward-compat scan is essential: tagging a field is a
// public commitment, so anything else getting ${...} support
// "for free" by living in a substituted subtree would surprise
// later-version us.
func Apply(v any, lookup LookupFunc) error {
	if v == nil {
		return fmt.Errorf("envsubst.Apply: nil target")
	}
	rv := reflect.ValueOf(v)
	if rv.Kind() != reflect.Ptr || rv.IsNil() {
		return fmt.Errorf("envsubst.Apply: target must be a non-nil pointer, got %T", v)
	}
	if lookup == nil {
		lookup = OSEnv
	}
	return walk(rv.Elem(), nil, false, lookup)
}

func walk(v reflect.Value, path []string, taggedHere bool, lookup LookupFunc) error {
	switch v.Kind() {
	case reflect.Ptr, reflect.Interface:
		if v.IsNil() {
			return nil
		}
		return walk(v.Elem(), path, taggedHere, lookup)

	case reflect.Struct:
		t := v.Type()
		for i := 0; i < t.NumField(); i++ {
			f := t.Field(i)
			if !f.IsExported() {
				continue
			}
			tagged := f.Tag.Get(Tag) == "true"
			fieldPath := append(append([]string(nil), path...), yamlName(f))
			if err := walk(v.Field(i), fieldPath, tagged, lookup); err != nil {
				return err
			}
		}
		return nil

	case reflect.Slice, reflect.Array:
		for i := 0; i < v.Len(); i++ {
			elemPath := append(append([]string(nil), path...), fmt.Sprintf("[%d]", i))
			if err := walk(v.Index(i), elemPath, taggedHere, lookup); err != nil {
				return err
			}
		}
		return nil

	case reflect.Map:
		// Map keys: reject ${...} unconditionally. We never want
		// to commit to expanding keys -- that would open the door
		// to dynamic schema shapes.
		iter := v.MapRange()
		for iter.Next() {
			k := iter.Key()
			if k.Kind() == reflect.String && containsRef(k.String()) {
				return policyError(append(path, fmt.Sprintf("[key=%q]", k.String())),
					"env substitution is never supported in YAML keys")
			}
			val := iter.Value()
			// Map values aren't addressable; copy through a
			// settable reflect.Value if we need to mutate.
			if val.Kind() == reflect.String {
				if err := applyString(val.String(), append(path, fmt.Sprintf("[%q]", k.String())), taggedHere, lookup, func(s string) {
					v.SetMapIndex(k, reflect.ValueOf(s))
				}); err != nil {
					return err
				}
				continue
			}
			// Recurse into struct values via a settable copy.
			cp := reflect.New(val.Type()).Elem()
			cp.Set(val)
			if err := walk(cp, append(path, fmt.Sprintf("[%q]", k.String())), taggedHere, lookup); err != nil {
				return err
			}
			v.SetMapIndex(k, cp)
		}
		return nil

	case reflect.String:
		return applyString(v.String(), path, taggedHere, lookup, func(s string) {
			if v.CanSet() {
				v.SetString(s)
			}
		})

	default:
		return nil
	}
}

func applyString(s string, path []string, tagged bool, lookup LookupFunc, set func(string)) error {
	if !containsRef(s) {
		return nil
	}
	if !tagged {
		return policyError(path, "env substitution is not supported here; if it should be, add `envsubst:\"true\"` to the field")
	}
	expanded, err := Expand(s, lookup)
	if err != nil {
		return fmt.Errorf("%s: %w", joinPath(path), err)
	}
	set(expanded)
	return nil
}

// containsRef reports whether s contains an expansion reference.
// We only look for ${ -- bare $X is not part of the supported
// syntax, and a stray $ that isn't $$ is allowed (it's just text).
func containsRef(s string) bool {
	return strings.Contains(s, "${")
}

func policyError(path []string, msg string) error {
	return fmt.Errorf("%s: %s", joinPath(path), msg)
}

func joinPath(path []string) string {
	if len(path) == 0 {
		return "<root>"
	}
	return strings.Join(path, ".")
}

// yamlName returns the YAML field name from a struct tag, falling
// back to the Go field name when there is no `yaml:` tag. The path
// reported in errors then matches what users see in their config
// file rather than the Go capitalization.
func yamlName(f reflect.StructField) string {
	tag := f.Tag.Get("yaml")
	if tag == "" || tag == "-" {
		// sigs.k8s.io/yaml falls back to the json tag; mirror that.
		tag = f.Tag.Get("json")
	}
	if tag == "" || tag == "-" {
		return f.Name
	}
	if i := strings.IndexByte(tag, ','); i >= 0 {
		tag = tag[:i]
	}
	if tag == "" {
		return f.Name
	}
	return tag
}
