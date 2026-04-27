package config

// Registries mirrors the shape of k3s's
// /etc/rancher/k3s/registries.yaml. The y-cluster provision step
// marshals this struct and writes it to that path on the node
// before k3s starts, replacing the bash `bin/y-registry-config`
// step ystack used pre-migration.
//
// Reference: https://docs.k3s.io/installation/private-registry
//
// # Substitution
//
// String fields holding values an operator may want to source
// from the environment are tagged `envsubst:"true"`: registry
// endpoints (so a fresh cluster can point at a bootstrap mirror
// IP) and credentials (so the GCP access token isn't checked
// into the config). pkg/configfile.Load runs the substitution
// during load -- see pkg/envsubst for the policy. Untagged
// fields (rewrite regexes, TLS paths) reject ${...} at load.
//
// # Validation
//
// k3s itself validates registries.yaml when it parses the file at
// startup, so we deliberately don't duplicate that check here.
// The `Empty` helper exists so the provisioner can short-circuit
// the write step when no registries are configured.
type Registries struct {
	Mirrors map[string]RegistryMirror `yaml:"mirrors,omitempty" json:"mirrors,omitempty" jsonschema:"description=Per-host mirror redirects. Keys are the original registry hostname requested by containerd."`
	Configs map[string]RegistryConfig `yaml:"configs,omitempty" json:"configs,omitempty" jsonschema:"description=Per-host TLS and auth settings. Keys match the registry hostname (the original host or a mirror endpoint host)."`
}

// RegistryMirror is one entry under registries.yaml `mirrors:`.
type RegistryMirror struct {
	Endpoint []string          `yaml:"endpoint,omitempty" json:"endpoint,omitempty" envsubst:"true" jsonschema:"description=Mirror endpoints to try in order. ${VAR} substitution supported."`
	Rewrite  map[string]string `yaml:"rewrite,omitempty"  json:"rewrite,omitempty"  jsonschema:"description=Regex rewrite rules applied to the request path before forwarding to the mirror."`
}

// RegistryConfig is one entry under registries.yaml `configs:`.
type RegistryConfig struct {
	Auth *RegistryAuth `yaml:"auth,omitempty" json:"auth,omitempty" jsonschema:"description=Username/password or token credentials for this host."`
	TLS  *RegistryTLS  `yaml:"tls,omitempty"  json:"tls,omitempty"  jsonschema:"description=Client TLS settings for this host."`
}

// RegistryAuth holds containerd's auth options. Fields are
// tagged for ${VAR} expansion since the canonical case
// (oauth2accesstoken + ${GCP_ACCESS_TOKEN}) needs it.
type RegistryAuth struct {
	Username      string `yaml:"username,omitempty"      json:"username,omitempty"      envsubst:"true" jsonschema:"description=Basic auth username; ${VAR} supported."`
	Password      string `yaml:"password,omitempty"      json:"password,omitempty"      envsubst:"true" jsonschema:"description=Basic auth password; ${VAR} supported. Common pattern: oauth2accesstoken with ${GCP_ACCESS_TOKEN}."`
	Auth          string `yaml:"auth,omitempty"          json:"auth,omitempty"          envsubst:"true" jsonschema:"description=Pre-encoded Authorization header; ${VAR} supported."`
	IdentityToken string `yaml:"identitytoken,omitempty" json:"identitytoken,omitempty" envsubst:"true" jsonschema:"description=Bearer identity token; ${VAR} supported."`
}

// RegistryTLS mirrors containerd's TLS knobs. Paths are local to
// the node and intentionally NOT envsubst-tagged -- a value like
// "${HOME}/cert.pem" would refer to the node's filesystem, not
// the host's, and that surprise is worth preventing.
type RegistryTLS struct {
	CertFile           string `yaml:"cert_file,omitempty"           json:"cert_file,omitempty"           jsonschema:"description=Path to client cert file on the node."`
	KeyFile            string `yaml:"key_file,omitempty"            json:"key_file,omitempty"            jsonschema:"description=Path to client key file on the node."`
	CAFile             string `yaml:"ca_file,omitempty"             json:"ca_file,omitempty"             jsonschema:"description=Path to CA file on the node."`
	InsecureSkipVerify bool   `yaml:"insecure_skip_verify,omitempty" json:"insecure_skip_verify,omitempty" jsonschema:"description=Disable TLS verification for this host. Off by default."`
}

// Empty reports whether r carries no mirror or config entries.
// Provisioners use this to skip the registries write step
// entirely when a config doesn't exercise the feature.
func (r Registries) Empty() bool {
	return len(r.Mirrors) == 0 && len(r.Configs) == 0
}
