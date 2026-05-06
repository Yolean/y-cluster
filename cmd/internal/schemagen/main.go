// schemagen generates JSON Schema files for two distinct surfaces:
//
//   - Provision-config schemas under pkg/provision/schema/: one
//     per provisioner config struct (qemu, docker, multipass) plus
//     a portable common.schema.json reflected from CommonConfig.
//
//   - Output schemas alongside the Go type that produces them
//     (e.g. pkg/gateway/state.schema.json next to gateway.State).
//     These document the JSON shape published by `y-cluster`
//     subcommands so downstream consumers can validate / parse
//     against a stable contract.
//
// Each per-provider schema has its `provider` property post-processed
// from the inherited enum into a single-value `const` so the file
// only validates configs intended for that provider. The common
// schema keeps the enum so portable configs validate against any
// supported provider value.
//
// schemagen also runs a collision check: it walks each provider
// struct's *own* (non-embedded) yaml field names and fails if the
// same name appears in more than one provider. CommonConfig fields
// are exempt — they're shared by design. The check stops a future
// per-provider field from accidentally shadowing or colliding with
// a name that should have been promoted to common.
//
// Run via `go generate ./pkg/provision/...`. CI runs the same
// command and fails if the working tree differs afterwards (drift
// gate), so the generator output and the source struct tags can't
// disagree.
package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"

	"github.com/invopop/jsonschema"
	"sigs.k8s.io/yaml"

	"github.com/Yolean/y-cluster/pkg/gateway"
	"github.com/Yolean/y-cluster/pkg/provision/config"
)

type pinFile struct {
	Version string `yaml:"version"`
	Mirror  struct {
		Upstream string `yaml:"upstream"`
		Target   string `yaml:"target"`
	} `yaml:"mirror"`
}

// providerTarget is one provider's schema generation job. The
// `provider` value drives the const-narrowing of the schema's
// `provider` property after invopop reflects it.
type providerTarget struct {
	filename string
	provider string
	sample   any
}

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func run() error {
	root, err := repoRoot()
	if err != nil {
		return fmt.Errorf("locate repo root: %w", err)
	}

	pinPath := filepath.Join(root, "pkg", "provision", "config", "k3s.yaml")
	pin, err := readPin(pinPath)
	if err != nil {
		return fmt.Errorf("read pin %s: %w", pinPath, err)
	}

	schemaDir := filepath.Join(root, "pkg", "provision", "schema")
	if err := os.MkdirAll(schemaDir, 0o755); err != nil {
		return err
	}

	providers := []providerTarget{
		{"qemu.schema.json", config.ProviderQEMU, &config.QEMUConfig{}},
		{"docker.schema.json", config.ProviderDocker, &config.DockerConfig{}},
		{"multipass.schema.json", config.ProviderMultipass, &config.MultipassConfig{}},
	}

	if err := checkCollisions(providers); err != nil {
		return fmt.Errorf("provider field collision: %w", err)
	}

	for _, t := range providers {
		out := filepath.Join(schemaDir, t.filename)
		if err := writeProviderSchema(out, t, pin); err != nil {
			return fmt.Errorf("generate %s: %w", t.filename, err)
		}
		fmt.Printf("wrote %s\n", out)
	}

	commonOut := filepath.Join(schemaDir, "common.schema.json")
	if err := writeCommonSchema(commonOut, pin); err != nil {
		return fmt.Errorf("generate common.schema.json: %w", err)
	}
	fmt.Printf("wrote %s\n", commonOut)

	// Output schemas: not provider-config schemas, but other
	// stable JSON shapes y-cluster produces for downstream
	// consumption. These live alongside the Go type that
	// produces them, NOT under pkg/provision/schema/ (which is
	// for input/config schemas). Add new outputs below as more
	// y-cluster commands publish stable JSON contracts.
	gatewayStateOut := filepath.Join(root, "pkg", "gateway", "state.schema.json")
	if err := writeOutputSchema(gatewayStateOut, &gateway.State{}, gateway.SchemaID); err != nil {
		return fmt.Errorf("generate %s: %w", gatewayStateOut, err)
	}
	fmt.Printf("wrote %s\n", gatewayStateOut)

	return nil
}

