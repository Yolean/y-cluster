package config

import "regexp"

// What the providers that rent a server (hetzner, glesys) share, and
// where they differ from the local ones.

// cloudContextRE is a DNS label that starts with a letter, four
// characters or more. On a cloud provider the kubeconfig context is
// also the server's name, which is how `y-cluster` finds the cluster
// again, so it has to be something the cloud API accepts as a
// hostname. The length floor keeps `dev` and other three-letter names
// that collide between colleagues out of a shared project.
var cloudContextRE = regexp.MustCompile(`^[a-z][a-z0-9-]{2,61}[a-z0-9]$`)

// applyCloudDefaults is the ApplyDefaults body of a cloud provider:
// the tag-driven and common defaults, minus the ones that only make
// sense for a cluster on this host.
//
//   - Name follows Context, so the operator writes the identifier
//     once.
//   - Context gets no default. CommonConfig defaults it to "local",
//     and a cloud config that omits it would then fail as `context
//     "local" is reserved`, blaming the operator for a value the
//     defaulting wrote. Left empty, Validate can say what is true:
//     a context is required.
//   - k3s.install defaults to script. The common default is airgap,
//     which needs a copy channel and a reason (a node without
//     egress); a rented server has egress and installs from
//     upstream.
//   - No portForwards. A server with a public address forwards
//     nothing through the operator's host.
func applyCloudDefaults(cfg any, c *CommonConfig) {
	explicitContext := c.Context != ""
	explicitInstall := c.K3s.Install != ""
	explicitForwards := len(c.PortForwards) > 0
	if c.Name == "" {
		c.Name = c.Context
	}
	applyTagDefaults(cfg)
	c.applyCommonDefaults()
	if !explicitContext {
		c.Context = ""
	}
	if !explicitInstall {
		c.K3s.Install = "script"
	}
	if !explicitForwards {
		c.PortForwards = nil
	}
}

// validateCloud is validateCommon plus the rules every cloud provider
// has for the cluster's identity and lifetime.
func (c *CommonConfig) validateCloud(provider string) error {
	if err := c.validateCommon(provider); err != nil {
		return err
	}
	// "local" is what the local providers default to; a cloud
	// cluster under that name would overwrite the operator's local
	// context on every kubeconfig merge.
	switch {
	case c.Context == "":
		return errInvalid("context is required for %s; pick a unique cluster identifier (>= 4 chars, DNS-label-safe)", provider)
	case c.Context == "local":
		return errInvalid("context %q is reserved for local clusters; pick a different name", c.Context)
	case len(c.Context) < 4:
		return errInvalid("context %q is too short; use >= 4 characters", c.Context)
	case !cloudContextRE.MatchString(c.Context):
		return errInvalid("context %q must match %s (a lowercase DNS label starting with a letter)", c.Context, cloudContextRE.String())
	}
	if c.Name != "" && c.Name != c.Context {
		return errInvalid("name %q must equal context %q on %s (the server is named after the context; that is how the cluster is found again)", c.Name, c.Context, provider)
	}
	// A server is running or it is stopped. Refuse rather than
	// downgrade to stop, so a config that asked for pause does not
	// quietly get other semantics. Enabled() matters: the tag
	// defaults fill onExpiry even when no budget is set.
	if c.Lifetime.Enabled() && c.Lifetime.OnExpiry == OnExpiryPause {
		return errInvalid("lifetime.onExpiry %q is not supported on %s (a rented server has no pause/resume primitive); use %s or %s", OnExpiryPause, provider, OnExpiryStop, OnExpiryTeardown)
	}
	return nil
}
