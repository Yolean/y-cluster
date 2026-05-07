package config

import (
	"os"
	"regexp"
)

// HetznerConfig is the on-disk shape of `y-cluster-provision.yaml`
// when `provider: hetzner`. CommonConfig carries portable fields;
// the fields below are Hetzner-specific.
//
// See specs/y-cluster/HETZNER_PROVISIONER.md for the dev-cluster
// shape this provisioner targets and the rationale behind the
// validation rules + defaults.
type HetznerConfig struct {
	CommonConfig `yaml:",inline" json:",inline"`

	// ServerType is the Hetzner Cloud server type (e.g. cx22,
	// cx23, cx32). Default cx23 matches the appliance scripts'
	// existing tested config.
	ServerType string `yaml:"serverType,omitempty"  json:"serverType,omitempty"  jsonschema:"default=cx23,description=Hetzner Cloud server type. cx23 is the default; pick a larger one for resource-heavy workloads."`
	// Location is the Hetzner Cloud location (datacenter region).
	// Default hel1 (Helsinki) for Stockholm-equivalent latency.
	Location string `yaml:"location,omitempty"    json:"location,omitempty"    jsonschema:"default=hel1,description=Hetzner Cloud location. hel1 (Helsinki) is the default; nbg1/fsn1 are the German alternatives."`
	// OSImage is the Hetzner Cloud OS image name. We install k3s
	// via cloud-init runcmd against an Ubuntu image (same shape
	// as the qemu provisioner). Named OSImage rather than the
	// natural-feeling Image because multipass already claims the
	// `image` YAML key for its own (incompatible) cloud-image
	// alias namespace, and schemagen's collision check forces
	// disambiguation.
	OSImage string `yaml:"osImage,omitempty"     json:"osImage,omitempty"     jsonschema:"default=ubuntu-24.04,description=Hetzner Cloud OS image name. Ubuntu cloud images get k3s installed at first boot via cloud-init."`
	// SSHUser is the unprivileged user cloud-init creates and
	// the y-cluster CLI authenticates as. ystack matches the
	// qemu provisioner's convention.
	SSHUser string `yaml:"sshUser,omitempty"     json:"sshUser,omitempty"     jsonschema:"default=ystack,description=Unprivileged user the y-cluster CLI SSHes as. Created by cloud-init at first boot."`

	// AutoTeardownHours sets the deadline for the in-cluster
	// reaper Job (see pkg/provision/hetzner/reaper.go). The Job
	// sleeps the configured duration then issues hcloud delete
	// calls for the server and (if no other lb-group members
	// remain) the LB. Default 8 hours, mandatory (zero / unset
	// is treated as "use default" -- this is a dev-cluster, not
	// a long-lived production VM, and a permanent dev cluster
	// is what we're trying to prevent).
	//
	// Trade documented in HETZNER_PROVISIONER.md: a cluster-side
	// reaper survives operator-host loss; a node reboot resets
	// the timer (acceptable for dev). An earlier on-host at(1)
	// approach got reverted because a wiped operator laptop
	// stranded paid Hetzner resources.
	AutoTeardownHours int `yaml:"autoTeardownHours,omitempty" json:"autoTeardownHours,omitempty" jsonschema:"default=8,description=Hours until host-side auto-teardown fires. 0 / unset uses the 8-hour default. Pick a higher value for an overnight session (e.g. 24)."`

	// LBGroup keys the per-developer shared LoadBalancer. Default
	// $USER (resolved at ApplyDefaults time). Two contexts with
	// the same lbGroup share one Hetzner Load Balancer; the LB is
	// created on first attach and deleted when the last attached
	// server leaves.
	LBGroup string `yaml:"lbGroup,omitempty"     json:"lbGroup,omitempty"     jsonschema:"description=Per-developer LoadBalancer key. Empty defaults to $USER at provision time. Multiple contexts with the same lbGroup share one LB."`

	// FQDNDomain is the parent domain for per-context FQDNs. The
	// per-context FQDN is `<context>.<lbGroup>.<fqdnDomain>`. The
	// default uses the RFC 6761 reserved test TLD so an /etc/hosts
	// miss never accidentally routes to a real domain.
	FQDNDomain string `yaml:"fqdnDomain,omitempty"  json:"fqdnDomain,omitempty"  jsonschema:"default=local.test,description=Parent domain for per-context FQDNs. Per-context FQDN is <context>.<lbGroup>.<fqdnDomain>. Default uses the RFC 6761 reserved test TLD."`

	// Dir is filled at load time from the absolute path of the
	// directory the config came from. Not part of the schema.
	Dir string `yaml:"-" json:"-" jsonschema:"-"`
}

