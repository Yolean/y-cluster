// Package config defines the typed `y-cluster-provision.yaml`
// surface for every provisioner, and the runtime that loads it.
//
// # Common vs provider-specific fields
//
// Every provider config struct embeds CommonConfig (in this file).
// The shared fields — provider discriminator, instance name,
// kubeconfig context, memory, cpus, k3s install settings — keep
// their YAML keys identical across providers so a user can switch
// provider: qemu → provider: docker without renaming anything else
// in the file.
//
// Provider-specific fields live on the per-provider struct (qemu.go,
// docker.go). The schemagen step in cmd/internal/schemagen validates
// that no two providers declare the same own (non-embedded) yaml key,
// so a future "Memory" knob can't accidentally drift into one
// provider's specific stanza.
//
// # Generated schemas
//
// schemagen emits one schema per provider plus a portable
// common.schema.json:
//
//   - qemu.schema.json: full QEMUConfig surface;
//     `provider` narrowed to const "qemu"
//   - docker.schema.json: full DockerConfig surface;
//     `provider` narrowed to const "docker"
//   - common.schema.json: just CommonConfig; `provider` is the enum
//     of all known providers, so a config that uses only common
//     keys is portable across providers
package config

// Provider IDs. Single source of truth for both the per-provider
// `Validate()` checks and the `enum` constraint on
// CommonConfig.Provider — schemagen reads AllProviders to build
// the enum, and per-provider schema post-processing replaces it
// with a const constraint.
const (
	ProviderQEMU   = "qemu"
	ProviderDocker = "docker"
)

// AllProviders is the canonical list, sorted, used by schemagen for
// the common-schema enum and by error messages that need to list
// supported values.
var AllProviders = []string{ProviderDocker, ProviderQEMU}

// CommonConfig is the portable subset of `y-cluster-provision.yaml`.
// Every provider config embeds it via `yaml:",inline"` so the keys
// surface at the top level of the file. Adding a field here adds it
// to every provider's schema and to common.schema.json.
//
// Per-provider Validate() must call validateCommon to enforce the
// shared invariants (provider discriminator, k3s.version present).
type CommonConfig struct {
	Provider     string        `yaml:"provider"               json:"provider"               jsonschema:"description=Provisioner to use. Per-provider schemas narrow this to a single literal."`
	Name         string        `yaml:"name,omitempty"         json:"name,omitempty"         jsonschema:"default=y-cluster,description=Cluster instance identifier; used as the docker container name / qemu -name / kubeconfig cluster name / prefix for cache files."`
	Context      string        `yaml:"context,omitempty"      json:"context,omitempty"      jsonschema:"default=local,description=kubeconfig context name to write."`
	Memory       string        `yaml:"memory,omitempty"       json:"memory,omitempty"       jsonschema:"default=8192,description=Memory in MB. qemu allocates this to the VM; docker passes it to --memory."`
	CPUs         string        `yaml:"cpus,omitempty"         json:"cpus,omitempty"         jsonschema:"default=4,description=vCPU count. qemu sets -smp; docker passes --cpus."`
	K3s          K3sConfig     `yaml:"k3s,omitempty"          json:"k3s,omitempty"          jsonschema:"description=k3s install settings. Defaults track pkg/provision/config/k3s.yaml."`
	PortForwards []PortForward `yaml:"portForwards,omitempty" json:"portForwards,omitempty" jsonschema:"description=Host->guest TCP port forwards. Defaults to 6443/80/443 when omitted. Must include a guest:6443 entry so the host's kubectl can reach the API server."`
	Registries   Registries    `yaml:"registries,omitempty"   json:"registries,omitempty"   jsonschema:"description=k3s registries.yaml content. Written to /etc/rancher/k3s/registries.yaml on the node before k3s starts. ${VAR} substitution is supported on credential and endpoint fields."`
}

// PortForward maps a host port to a guest port. Common to all
// providers: qemu uses it for SLIRP -netdev hostfwd, docker uses
// it for container PortBindings.
type PortForward struct {
	Host  string `yaml:"host"  json:"host"  jsonschema:"description=Host port. Empty string lets the provider pick (qemu: SLIRP-assigned; docker: docker-assigned)."`
	Guest string `yaml:"guest" json:"guest" jsonschema:"description=Guest port to forward to."`
}

// HostAPIPort returns the host-side port mapped to guest 6443.
// Provisioners use this to surface the kubectl-facing endpoint:
// qemu rewrites the extracted kubeconfig server URL, docker does
// the same after the container starts. Empty string means there
// is no 6443 forward defined; Validate guards against this.
func (c CommonConfig) HostAPIPort() string {
	for _, pf := range c.PortForwards {
		if pf.Guest == "6443" {
			return pf.Host
		}
	}
	return ""
}

// K3sConfig controls the k3s install. The container image is
// **not** a config field — it's derived from Version at runtime
// (MirrorImage / UpstreamImage in defaults.go) so a Version bump
// is the only edit required to switch k3s versions. The docker
// provisioner additionally probes the mirror at provision time and
// falls back to the upstream rancher/k3s image with a warning when
// the mirror has no manifest yet (typical when testing a freshly
// released version before the mirror workflow has run).
//
// Version's default is filled by schemagen from
// pkg/provision/config/k3s.yaml so a single tag bump in that file
// flows to: GHA mirror, schema default, runtime default.
type K3sConfig struct {
	Version string `yaml:"version,omitempty" json:"version,omitempty" jsonschema:"default=__K3S_TAG__,description=k3s release version e.g. vX.Y.Z+k3sN."`
	Install string `yaml:"install,omitempty" json:"install,omitempty" jsonschema:"enum=airgap,enum=script,default=airgap,description=Install strategy. airgap pre-loads images on the node; script downloads via get.k3s.io. qemu only."`
}

// applyCommonDefaults fills defaults that the reflective tag-default
// pass can't reach: K3s.Version (data-file driven) and PortForwards
// (slice default).
func (c *CommonConfig) applyCommonDefaults() {
	if c.K3s.Version == "" {
		c.K3s.Version = K3sDefaultVersion()
	}
	if len(c.PortForwards) == 0 {
		// y-cluster convention: API + ingress HTTP/HTTPS.
		// A user who wants a different shape can spell out their
		// own portForwards: list and replace this default
		// wholesale (including 6443, which Validate then enforces).
		c.PortForwards = []PortForward{
			{Host: "6443", Guest: "6443"},
			{Host: "80", Guest: "80"},
			{Host: "443", Guest: "443"},
		}
	}
}

// validateCommon checks invariants every provider relies on. The
// per-provider Validate methods call this first, then check their
// own fields.
func (c *CommonConfig) validateCommon(expected string) error {
	if c.Provider != expected {
		return errInvalid("provider must be %q, got %q", expected, c.Provider)
	}
	if c.K3s.Version == "" {
		return errInvalid("k3s.version is empty; check pkg/provision/config/k3s.yaml")
	}
	if c.HostAPIPort() == "" {
		return errInvalid("portForwards must include a guest:6443 entry to reach k3s from the host")
	}
	return nil
}