// writeOutputSchema reflects a non-provider Go struct into a
// standalone JSON Schema file. Differs from writeProviderSchema
// in two ways:
//
//   - Uses the `json` struct tag for property names, since the
//     output is JSON (not YAML), and consumers parse the JSON
//     directly. Reusing FieldNameTag="yaml" would produce YAML-
//     tagged property names that don't match the runtime output.
//   - No provider-narrowing post-processing -- the schema covers
//     the full output type as-is.
//
// The schemaID is written into the schema's $id so consumers
// can validate by URL reference. SchemaID values live in the
// source package as exported constants (e.g. gateway.SchemaID).
func writeOutputSchema(outPath string, sample any, schemaID string) error {
	r := &jsonschema.Reflector{
		AllowAdditionalProperties: false,
		DoNotReference:            false,
		FieldNameTag:              "json",
		// Keep RequiredFromJSONSchemaTags symmetric with the
		// provider schemas: omitempty fields fall through to
		// non-required without us having to hand-tag each one.
		RequiredFromJSONSchemaTags: false,
	}
	schema := r.Reflect(sample)
	data, err := json.MarshalIndent(schema, "", "  ")
	if err != nil {
		return err
	}
	// Replace the reflector's auto-generated $id with our stable
	// one. invopop emits a github.com/-prefixed URL by default;
	// we want the schema reachable by a URL operators control.
	data, err = setSchemaID(data, schemaID)
	if err != nil {
		return err
	}
	return os.WriteFile(outPath, append(data, '\n'), 0o644)
}

// setSchemaID rewrites the top-level $id field of the schema
// document. Returns the raw JSON bytes with the new $id.
func setSchemaID(data []byte, schemaID string) ([]byte, error) {
	var doc map[string]any
	if err := json.Unmarshal(data, &doc); err != nil {
		return nil, err
	}
	doc["$id"] = schemaID
	return json.MarshalIndent(doc, "", "  ")
}

// checkCollisions ensures no two providers declare the same own
// (non-embedded) yaml field name. CommonConfig fields are skipped:
// they're shared by design and surface in every provider via
// `yaml:",inline"`.
func checkCollisions(providers []providerTarget) error {
	seen := map[string]string{} // yaml name → first provider that declared it
	for _, p := range providers {
		for _, name := range ownYAMLNames(reflect.TypeOf(p.sample).Elem()) {
			if prev, ok := seen[name]; ok {
				return fmt.Errorf(
					"yaml key %q is declared by both %q and %q; "+
						"if it's a portable concept move it to CommonConfig, "+
						"otherwise rename one to disambiguate",
					name, prev, p.provider,
				)
			}
			seen[name] = p.provider
		}
	}
	return nil
}

