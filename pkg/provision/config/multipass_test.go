package config

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Yolean/y-cluster/pkg/configfile"
)

func TestMultipass_ApplyDefaults_Empty(t *testing.T) {
	c := &MultipassConfig{CommonConfig: CommonConfig{Provider: ProviderMultipass}}
	c.ApplyDefaults()

	if c.Name != "y-cluster" {
		t.Errorf("Name: %q", c.Name)
	}
	if c.Image != "24.04" {
		t.Errorf("Image: %q", c.Image)
	}
	if c.Memory != "8192" {
		t.Errorf("Memory: %q", c.Memory)
	}
	if c.CPUs != "4" {
		t.Errorf("CPUs: %q", c.CPUs)
	}
	if c.Context != "local" {
		t.Errorf("Context: %q", c.Context)
	}
	if c.K3s.Install != "script" {
		t.Errorf("K3s.Install default for multipass should be script, got %q", c.K3s.Install)
	}
	if c.K3s.Version != K3sDefaultVersion() {
		t.Errorf("K3s.Version: %q vs pin %q", c.K3s.Version, K3sDefaultVersion())
	}
	// Multipass dials the VM IP directly; the qemu/docker default
	// triple [6443, 80, 443] has no operational meaning for it,
	// and ApplyDefaults clears the slice when the user didn't ask
	// for any forwards.
	if len(c.PortForwards) != 0 {
		t.Errorf("PortForwards should default to empty for multipass, got %v", c.PortForwards)
	}
}

func TestMultipass_ApplyDefaults_PreservesExplicitInstall(t *testing.T) {
	c := &MultipassConfig{
		CommonConfig: CommonConfig{
			Provider: ProviderMultipass,
			K3s:      K3sConfig{Install: "airgap"},
		},
	}
	c.ApplyDefaults()
	if c.K3s.Install != "airgap" {
		t.Fatalf("explicit Install overridden: %q", c.K3s.Install)
	}
}

func TestMultipass_ApplyDefaults_PreservesExplicitForwards(t *testing.T) {
	c := &MultipassConfig{
		CommonConfig: CommonConfig{
			Provider:     ProviderMultipass,
			PortForwards: []PortForward{{Host: "9090", Guest: "9090"}},
		},
	}
	c.ApplyDefaults()
	if len(c.PortForwards) != 1 || c.PortForwards[0].Guest != "9090" {
		t.Fatalf("explicit PortForwards overridden: %v", c.PortForwards)
	}
}

func TestMultipass_Validate_DropsHostAPIPortRequirement(t *testing.T) {
	// A defaulted MultipassConfig has no PortForwards. Validate
	// must succeed -- multipass dials the VM IP directly and
	// doesn't need a guest:6443 host forward.
	c := &MultipassConfig{CommonConfig: CommonConfig{Provider: ProviderMultipass}}
	c.ApplyDefaults()
	if err := c.Validate(); err != nil {
		t.Fatalf("Validate on defaulted multipass config: %v", err)
	}
}

func TestMultipass_Validate_Provider(t *testing.T) {
	c := &MultipassConfig{CommonConfig: CommonConfig{Provider: "qemu"}}
	c.ApplyDefaults()
	err := c.Validate()
	if err == nil || !strings.Contains(err.Error(), "multipass") {
		t.Fatalf("want provider error, got %v", err)
	}
}

func TestMultipass_Validate_Install(t *testing.T) {
	c := &MultipassConfig{CommonConfig: CommonConfig{
		Provider: ProviderMultipass,
		K3s:      K3sConfig{Install: "lol"},
	}}
	c.ApplyDefaults()
	err := c.Validate()
	if err == nil || !strings.Contains(err.Error(), "install") {
		t.Fatalf("want install error, got %v", err)
	}
}

func TestMultipass_Load_HappyPath(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "y-cluster-provision.yaml"),
		[]byte("provider: multipass\nname: foo\nmemory: '12288'\nimage: jammy\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	var c MultipassConfig
	if err := configfile.Load(dir, "y-cluster-provision.yaml", &c); err != nil {
		t.Fatal(err)
	}
	if c.Name != "foo" {
		t.Fatalf("Name: %q", c.Name)
	}
	if c.Memory != "12288" {
		t.Fatalf("Memory: %q", c.Memory)
	}
	if c.Image != "jammy" {
		t.Fatalf("Image: %q", c.Image)
	}
	if c.Dir == "" {
		t.Fatal("Dir not set")
	}
}

func TestMultipass_Load_RejectsUnknownField(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "y-cluster-provision.yaml"),
		[]byte("provider: multipass\nbogus: 1\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	var c MultipassConfig
	err := configfile.Load(dir, "y-cluster-provision.yaml", &c)
	if err == nil || !strings.Contains(err.Error(), "unknown field") {
		t.Fatalf("want unknown-field error, got %v", err)
	}
}

// TestMultipassSchemaIsCanonical mirrors TestSchemaIsCanonical for
// the multipass provider: parses the committed schema, checks the
// pin-driven default and the const narrowing on `provider`.
func TestMultipassSchemaIsCanonical(t *testing.T) {
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

	schemaPath := filepath.Join(repoRoot, "pkg", "provision", "schema", "multipass.schema.json")
	have, err := os.ReadFile(schemaPath)
	if err != nil {
		t.Fatal(err)
	}
	var s map[string]any
	if err := json.Unmarshal(have, &s); err != nil {
		t.Fatalf("schema does not parse: %v", err)
	}
	str := string(have)
	if !strings.Contains(str, K3sDefaultVersion()) {
		t.Fatal("schema missing k3s tag default")
	}
	if !strings.Contains(str, `"const": "multipass"`) {
		t.Fatal(`multipass.schema.json missing "const": "multipass" on provider`)
	}
	// qemu-only fields must not bleed into the multipass schema.
	for _, k := range []string{`"sshPort"`, `"diskSize"`} {
		if strings.Contains(str, k) {
			t.Fatalf("multipass.schema.json must not include qemu-specific field %s", k)
		}
	}
}

func TestLoadProvision_Multipass(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "y-cluster-provision.yaml"),
		[]byte("provider: multipass\nname: bar\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	got, err := LoadProvision(dir)
	if err != nil {
		t.Fatal(err)
	}
	c, ok := got.(*MultipassConfig)
	if !ok {
		t.Fatalf("type %T", got)
	}
	if c.Name != "bar" {
		t.Fatalf("Name: %q", c.Name)
	}
	if c.Provider != ProviderMultipass {
		t.Fatalf("Provider: %q", c.Provider)
	}
}
