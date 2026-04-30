package config

import (
	"fmt"
	"os"
	"path/filepath"

	"sigs.k8s.io/yaml"

	"github.com/Yolean/y-cluster/pkg/configfile"
)

// ProvisionFilename is the conventional file every `-c <dir>` for
// `y-cluster provision` reads. Mirrors y-cluster-serve.yaml on the
// serve side.
const ProvisionFilename = "y-cluster-provision.yaml"

// LoadProvision reads `<dir>/y-cluster-provision.yaml`, peeks the
// `provider:` discriminator, and dispatches to the matching typed
// loader. Returns a pointer to the concrete provider config (e.g.
// *QEMUConfig) so callers can switch on it.
//
// The peek uses non-strict YAML decoding so unknown fields (the
// per-provider settings) are tolerated; once we know the provider,
// the typed loader runs with the package's strict-decode contract
// and rejects unknown keys properly.
func LoadProvision(dir string) (any, error) {
	abs, err := filepath.Abs(dir)
	if err != nil {
		return nil, fmt.Errorf("resolve %s: %w", dir, err)
	}
	path := filepath.Join(abs, ProvisionFilename)
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", path, err)
	}
	var hdr struct {
		Provider string `yaml:"provider"`
	}
	if err := yaml.Unmarshal(data, &hdr); err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}

	provider := hdr.Provider
	if provider == "" {
		// Run discovery only when the file omits provider:; an
		// explicit-but-wrong value still falls through to the
		// default branch with a clear error.
		provider = DiscoverProvider()
		if provider == "" {
			return nil, fmt.Errorf(
				"%s: provider unset and discovery found none of multipass "+
					"(needs `multipass version`), qemu (needs /dev/kvm + "+
					"qemu-system-x86_64 on Linux), or docker (needs "+
					"`docker info` to succeed); set `provider:` in the config",
				path)
		}
	}

	switch provider {
	case ProviderQEMU:
		var c QEMUConfig
		if err := configfile.Load(dir, ProvisionFilename, &c); err != nil {
			return nil, err
		}
		return &c, nil
	case ProviderDocker:
		var c DockerConfig
		if err := configfile.Load(dir, ProvisionFilename, &c); err != nil {
			return nil, err
		}
		return &c, nil
	case ProviderMultipass:
		var c MultipassConfig
		if err := configfile.Load(dir, ProvisionFilename, &c); err != nil {
			return nil, err
		}
		return &c, nil
	default:
		return nil, fmt.Errorf("%s: unknown provider %q (supported: docker, multipass, qemu)", path, hdr.Provider)
	}
}
