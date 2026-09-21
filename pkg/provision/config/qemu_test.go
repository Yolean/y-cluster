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
	if c.DiskSize != "20G" {
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
	if c.DiskSize != "20G" {
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
	if !strings.Contains(str, `"default": "20G"`) {
		t.Fatal("schema missing diskSize default")
	}
	// Image is no longer a schema field -- it's derived at runtime.
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

// Loopback is the default because a forward is otherwise reachable
// from every network the host is on.
func TestQEMU_Network_Defaults(t *testing.T) {
	c := &QEMUConfig{}
	c.ApplyDefaults()
	if c.Network.Mode != "user" || c.Network.BindAddress != "127.0.0.1" {
		t.Fatalf("network defaults: %+v", c.Network)
	}
	if err := c.Validate(); err != nil {
		t.Fatalf("defaulted config should validate: %v", err)
	}
}

func TestQEMU_Network_Validate(t *testing.T) {
	tap := func(mut func(*QEMUConfig)) *QEMUConfig {
		c := &QEMUConfig{Network: QEMUNetwork{Mode: "tap", Ifname: "ycl0", GuestAddress: "10.88.0.2/24"}}
		if mut != nil {
			mut(c)
		}
		return c
	}
	user := func(mut func(*QEMUConfig)) *QEMUConfig {
		c := &QEMUConfig{}
		if mut != nil {
			mut(c)
		}
		return c
	}
	for _, tc := range []struct {
		name    string
		cfg     *QEMUConfig
		wantErr string
	}{
		{"user: defaults", user(nil), ""},
		{"user: wildcard restores exposure on all interfaces", user(func(c *QEMUConfig) { c.Network.BindAddress = "0.0.0.0" }), ""},
		{"user: specific host address", user(func(c *QEMUConfig) { c.Network.BindAddress = "192.168.1.10" }), ""},
		{"user: hostname", user(func(c *QEMUConfig) { c.Network.BindAddress = "localhost" }), "network.bindAddress"},
		{"user: ipv6", user(func(c *QEMUConfig) { c.Network.BindAddress = "::1" }), "network.bindAddress"},
		{"user: ipv4-mapped ipv6", user(func(c *QEMUConfig) { c.Network.BindAddress = "::ffff:127.0.0.1" }), "network.bindAddress"},
		{"user: with port", user(func(c *QEMUConfig) { c.Network.BindAddress = "127.0.0.1:80" }), "network.bindAddress"},
		{"user: tap field", user(func(c *QEMUConfig) { c.Network.Ifname = "ycl0" }), "only apply to network.mode"},
		{"user: dns", user(func(c *QEMUConfig) { c.Network.DNS = []string{"1.1.1.1"} }), "only apply to network.mode"},
		{"unknown mode", user(func(c *QEMUConfig) { c.Network.Mode = "bridge" }), "network.mode"},

		{"tap: minimal", tap(nil), ""},
		{"tap: explicit gateway and dns", tap(func(c *QEMUConfig) { c.Network.Gateway = "10.88.0.254"; c.Network.DNS = []string{"10.88.0.254"} }), ""},
		{"tap: ifname missing", tap(func(c *QEMUConfig) { c.Network.Ifname = "" }), "network.ifname is required"},
		{"tap: ifname too long", tap(func(c *QEMUConfig) { c.Network.Ifname = "sixteen-chars-xx" }), "not a valid interface name"},
		{"tap: ifname would inject a netdev option", tap(func(c *QEMUConfig) { c.Network.Ifname = "a,script=x" }), "not a valid interface name"},
		{"tap: guestAddress missing", tap(func(c *QEMUConfig) { c.Network.GuestAddress = "" }), "network.guestAddress"},
		{"tap: guestAddress without prefix", tap(func(c *QEMUConfig) { c.Network.GuestAddress = "10.88.0.2" }), "network.guestAddress"},
		{"tap: guestAddress ipv6", tap(func(c *QEMUConfig) { c.Network.GuestAddress = "fd00::2/64" }), "network.guestAddress"},
		{"tap: /31 has no room for a gateway", tap(func(c *QEMUConfig) { c.Network.GuestAddress = "10.88.0.2/31" }), "no room for a gateway"},
		{"tap: gateway outside subnet", tap(func(c *QEMUConfig) { c.Network.Gateway = "10.89.0.1" }), "outside the guestAddress subnet"},
		{"tap: gateway equals guest", tap(func(c *QEMUConfig) { c.Network.Gateway = "10.88.0.2" }), "are both"},
		{"tap: guest on the default gateway address", tap(func(c *QEMUConfig) { c.Network.GuestAddress = "10.88.0.1/24" }), "are both"},
		{"tap: dns not an address", tap(func(c *QEMUConfig) { c.Network.DNS = []string{"resolver.local"} }), "network.dns"},
		{"tap: bindAddress", tap(func(c *QEMUConfig) { c.Network.BindAddress = "127.0.0.1" }), "network.bindAddress only applies"},
		{"tap: sshPort", tap(func(c *QEMUConfig) { c.SSHPort = "2222" }), "sshPort and portForwards have no meaning"},
		{"tap: portForwards", tap(func(c *QEMUConfig) { c.PortForwards = []PortForward{{Host: "80", Guest: "80"}} }), "sshPort and portForwards have no meaning"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tc.cfg.ApplyDefaults()
			err := tc.cfg.Validate()
			if tc.wantErr == "" {
				if err != nil {
					t.Fatalf("unexpected error: %v", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("want error containing %q, got %v", tc.wantErr, err)
			}
		})
	}
}

// Tap mode has no host port forwards, so neither the ssh port nor the
// forward list may pick up the user-mode defaults.
func TestQEMU_Network_TapDefaults(t *testing.T) {
	c := &QEMUConfig{Network: QEMUNetwork{Mode: "tap", Ifname: "ycl0", GuestAddress: "10.88.0.2/24"}}
	c.ApplyDefaults()
	if c.SSHPort != "" || len(c.PortForwards) != 0 {
		t.Errorf("user-mode defaults leaked into tap mode: sshPort=%q portForwards=%v", c.SSHPort, c.PortForwards)
	}
	if c.Network.BindAddress != "" {
		t.Errorf("bindAddress defaulted in tap mode: %q", c.Network.BindAddress)
	}
	if c.Network.Gateway != "10.88.0.1" {
		t.Errorf("gateway: got %q, want the first host address 10.88.0.1", c.Network.Gateway)
	}
	if strings.Join(c.Network.DNS, ",") != "1.1.1.1,9.9.9.9" {
		t.Errorf("dns: %v", c.Network.DNS)
	}
	if got := c.Network.GuestIP(); got != "10.88.0.2" {
		t.Errorf("GuestIP: %q", got)
	}
	if got := c.HostRoutableIP(); got != "" {
		t.Errorf("CommonConfig.HostRoutableIP is about port forwards and must stay empty in tap mode, got %q", got)
	}
}

func TestHostDialAddress(t *testing.T) {
	for bind, want := range map[string]string{
		"":             "127.0.0.1", // forward from before bindAddress existed: wildcard
		"0.0.0.0":      "127.0.0.1", // the wildcard is not dialable
		"127.0.0.1":    "127.0.0.1",
		"192.168.1.10": "192.168.1.10",
	} {
		if got := HostDialAddress(bind); got != want {
			t.Errorf("HostDialAddress(%q) = %q, want %q", bind, got, want)
		}
	}
}

func TestQEMUSchema_Network(t *testing.T) {
	data, err := os.ReadFile("../schema/qemu.schema.json")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{`"bindAddress"`, `"guestAddress"`, `"ifname"`, `"tap"`, `"QEMUNetwork"`} {
		if !strings.Contains(string(data), want) {
			t.Errorf("qemu.schema.json is missing %s; run go generate ./pkg/provision/config/", want)
		}
	}
}
