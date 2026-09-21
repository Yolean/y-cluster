package main

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"

	"github.com/Yolean/y-cluster/pkg/provision/config"
)

func generated(t *testing.T) (root string, files map[string][]byte) {
	t.Helper()
	root, err := repoRoot()
	if err != nil {
		t.Fatal(err)
	}
	files, err = generate(root)
	if err != nil {
		t.Fatal(err)
	}
	return root, files
}

// The committed schemas are what editors and downstream repos read.
// A struct tag changed without regenerating leaves them describing a
// config the binary no longer accepts.
func TestGenerate_CommittedFilesAreCurrent(t *testing.T) {
	root, files := generated(t)
	for rel, want := range files {
		got, err := os.ReadFile(filepath.Join(root, rel))
		if err != nil {
			t.Errorf("%s: %v (run `go generate ./pkg/provision/...`)", rel, err)
			continue
		}
		if !bytes.Equal(got, want) {
			t.Errorf("%s is stale: run `go generate ./pkg/provision/...` and commit the result", rel)
		}
	}
}

// A schema nothing generates any more (a removed or renamed provider)
// would stay in the repo looking authoritative.
func TestGenerate_NoOrphanSchemas(t *testing.T) {
	root, files := generated(t)
	onDisk, err := filepath.Glob(filepath.Join(root, schemaDir, "*.schema.json"))
	if err != nil {
		t.Fatal(err)
	}
	for _, path := range onDisk {
		rel := schemaDir + "/" + filepath.Base(path)
		if _, ok := files[rel]; !ok {
			t.Errorf("%s is not generated; delete it or register its provider", rel)
		}
	}
}

func TestGenerate_OneSchemaPerProviderPlusCommon(t *testing.T) {
	_, files := generated(t)
	var got []string
	for rel := range files {
		if strings.HasPrefix(rel, schemaDir+"/") {
			got = append(got, strings.TrimSuffix(filepath.Base(rel), ".schema.json"))
		}
	}
	sort.Strings(got)
	want := append([]string{"common"}, config.AllProviders...)
	sort.Strings(want)
	if !reflect.DeepEqual(got, want) {
		t.Errorf("schemas = %v, want %v", got, want)
	}
}

// providerProperty digs `provider` out of the embedded CommonConfig
// definition, and whether CommonConfig requires it.
func providerProperty(t *testing.T, schema []byte) (prop map[string]any, required bool) {
	t.Helper()
	var doc map[string]any
	if err := json.Unmarshal(schema, &doc); err != nil {
		t.Fatal(err)
	}
	var find func(node any) map[string]any
	find = func(node any) map[string]any {
		m, ok := node.(map[string]any)
		if !ok {
			return nil
		}
		if props, ok := m["properties"].(map[string]any); ok {
			if _, ok := props["provider"]; ok {
				return m
			}
		}
		for _, v := range m {
			if found := find(v); found != nil {
				return found
			}
		}
		return nil
	}
	owner := find(doc)
	if owner == nil {
		t.Fatal("no definition with a provider property")
	}
	// No `required` key at all when nothing is required.
	list, _ := owner["required"].([]any)
	for _, r := range list {
		if r == "provider" {
			required = true
		}
	}
	return owner["properties"].(map[string]any)["provider"].(map[string]any), required
}

// A per-provider schema validates configs for that provider only.
func TestGenerate_ProviderSchemaPinsItsProvider(t *testing.T) {
	_, files := generated(t)
	for _, name := range config.AllProviders {
		prop, required := providerProperty(t, files[schemaDir+"/"+name+".schema.json"])
		if prop["const"] != name {
			t.Errorf("%s: provider const = %v", name, prop["const"])
		}
		if _, hasEnum := prop["enum"]; hasEnum {
			t.Errorf("%s: provider still carries the enum next to its const", name)
		}
		if !required {
			t.Errorf("%s: provider must be required", name)
		}
	}
}

// The common schema is for portable configs: any registered provider
// is valid, and so is leaving it out for host discovery to fill in.
func TestGenerate_CommonSchemaAcceptsAnyOrNoProvider(t *testing.T) {
	_, files := generated(t)
	prop, required := providerProperty(t, files[schemaDir+"/common.schema.json"])
	var enum []string
	for _, v := range prop["enum"].([]any) {
		enum = append(enum, v.(string))
	}
	if !reflect.DeepEqual(enum, config.AllProviders) {
		t.Errorf("provider enum = %v, want %v", enum, config.AllProviders)
	}
	if required {
		t.Error("provider must be optional in the common schema")
	}
}

// The k3s version default comes from the pin file, the same file the
// runtime default and the image mirror workflow read.
func TestGenerate_K3sDefaultIsThePin(t *testing.T) {
	root, files := generated(t)
	pin, err := readPin(filepath.Join(root, "pkg", "provision", "config", "k3s.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	for rel, data := range files {
		if !strings.HasPrefix(rel, schemaDir+"/") {
			continue
		}
		if bytes.Contains(data, []byte("__K3S_TAG__")) {
			t.Errorf("%s: placeholder left in place", rel)
		}
		if !bytes.Contains(data, []byte(`"default": `+jsonString(pin.Version))) {
			t.Errorf("%s: no default %s", rel, pin.Version)
		}
	}
}

type collisionCommon struct {
	Name string `yaml:"name"`
}

type collisionA struct {
	collisionCommon `yaml:",inline"`
	DiskSize        string `yaml:"diskSize"`
	Dir             string `yaml:"-"`
}

type collisionB struct {
	collisionCommon `yaml:",inline"`
	DiskSize        string `yaml:"diskSize"`
	Dir             string `yaml:"-"`
}

type collisionC struct {
	collisionCommon `yaml:",inline"`
	ServerDisk      string `yaml:"serverDisk"`
	Dir             string `yaml:"-"`
}

// One yaml key means one thing across providers: a name two providers
// both declare either belongs in CommonConfig or needs two names.
func TestCheckCollisions(t *testing.T) {
	err := checkCollisions([]providerTarget{
		{provider: "a", sample: &collisionA{}},
		{provider: "b", sample: &collisionB{}},
	})
	if err == nil {
		t.Fatal("two providers declaring diskSize went unnoticed")
	}
	for _, want := range []string{`"diskSize"`, `"a"`, `"b"`} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error should name %s: %v", want, err)
		}
	}

	// Embedded (shared) fields and yaml:"-" fields are not
	// collisions.
	if err := checkCollisions([]providerTarget{
		{provider: "a", sample: &collisionA{}},
		{provider: "c", sample: &collisionC{}},
	}); err != nil {
		t.Errorf("unexpected collision: %v", err)
	}
}