// ownYAMLNames returns the yaml tag names of fields declared
// directly on t, skipping anonymous (embedded) fields, fields
// marked `yaml:"-"`, and the runtime-only Dir field.
func ownYAMLNames(t reflect.Type) []string {
	var names []string
	for i := 0; i < t.NumField(); i++ {
		f := t.Field(i)
		if f.Anonymous {
			continue
		}
		name := yamlName(f)
		if name == "" || name == "-" {
			continue
		}
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

func yamlName(f reflect.StructField) string {
	tag := f.Tag.Get("yaml")
	if tag == "" {
		return strings.ToLower(f.Name)
	}
	return strings.SplitN(tag, ",", 2)[0]
}

func writeProviderSchema(outPath string, t providerTarget, pin pinFile) error {
	data, err := reflectSchema(t.sample, pin)
	if err != nil {
		return err
	}
	data, err = narrowProviderToConst(data, t.provider)
	if err != nil {
		return err
	}
	return os.WriteFile(outPath, append(data, '\n'), 0o644)
}

func writeCommonSchema(outPath string, pin pinFile) error {
	data, err := reflectSchema(&config.CommonConfig{}, pin)
	if err != nil {
		return err
	}
	// `provider` is required in the per-provider schemas (and
	// const-narrowed there), but optional in the common schema:
	// `LoadProvision` calls `DiscoverProvider` to fill it when a
	// common-shape config omits the field. Editors validating
	// against common.schema.json would otherwise flag a perfectly
	// portable config as invalid.
	data, err = dropRequired(data, "CommonConfig", "provider")
	if err != nil {
		return fmt.Errorf("drop CommonConfig.provider from required: %w", err)
	}
	return os.WriteFile(outPath, append(data, '\n'), 0o644)
}

// dropRequired removes a property from the `required` list of the
// named definition under $defs. No-op when the property isn't
// listed (an earlier hand-edit or schema-shape change shouldn't
// fail the generator). Errors only on unparseable JSON or a
// missing definition, both of which point at a real bug upstream.
func dropRequired(data []byte, defName, prop string) ([]byte, error) {
	var doc map[string]any
	if err := json.Unmarshal(data, &doc); err != nil {
		return nil, err
	}
	defs, ok := doc["$defs"].(map[string]any)
	if !ok {
		return nil, fmt.Errorf("$defs missing or wrong type")
	}
	def, ok := defs[defName].(map[string]any)
	if !ok {
		return nil, fmt.Errorf("definition %q missing", defName)
	}
	required, _ := def["required"].([]any)
	out := required[:0]
	for _, item := range required {
		if name, _ := item.(string); name != prop {
			out = append(out, item)
		}
	}
	if len(out) == 0 {
		delete(def, "required")
	} else {
		def["required"] = out
	}
	return json.MarshalIndent(doc, "", "  ")
}

// reflectSchema runs invopop on the sample, applies pin
// substitutions, and returns the JSON-marshalled bytes.
func reflectSchema(sample any, pin pinFile) ([]byte, error) {
	r := &jsonschema.Reflector{
		// Strict mode: reject unknown fields. Mirrors the strict
		// YAML decode at runtime so editor hints and runtime
		// behavior agree.
		AllowAdditionalProperties: false,
		// Inline child types into $defs so each schema is
		// self-contained.
		DoNotReference: false,
		// Use struct field's `yaml:` tag for property names where
		// present, falling back to `json:` and field name.
		FieldNameTag: "yaml",
		// Render fields whose tags include `,omitempty` as
		// non-required. Without this, optional fields would be
		// listed in `required:` and tooling would flag missing
		// defaults as schema errors.
		RequiredFromJSONSchemaTags: false,
	}

	schema := r.Reflect(sample)
	data, err := json.MarshalIndent(schema, "", "  ")
	if err != nil {
		return nil, err
	}

	// Pin substitution. The placeholder matches the literal text
	// invopop wrote from the struct tag (`"default": "__K3S_TAG__"`).
	// `__K3S_TAG__` is the GitHub-release form of the version (with
	// `+k3sN` build-metadata separator). The container image is no
	// longer a config field — the docker provisioner derives it
	// from the version at runtime — so no `__K3S_IMAGE__`
	// substitution here.
	data = bytes.ReplaceAll(data, []byte(`"__K3S_TAG__"`), []byte(jsonString(pin.Version)))

	// Inject the provider enum into the embedded CommonConfig's
	// schema. invopop has no idea what values are legal — that
	// list lives in config.AllProviders — so we add the enum here
	// instead of hand-writing it in a struct tag we'd then have to
	// keep in sync.
	data, err = injectProviderEnum(data, config.AllProviders)
	if err != nil {
		return nil, err
	}

	return data, nil
}

// injectProviderEnum walks the JSON schema tree and adds an `enum`
// constraint to every `provider` property whose surrounding object
// also declares `"type": "string"`. The reflector emits the
// property without an enum because the tag deliberately omits
// `enum=...` literals — schemagen owns the value list via
// config.AllProviders so the source of truth is one place.
func injectProviderEnum(data []byte, providers []string) ([]byte, error) {
	var doc map[string]any
	if err := json.Unmarshal(data, &doc); err != nil {
		return nil, err
	}
	var walk func(any)
	walk = func(node any) {
		switch n := node.(type) {
		case map[string]any:
			if props, ok := n["properties"].(map[string]any); ok {
				if prov, ok := props["provider"].(map[string]any); ok {
					if _, hasConst := prov["const"]; !hasConst {
						prov["enum"] = stringSliceToAny(providers)
					}
				}
			}
			for _, v := range n {
				walk(v)
			}
		case []any:
			for _, v := range n {
				walk(v)
			}
		}
	}
	walk(doc)
	return json.MarshalIndent(doc, "", "  ")
}

func stringSliceToAny(ss []string) []any {
	out := make([]any, len(ss))
	for i, s := range ss {
		out[i] = s
	}
	return out
}

// narrowProviderToConst replaces the `enum` on every `provider`
// property with `const: <value>` and drops the enum, so a
// per-provider schema only validates configs for that provider.
func narrowProviderToConst(data []byte, value string) ([]byte, error) {
	var doc map[string]any
	if err := json.Unmarshal(data, &doc); err != nil {
		return nil, err
	}
	var walk func(any)
	walk = func(node any) {
		switch n := node.(type) {
		case map[string]any:
			if props, ok := n["properties"].(map[string]any); ok {
				if prov, ok := props["provider"].(map[string]any); ok {
					prov["const"] = value
					delete(prov, "enum")
				}
			}
			for _, v := range n {
				walk(v)
			}
		case []any:
			for _, v := range n {
				walk(v)
			}
		}
	}
	walk(doc)
	return json.MarshalIndent(doc, "", "  ")
}

// jsonString returns a JSON-quoted string literal. We use
// json.Marshal rather than fmt.Sprintf("%q", s) so embedded special
// characters get JSON-correct escaping.
func jsonString(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}

func readPin(path string) (pinFile, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return pinFile{}, err
	}
	var p pinFile
	if err := yaml.Unmarshal(data, &p); err != nil {
		return pinFile{}, err
	}
	if p.Version == "" || p.Mirror.Target == "" {
		return pinFile{}, fmt.Errorf("pin file missing version or mirror.target")
	}
	return p, nil
}

// repoRoot walks up from the current working directory looking for
// go.mod. The generator is invoked via `go generate` so cwd may be
// the package dir, not the repo root.
func repoRoot() (string, error) {
	dir, err := os.Getwd()
	if err != nil {
		return "", err
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir, nil
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return "", fmt.Errorf("go.mod not found above %s", dir)
		}
		dir = parent
	}
}
