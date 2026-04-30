package config

// MultipassConfig is the on-disk shape of `y-cluster-provision.yaml`
// when `provider: multipass`. Multipass owns the hypervisor and CPU
// / RAM / disk are first-class CLI flags, so the provider-specific
// surface is small. Everything else (Name, Memory, CPUs, K3s,
// Registries, Gateway) comes from CommonConfig and uses the same
// keys as qemu/docker.
//
// There's no CacheDir field: the cloud-init seed is staged in
// os.TempDir() (it's only read by `multipass launch` once), the
// k3s airgap cache is shared at pkg/cache.K3s, and the kubeconfig
// is managed by pkg/kubeconfig. None of those are per-VM host-side
// state worth surfacing in the schema.
type MultipassConfig struct {
	CommonConfig `yaml:",inline" json:",inline"`

	Image string `yaml:"image,omitempty" json:"image,omitempty" jsonschema:"default=24.04,description=Multipass image alias passed as the launch positional argument. LTS aliases (24.04, jammy) work; daily:noble and file:///path/to/img also work."`

	// Dir is filled at load time. Not part of the schema.
	Dir string `yaml:"-" json:"-" jsonschema:"-"`
}

// SetDir satisfies configfile.DirAware.
func (c *MultipassConfig) SetDir(dir string) { c.Dir = dir }

// ApplyDefaults satisfies configfile.Defaulter. Two divergences
// from the qemu/docker shape:
//
//   - PortForwards: multipass dials the VM IP directly, so the
//     [6443, 80, 443] common default has no operational meaning.
//     If the user didn't spell out their own forwards, clear the
//     slice that applyCommonDefaults installed.
//   - K3s.Install: the common K3sConfig tag default is `airgap`
//     (geared at qemu where the VM may have flaky outbound).
//     Multipass VMs have first-class outbound through the host's
//     network stack, so the curl|sh installer is faster and avoids
//     the airgap dance. Only override when the user didn't pin
//     a value explicitly.
func (c *MultipassConfig) ApplyDefaults() {
	if c.Provider == "" {
		c.Provider = ProviderMultipass
	}
	hadExplicitForwards := len(c.PortForwards) > 0
	hadExplicitInstall := c.K3s.Install != ""
	applyTagDefaults(c)
	c.applyCommonDefaults()
	if !hadExplicitForwards && isDefaultPortForwards(c.PortForwards) {
		c.PortForwards = nil
	}
	if !hadExplicitInstall {
		c.K3s.Install = "script"
	}
}

// Validate checks the discriminator and multipass-specific
// invariants. PortForwards is intentionally not enforced -- the
// host dials the VM IP directly, so kubectl reachability doesn't
// depend on a guest:6443 forward.
func (c *MultipassConfig) Validate() error {
	if err := c.validateCommon(ProviderMultipass); err != nil {
		return err
	}
	if c.Image == "" {
		return errInvalid("image must not be empty after defaults")
	}
	switch c.K3s.Install {
	case "", "airgap", "script":
	default:
		return errInvalid("k3s.install must be one of {airgap, script}, got %q", c.K3s.Install)
	}
	return nil
}

// isDefaultPortForwards reports whether forwards is exactly the
// shape applyCommonDefaults installs when the user supplies no
// portForwards. Used by MultipassConfig.ApplyDefaults to undo that
// install since multipass doesn't tunnel through the host.
func isDefaultPortForwards(forwards []PortForward) bool {
	if len(forwards) != 3 {
		return false
	}
	want := []PortForward{
		{Host: "6443", Guest: "6443"},
		{Host: "80", Guest: "80"},
		{Host: "443", Guest: "443"},
	}
	for i, pf := range forwards {
		if pf != want[i] {
			return false
		}
	}
	return true
}
