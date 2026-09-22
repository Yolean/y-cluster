package config

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

// configfile.Load decodes through sigs.k8s.io/yaml, which reads the
// `json` tags. The generated schemas and schemagen's key collision
// check read the `yaml` tags. Nothing but this test keeps the two
// tag sets saying the same thing; a drift would make the schema
// accept a key the loader rejects, or the reverse.
func TestConfigTags_YAMLAndJSONNamesAgree(t *testing.T) {
	var walk func(t *testing.T, typ reflect.Type, path string)
	walk = func(t *testing.T, typ reflect.Type, path string) {
		for typ.Kind() == reflect.Ptr || typ.Kind() == reflect.Slice || typ.Kind() == reflect.Map {
			typ = typ.Elem()
		}
		if typ.Kind() != reflect.Struct {
			return
		}
		for i := 0; i < typ.NumField(); i++ {
			f := typ.Field(i)
			yamlName, _, _ := strings.Cut(f.Tag.Get("yaml"), ",")
			jsonName, _, _ := strings.Cut(f.Tag.Get("json"), ",")
			if yamlName != jsonName {
				t.Errorf("%s.%s: yaml tag %q but json tag %q", path, f.Name, yamlName, jsonName)
			}
			if f.IsExported() {
				walk(t, f.Type, path+"."+f.Name)
			}
		}
	}
	for _, cfg := range []any{QEMUConfig{}, DockerConfig{}, MultipassConfig{}, HetznerConfig{}} {
		typ := reflect.TypeOf(cfg)
		walk(t, typ, typ.Name())
	}
}

// Every provider constant is loadable, has a generated schema, and is
// named in the unknown-provider error.
func TestAllProviders_AreWiredUp(t *testing.T) {
	for _, p := range AllProviders {
		dir := t.TempDir()
		// Deliberately invalid beyond the provider, so the load fails in
		// that provider's own validation instead of "unknown provider".
		body := "provider: " + p + "\nmemory: not-a-number\n"
		if err := os.WriteFile(filepath.Join(dir, ProvisionFilename), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
		_, err := LoadProvision(dir)
		if err == nil || strings.Contains(err.Error(), "unknown provider") {
			t.Errorf("provider %q is in AllProviders but LoadProvision does not dispatch it: %v", p, err)
		}
		if _, err := os.Stat(filepath.Join("..", "schema", p+".schema.json")); err != nil {
			t.Errorf("provider %q has no generated schema: %v", p, err)
		}
	}
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, ProvisionFilename), []byte("provider: nope\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	_, err := LoadProvision(dir)
	for _, p := range AllProviders {
		if err == nil || !strings.Contains(err.Error(), p) {
			t.Errorf("unknown-provider error should list %q: %v", p, err)
		}
	}
}

func TestK3sPin_IsComplete(t *testing.T) {
	if K3sDefaultVersion() == "" || K3sMirrorTarget() == "" || K3sUpstreamRepo() == "" {
		t.Fatalf("k3s.yaml pin is incomplete: version=%q target=%q upstream=%q",
			K3sDefaultVersion(), K3sMirrorTarget(), K3sUpstreamRepo())
	}
}
