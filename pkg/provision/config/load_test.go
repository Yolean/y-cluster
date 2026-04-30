package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeProvision(t *testing.T, body string) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, ProvisionFilename), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	return dir
}

func TestLoadProvision_QEMU(t *testing.T) {
	dir := writeProvision(t, "provider: qemu\nname: foo\n")
	got, err := LoadProvision(dir)
	if err != nil {
		t.Fatal(err)
	}
	c, ok := got.(*QEMUConfig)
	if !ok {
		t.Fatalf("type %T", got)
	}
	if c.Name != "foo" {
		t.Fatalf("Name: %q", c.Name)
	}
	if c.DiskSize != "10G" {
		t.Fatalf("default DiskSize missing: %q", c.DiskSize)
	}
	if c.Dir == "" {
		t.Fatal("Dir not set")
	}
}

// TestLoadProvision_MissingProvider_DiscoveryFails forces
// discovery to return "" so we can assert the error message
// names what was probed. On a real host discovery often
// succeeds; the override is the only way to exercise the
// empty path deterministically.
func TestLoadProvision_MissingProvider_DiscoveryFails(t *testing.T) {
	prev := DiscoverProviderFn
	DiscoverProviderFn = func() string { return "" }
	t.Cleanup(func() { DiscoverProviderFn = prev })

	dir := writeProvision(t, "name: foo\n")
	_, err := LoadProvision(dir)
	if err == nil {
		t.Fatal("want error when discovery returns empty")
	}
	for _, want := range []string{"discovery", "qemu", "docker", "multipass", "set `provider:`"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("error should mention %q, got %v", want, err)
		}
	}
}

// TestLoadProvision_MissingProvider_DiscoveryFillsIn covers the
// happy path: with no provider: in the YAML, discovery picks
// one and the typed loader runs. We force discovery to return
// docker so we don't depend on the test host having /dev/kvm.
func TestLoadProvision_MissingProvider_DiscoveryFillsIn(t *testing.T) {
	prev := DiscoverProviderFn
	DiscoverProviderFn = func() string { return ProviderDocker }
	t.Cleanup(func() { DiscoverProviderFn = prev })

	dir := writeProvision(t, "name: foo\nportForwards:\n- {host: '36443', guest: '6443'}\n")
	got, err := LoadProvision(dir)
	if err != nil {
		t.Fatalf("LoadProvision: %v", err)
	}
	c, ok := got.(*DockerConfig)
	if !ok {
		t.Fatalf("expected *DockerConfig, got %T", got)
	}
	if c.Provider != ProviderDocker {
		t.Fatalf("Provider not filled in by ApplyDefaults: %q", c.Provider)
	}
}

func TestLoadProvision_UnknownProvider(t *testing.T) {
	dir := writeProvision(t, "provider: nonexistent\n")
	_, err := LoadProvision(dir)
	if err == nil || !strings.Contains(err.Error(), "unknown provider") {
		t.Fatalf("want unknown-provider error, got %v", err)
	}
}

func TestLoadProvision_StrictDecodeRejectsUnknownFields(t *testing.T) {
	dir := writeProvision(t, "provider: qemu\nrandomField: 1\n")
	_, err := LoadProvision(dir)
	if err == nil || !strings.Contains(err.Error(), "unknown field") {
		t.Fatalf("want unknown-field error, got %v", err)
	}
}

func TestLoadProvision_MissingFile(t *testing.T) {
	dir := t.TempDir()
	_, err := LoadProvision(dir)
	if err == nil {
		t.Fatal("want missing-file error")
	}
}

func TestLoadProvision_InvalidYAML(t *testing.T) {
	dir := writeProvision(t, "provider: [oops\n")
	_, err := LoadProvision(dir)
	if err == nil {
		t.Fatal("want parse error")
	}
}

func TestLoadProvision_ValidationFailure(t *testing.T) {
	dir := writeProvision(t, "provider: qemu\nk3s:\n  install: bogus\n")
	_, err := LoadProvision(dir)
	if err == nil || !strings.Contains(err.Error(), "install") {
		t.Fatalf("want validation error, got %v", err)
	}
}
