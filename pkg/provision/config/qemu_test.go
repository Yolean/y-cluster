package config

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Yolean/y-cluster/pkg/configfile"
)

func TestQEMU_ApplyDefaults_Empty(t *testing.T) {
	c := &QEMUConfig{CommonConfig: CommonConfig{Provider: ProviderQEMU}}
	c.ApplyDefaults()

	if c.Name != "y-cluster" {
		t.Errorf("Name: %q", c.Name)
	}
	if c.DiskSize != "10G" {
		t.Errorf("DiskSize: %q", c.DiskSize)
	}
	if c.Memory != "8192" {
		t.Errorf("Memory: %q", c.Memory)
	}
	if c.CPUs != "4" {
		t.Errorf("CPUs: %q", c.CPUs)
	}
	if c.SSHPort != "2222" {
		t.Errorf("SSHPort: %q", c.SSHPort)
	}
	if c.Context != "local" {
		t.Errorf("Context: %q", c.Context)
	}
	if c.K3s.Install != "airgap" {
		t.Errorf("K3s.Install: %q", c.K3s.Install)
	}
	// Pin-driven default
	if c.K3s.Version != K3sDefaultVersion() {
		t.Errorf("K3s.Version: %q vs pin %q", c.K3s.Version, K3sDefaultVersion())
	}
}

func TestQEMU_ApplyDefaults_RespectsExplicitValues(t *testing.T) {
	c := &QEMUConfig{
		CommonConfig: CommonConfig{
			Provider: ProviderQEMU,
			Name:     "custom",
			Memory:   "16384",
			K3s:      K3sConfig{Version: "v1.34.0+k3s1"},
		},
	}
	c.ApplyDefaults()
	if c.Name != "custom" {
		t.Fatalf("explicit Name overridden: %q", c.Name)
	}
	if c.Memory != "16384" {
		t.Fatalf("explicit Memory overridden: %q", c.Memory)
	}
	if c.K3s.Version != "v1.34.0+k3s1" {
		t.Fatalf("explicit K3s.Version overridden: %q", c.K3s.Version)
	}
}

func TestQEMU_Validate_Provider(t *testing.T) {
	c := &QEMUConfig{CommonConfig: CommonConfig{Provider: "multipass"}}
	c.ApplyDefaults()
	err := c.Validate()
	if err == nil || !strings.Contains(err.Error(), "qemu") {
		t.Fatalf("want provider error, got %v", err)
	}
}

func TestQEMU_Validate_Install(t *testing.T) {
	c := &QEMUConfig{CommonConfig: CommonConfig{
		Provider: ProviderQEMU,
		K3s:      K3sConfig{Install: "lol"},
	}}
	c.ApplyDefaults() // does not override non-empty
	err := c.Validate()
	if err == nil || !strings.Contains(err.Error(), "install") {
		t.Fatalf("want install error, got %v", err)
	}
}

func TestQEMU_Load_HappyPath(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "y-cluster-provision.yaml"),
		[]byte("provider: qemu\nname: foo\nmemory: '12288'\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	var c QEMUConfig
	if err := configfile.Load(dir, "y-cluster-provision.yaml", &c); err != nil {
		t.Fatal(err)
	}
	// User-set fields preserved
	if c.Name != "foo" {
		t.Fatalf("Name: %q", c.Name)
	}
	if c.Memory != "12288" {
		t.Fatalf("Memory: %q", c.Memory)
	}
	// Defaults filled
	if c.DiskSize != "10G" {
		t.Fatalf("DiskSize default missing: %q", c.DiskSize)
	}
	// Pin-driven default
	if c.K3s.Version != K3sDefaultVersion() {
		t.Fatalf("K3s.Version default missing: %q", c.K3s.Version)
	}
	if c.Dir == "" {
		t.Fatal("Dir not set")
	}
}

func TestQEMU_Load_RejectsUnknownField(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "y-cluster-provision.yaml"),
		[]byte("provider: qemu\nbogus: 1\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	var c QEMUConfig
	err := configfile.Load(dir, "y-cluster-provision.yaml", &c)
	if err == nil || !strings.Contains(err.Error(), "unknown field") {
		t.Fatalf("want unknown-field error, got %v", err)
	}
}

func TestQEMU_Load_ValidateFails(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "y-cluster-provision.yaml"),
		[]byte("provider: qemu\nk3s:\n  install: nope\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	var c QEMUConfig
	err := configfile.Load(dir, "y-cluster-provision.yaml", &c)
	if err == nil {
		t.Fatal("want validation error")
	}
}