// SetDir satisfies configfile.DirAware so the provisioner can
// resolve relative paths (the auto-teardown at(1) job needs the
// absolute config dir to call `y-cluster teardown -c <dir>`).
func (c *HetznerConfig) SetDir(dir string) { c.Dir = dir }

// hetznerContextRE constrains the context (and therefore the
// Hetzner server name) to a DNS-label-safe shape. The Hetzner
// Cloud API rejects names with uppercase or special characters
// anyway; we enforce >= 4 chars on top of that to reduce the
// chance of someone naming a cluster `dev` or similar three-letter
// collision-prone identifier.
var hetznerContextRE = regexp.MustCompile(`^[a-z][a-z0-9-]{2,}[a-z0-9]$`)

// ApplyDefaults satisfies configfile.Defaulter. Tag-driven string
// defaults run via reflection (covering both common and
// hetzner-specific fields); int / host-dependent defaults are
// handled explicitly below.
//
// Two non-tag-driven defaults need explanation:
//
//   - Name: CommonConfig.Name defaults to "y-cluster" via its
//     own jsonschema tag. For hetzner the server name IS the
//     cluster identifier (Validate enforces equality with
//     Context); we force Name = Context here BEFORE
//     applyCommonDefaults so the operator never has to specify
//     both fields.
//   - AutoTeardownHours: int field, applyTagDefaults handles
//     only string fields. We set the 8-hour default explicitly.
//   - LBGroup: $USER fallback for the per-developer LB key.
//     Host-dependent so it can't be a tag default.
func (c *HetznerConfig) ApplyDefaults() {
	if c.Provider == "" {
		c.Provider = ProviderHetzner
	}
	if c.Name == "" && c.Context != "" {
		c.Name = c.Context
	}
	applyTagDefaults(c)
	c.applyCommonDefaults()
	if c.LBGroup == "" {
		c.LBGroup = os.Getenv("USER")
	}
	if c.AutoTeardownHours == 0 {
		c.AutoTeardownHours = 8
	}
}

// Validate checks the discriminator and Hetzner-specific
// invariants. The context-shape rules are the dev-cluster guards
// from HETZNER_PROVISIONER.md.
func (c *HetznerConfig) Validate() error {
	if err := c.validateCommon(ProviderHetzner); err != nil {
		return err
	}
	// Context: required, not "local", >= 4 chars, DNS-label-safe.
	// "local" specifically is the qemu/docker default; reusing it
	// for a Hetzner cluster would clobber the operator's local
	// cluster context on every kubeconfig merge.
	if c.Context == "" {
		return errInvalid("context is required for hetzner; pick a unique cluster identifier (>= 4 chars, DNS-label-safe)")
	}
	if c.Context == "local" {
		return errInvalid("context %q is reserved for local clusters; pick a different name", c.Context)
	}
	if len(c.Context) < 4 {
		return errInvalid("context %q is too short; use >= 4 characters", c.Context)
	}
	if !hetznerContextRE.MatchString(c.Context) {
		return errInvalid("context %q must match %s (lowercase, DNS-label-safe)", c.Context, hetznerContextRE.String())
	}
	// Name is forced to equal Context. The Hetzner server name
	// IS the cluster identifier in this provisioner -- there's
	// no separation of concerns, no second name to remember, and
	// `cluster.Lookup` resolves by `name == context`.
	if c.Name != "" && c.Name != c.Context {
		return errInvalid("name %q must equal context %q on hetzner (the Hetzner server name is the cluster identifier)", c.Name, c.Context)
	}
	if c.AutoTeardownHours < 0 {
		return errInvalid("autoTeardownHours %d cannot be negative", c.AutoTeardownHours)
	}
	switch c.K3s.Install {
	case "", "airgap", "script":
	default:
		return errInvalid("k3s.install must be one of {airgap, script}, got %q", c.K3s.Install)
	}
	return nil
}
