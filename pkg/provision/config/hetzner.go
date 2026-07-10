package config

import (
	"os"
	"regexp"
)

// HetznerConfig is the on-disk shape of `y-cluster-provision.yaml`
// when `provider: hetzner`. CommonConfig carries portable fields;
// the fields below are Hetzner-specific.
//
// See HETZNER_PROVISIONER.md in the specs repo for the dev-cluster
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

	// ImageCache configures the Hetzner Object Storage-backed image
	// cache (phase 6 in HETZNER_PROVISIONER.md). Empty Bucket
	// disables the feature entirely; defaults preserve the
	// pull-from-upstream behavior of phases 1-5.
	ImageCache HetznerImageCache `yaml:"imageCache,omitempty" json:"imageCache,omitempty" jsonschema:"description=Hetzner Object Storage-backed image cache. Leave bucket empty to disable; see HETZNER_PROVISIONER.md phase 6."`

	// Dir is filled at load time from the absolute path of the
	// directory the config came from. Not part of the schema.
	Dir string `yaml:"-" json:"-" jsonschema:"-"`
}

// HetznerImageCache configures the per-cluster S3-backed image
// cache. The fields land on HetznerConfig.ImageCache and the
// validation rules live on this type so Validate's call site
// stays readable.
//
// All fields are optional. The cache is OFF unless Bucket is set;
// the other fields fill in regional defaults when Bucket is set
// and the operator left them blank.
type HetznerImageCache struct {
	// Bucket is the Hetzner Object Storage bucket holding the
	// per-cluster index + OCI layouts. Empty disables the cache.
	Bucket string `yaml:"bucket,omitempty" json:"bucket,omitempty" jsonschema:"description=Hetzner Object Storage bucket holding the OCI layouts + index.json. Empty disables the image-cache feature."`

	// Region is the Hetzner Object Storage region. Defaults to
	// hel1 (matching the cluster Location default) at runtime --
	// applyDefaults only fires when Bucket is set, so the
	// jsonschema tag intentionally omits a `default=` to keep
	// the reflection-based applyTagDefaults from auto-populating
	// regional surface area on a disabled cache.
	Region string `yaml:"region,omitempty" json:"region,omitempty" jsonschema:"description=Hetzner Object Storage region (hel1 / fsn1 / nbg1). Defaults to hel1 when bucket is set."`

	// IndexKey is the object key within Bucket holding the
	// (ref -> digest -> layout-prefix) map. Defaults to index.json
	// at the bucket root, applied at runtime only when the cache
	// is enabled (see Region's note above).
	IndexKey string `yaml:"indexKey,omitempty" json:"indexKey,omitempty" jsonschema:"description=Object key within Bucket holding the cache index. Defaults to index.json when bucket is set."`

	// RejectUpstream, when true, drops a /etc/rancher/k3s/registries.yaml
	// on the node that maps every wildcard mirror to the empty set,
	// turning any upstream image pull into a hard error. Off by
	// default; intended for e2e runs that need to surface cache
	// misses instead of silently pulling from upstream.
	RejectUpstream bool `yaml:"rejectUpstream,omitempty" json:"rejectUpstream,omitempty" jsonschema:"default=false,description=Makes k3s refuse to pull from any upstream registry so cache misses become hard errors. Off by default."`
}

// Enabled is the canonical "is the cache configured" check. Empty
// Bucket = disabled regardless of other fields, so a partially-
// filled config (e.g. only Region set) doesn't accidentally
// activate the feature.
func (c HetznerImageCache) Enabled() bool { return c.Bucket != "" }

// hetznerImageCacheRegions is the union of regions Hetzner Object
// Storage currently exposes. Validate against the union so a typo
// (`hel-1`, `helsinki`) fails at config-load time, not deep inside
// a request that the regional endpoint won't resolve.
var hetznerImageCacheRegions = map[string]bool{
	"hel1": true,
	"fsn1": true,
	"nbg1": true,
}

func (c HetznerImageCache) validate() error {
	if !c.Enabled() {
		// Surface obvious config mistakes: a region or
		// rejectUpstream set without a bucket is dead weight at
		// best and a "did the operator forget the bucket?"
		// landmine at worst.
		if c.Region != "" || c.IndexKey != "" || c.RejectUpstream {
			return errInvalid("imageCache fields set but bucket is empty; either set bucket or remove the other imageCache fields")
		}
		return nil
	}
	if !hetznerImageCacheRegions[c.Region] {
		return errInvalid("imageCache.region %q is not a known Hetzner Object Storage region; expected one of hel1, fsn1, nbg1", c.Region)
	}
	return nil
}

// SetDir satisfies configfile.DirAware. Nothing hetzner-specific
// reads Dir today (lifetime expiry runs as an in-cluster reaper
// Job, which needs no host-side paths); the field exists so
// relative paths in future config surface can resolve against the
// config file location like the other providers do.
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
	c.ImageCache.applyDefaults()
}

// applyDefaults fills in regional defaults for an enabled cache;
// a disabled cache (empty Bucket) keeps zero-values across the
// board so the operator can `git diff` a config without seeing
// noise from defaults that don't actually do anything.
func (c *HetznerImageCache) applyDefaults() {
	if !c.Enabled() {
		return
	}
	if c.Region == "" {
		c.Region = "hel1"
	}
	if c.IndexKey == "" {
		c.IndexKey = "index.json"
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
	// lifetime: the standard config drives hetzner's expiry
	// mechanism, an in-cluster reaper Job installed at provision
	// (the analog of GCP's max-run-duration). stop and teardown
	// map to hcloud server shutdown / delete; pause has no
	// Hetzner Cloud primitive, so a set budget with onExpiry
	// pause is rejected instead of silently downgraded. The
	// Enabled() guard matters: applyTagDefaults fills onExpiry
	// (stop) even when no budget is set, and a disabled lifetime
	// must stay valid.
	if c.Lifetime.Enabled() && c.Lifetime.OnExpiry == OnExpiryPause {
		return errInvalid("lifetime.onExpiry %q is not supported on hetzner (Hetzner Cloud has no pause/resume primitive); use %s or %s", OnExpiryPause, OnExpiryStop, OnExpiryTeardown)
	}
	switch c.K3s.Install {
	case "", "airgap", "script":
	default:
		return errInvalid("k3s.install must be one of {airgap, script}, got %q", c.K3s.Install)
	}
	if err := c.ImageCache.validate(); err != nil {
		return err
	}
	return nil
}
