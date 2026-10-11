package config

import (
	"strings"
	"testing"
)

// TestCertManager_InstalledByDefault: a config that says nothing about
// cert-manager gets it on the providers that install it. Exercised
// through each provider's ApplyDefaults, which is what a loaded config
// goes through, so a tag default that flips Skip would show here.
func TestCertManager_InstalledByDefault(t *testing.T) {
	for name, defaulted := range map[string]func() CertManagerConfig{
		"qemu": func() CertManagerConfig {
			c := &QEMUConfig{}
			c.ApplyDefaults()
			return c.CertManager
		},
		"docker": func() CertManagerConfig {
			c := &DockerConfig{}
			c.ApplyDefaults()
			return c.CertManager
		},
	} {
		if got := defaulted(); got.Skip {
			t.Errorf("%s: Skip should default to false, got %+v", name, got)
		}
	}
}

// TestLoadProvision_CertManagerSkip: certManager.skip is read from the
// file, for both providers that install cert-manager, and survives
// defaulting and validation.
func TestLoadProvision_CertManagerSkip(t *testing.T) {
	for _, provider := range []string{ProviderQEMU, ProviderDocker} {
		t.Run(provider, func(t *testing.T) {
			dir := writeProvision(t, "provider: "+provider+"\ncertManager:\n  skip: true\n")
			got, err := LoadProvision(dir)
			if err != nil {
				t.Fatal(err)
			}
			if !got.Common().CertManager.Skip {
				t.Fatalf("certManager.skip: true did not load: %+v", got.Common().CertManager)
			}
			// The two installs are independent: skipping cert-manager
			// leaves the gateway install and its defaults alone.
			if gw := got.Common().Gateway; gw.Skip || gw.ClassName != "y-cluster" {
				t.Fatalf("gateway changed by certManager.skip: %+v", gw)
			}
		})
	}
}

// TestLoadProvision_CertManagerExplicitFalse: skip: false is the
// default spelled out, and loads as such.
func TestLoadProvision_CertManagerExplicitFalse(t *testing.T) {
	dir := writeProvision(t, "provider: docker\ncertManager:\n  skip: false\n")
	got, err := LoadProvision(dir)
	if err != nil {
		t.Fatal(err)
	}
	if got.Common().CertManager.Skip {
		t.Fatal("certManager.skip: false loaded as true")
	}
}

// TestLoadProvision_CertManagerStrictKeys: the key is camelCase like
// the config's other keys, and the strict decode rejects a guessed
// spelling rather than installing cert-manager anyway. (Matching is
// case-insensitive, as for every key: certmanager loads.)
func TestLoadProvision_CertManagerStrictKeys(t *testing.T) {
	for _, body := range []string{
		"provider: docker\ncert-manager:\n  skip: true\n",
		"provider: qemu\ncertManager:\n  enabled: false\n",
	} {
		_, err := LoadProvision(writeProvision(t, body))
		if err == nil || !strings.Contains(err.Error(), "unknown field") {
			t.Errorf("%q: want unknown-field error, got %v", body, err)
		}
	}
}