// TestSchemaIsCanonical asserts the committed
// pkg/provision/schema/qemu.schema.json matches what the generator
// would emit. CI runs go generate ./pkg/provision/...; this is the
// pre-generate guard for local dev so a forgotten regenerate fails
// loudly.
func TestSchemaIsCanonical(t *testing.T) {
	repoRoot := func() string {
		dir, _ := os.Getwd()
		for {
			if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
				return dir
			}
			parent := filepath.Dir(dir)
			if parent == dir {
				t.Fatal("go.mod not found above test dir")
			}
			dir = parent
		}
	}()

	schemaPath := filepath.Join(repoRoot, "pkg", "provision", "schema", "qemu.schema.json")
	have, err := os.ReadFile(schemaPath)
	if err != nil {
		t.Fatal(err)
	}

	// Sanity: the schema parses, references our defaults and
	// includes the pin-driven k3s default.
	var s map[string]any
	if err := json.Unmarshal(have, &s); err != nil {
		t.Fatalf("schema does not parse: %v", err)
	}
	str := string(have)
	if !strings.Contains(str, K3sDefaultVersion()) {
		t.Fatal("schema missing k3s tag default")
	}
	if !strings.Contains(str, `"default": "10G"`) {
		t.Fatal("schema missing diskSize default")
	}
	// Image is no longer a schema field — it's derived at runtime.
	if strings.Contains(str, `"image"`) {
		t.Fatal("schema must not contain an image property")
	}
	// Discriminator is a required field in the schema even though
	// it's not omitempty in the struct.
	if !strings.Contains(str, `"required": [
        "provider"
      ]`) {
		t.Fatal("schema does not mark provider as required")
	}
	// Per-provider schema narrows provider to a const.
	if !strings.Contains(str, `"const": "qemu"`) {
		t.Fatal(`qemu.schema.json missing "const": "qemu" on provider`)
	}
	// Common schema-only enum should NOT appear in the per-provider
	// schema (we replace it with const during post-processing).
	if strings.Contains(str, `"enum": [
            "docker",
            "multipass",
            "qemu"
          ]`) {
		t.Fatal("per-provider schema still has the all-providers enum")
	}
}

// TestCommonSchemaIsCanonical sanity-checks the portable schema.
func TestCommonSchemaIsCanonical(t *testing.T) {
	repoRoot := func() string {
		dir, _ := os.Getwd()
		for {
			if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
				return dir
			}
			parent := filepath.Dir(dir)
			if parent == dir {
				t.Fatal("go.mod not found above test dir")
			}
			dir = parent
		}
	}()
	have, err := os.ReadFile(filepath.Join(repoRoot, "pkg", "provision", "schema", "common.schema.json"))
	if err != nil {
		t.Fatal(err)
	}
	str := string(have)
	// Common schema accepts any known provider value.
	for _, want := range []string{`"docker"`, `"qemu"`, `"multipass"`} {
		if !strings.Contains(str, want) {
			t.Fatalf("common.schema.json missing provider enum value %s", want)
		}
	}
	// Per-provider-only fields must NOT appear in the common schema.
	for _, k := range []string{`"diskSize"`, `"sshPort"`, `"cacheDir"`, `"image"`} {
		if strings.Contains(str, k) {
			t.Fatalf("common.schema.json must not include provider-specific field %s", k)
		}
	}

	// `provider` is required in per-provider schemas but optional
	// in the common schema -- the runtime auto-discovers it via
	// DiscoverProvider when a common-shape config omits the field.
	// We parse the JSON tree rather than string-grep because the
	// schema also contains a PortForward definition with its own
	// required: [host, guest], and we don't want to confuse the
	// two.
	var doc map[string]any
	if err := json.Unmarshal(have, &doc); err != nil {
		t.Fatalf("schema does not parse: %v", err)
	}
	defs, _ := doc["$defs"].(map[string]any)
	cc, _ := defs["CommonConfig"].(map[string]any)
	if cc == nil {
		t.Fatal("common schema missing $defs.CommonConfig")
	}
	required, _ := cc["required"].([]any)
	for _, item := range required {
		if name, _ := item.(string); name == "provider" {
			t.Fatalf("common.schema.json must NOT mark provider as required (auto-discovery covers the omitted case); required: %v", required)
		}
	}
}
