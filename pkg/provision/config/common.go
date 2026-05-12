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
	ProviderQEMU      = "qemu"
	ProviderDocker    = "docker"
	ProviderMultipass = "multipass"
)

// AllProviders is the canonical list, sorted, used by schemagen for
// the common-schema enum and by error messages that need to list
// supported values.
var AllProviders = []string{ProviderDocker, ProviderMultipass, ProviderQEMU}

// CommonConfig is the portable subset of `y-cluster-provision.yaml`.
// Every provider config embeds it via `yaml:",inline"` so the keys
// surface at the top level of the file. Adding a field here adds it
// to every provider's schema and to common.schema.json.
//
// Per-provider Validate() must call validateCommon to enforce the
// shared invariants (provider discriminator, k3s.version present).
type CommonConfig struct {
	Provider     string        `yaml:"provider"               json:"provider"               jsonschema:"description=Provisioner to use. Optional in the common schema -- when omitted at provision time the host is probed (multipass daemon reachable -> multipass; Linux+/dev/kvm+qemu-system-x86_64 -> qemu; else reachable docker daemon -> docker). Per-provider schemas narrow this to a single literal and keep it required."`
	Name         string        `yaml:"name,omitempty"         json:"name,omitempty"         jsonschema:"default=y-cluster,description=Cluster instance identifier; used as the docker container name / qemu -name / kubeconfig cluster name / prefix for cache files."`
	Context      string        `yaml:"context,omitempty"      json:"context,omitempty"      jsonschema:"default=local,description=kubeconfig context name to write."`
	Memory       string        `yaml:"memory,omitempty"       json:"memory,omitempty"       jsonschema:"default=8192,description=Memory in MB. qemu allocates this to the VM; docker passes it to --memory."`
	CPUs         string        `yaml:"cpus,omitempty"         json:"cpus,omitempty"         jsonschema:"default=4,description=vCPU count. qemu sets -smp; docker passes --cpus."`
	K3s          K3sConfig     `yaml:"k3s,omitempty"          json:"k3s,omitempty"          jsonschema:"description=k3s install settings. Defaults track pkg/provision/config/k3s.yaml."`
	PortForwards []PortForward `yaml:"portForwards,omitempty" json:"portForwards,omitempty" jsonschema:"description=Host->guest TCP port forwards. Defaults to 6443/80/443 when omitted. Must include a guest:6443 entry so the host's kubectl can reach the API server."`
	Registries   Registries    `yaml:"registries,omitempty"   json:"registries,omitempty"   jsonschema:"description=k3s registries.yaml content. Written to /etc/rancher/k3s/registries.yaml on the node before k3s starts. ${VAR} substitution is supported on credential and endpoint fields."`
	Gateway      GatewayConfig `yaml:"gateway,omitempty"      json:"gateway,omitempty"      jsonschema:"description=Bundled Envoy Gateway install. Skip the install entirely (no CRDs, controller, or GatewayClass) by setting skip:true; rename the default GatewayClass via name."`
	Storage      StorageConfig `yaml:"storage,omitempty"      json:"storage,omitempty"      jsonschema:"description=Bundled local-path-provisioner install. Defaults give a predictable on-disk layout (/data/yolean/<ns>_<pvc>) and Retain reclaim so PV content survives PVC delete and an appliance upgrade rebinds the same directory by name."`
}

// StorageConfig controls the local-path-provisioner install that
// y-cluster ships in place of k3s's bundled local-storage. Three
// knobs, all with defaults that suit the per-customer appliance
// model:
//
//   - Path (default /data/yolean): root directory for every PV
//     directory. Predictable and namespace-friendly so customers
//     can mount a separate disk at this path on import without
//     digging through /var/lib/rancher/k3s/storage/. Override
//     when a customer wants a different mountpoint convention.
//   - PathPattern (default `{{ .PVC.Namespace }}_{{ .PVC.Name }}`):
//     directory name for each PV beneath Path. The default omits
//     the upstream `pvc-<uuid>_` prefix so the path is reachable
//     by namespace+name alone -- which lets an appliance upgrade
//     (a fresh appliance disk) rebind to the same data directory
//     under the operator's mounted data disk just by re-creating
//     the PVC with the same namespace+name. Combined with
//     ReclaimPolicy=Retain, this is the appliance-upgrade
//     migration story.
//   - ReclaimPolicy (default Retain): PV reclaim policy on the
//     y-cluster StorageClass. Retain keeps the directory on PVC
//     delete; the customer's data outlives a stray
//     `kubectl delete pvc` and a fresh appliance install picks
//     up the same data on the next bind.
//
// All three knobs flow into pkg/provision/localstorage.Install
// via the runtime Config translation in each provisioner.
type StorageConfig struct {
	// Path is the local-path-provisioner nodePathMap default
	// path -- the directory that holds one subdirectory per PV.
	Path string `yaml:"path,omitempty" json:"path,omitempty" jsonschema:"default=/data/yolean,description=Root directory under which local-path-provisioner allocates one subdirectory per PV. Customers who attach a separate data disk should mount it here."`

	// PathPattern is the per-PV subdirectory naming template
	// (Go text/template against the local-path-provisioner
	// helper-pod variables: .PVName, .PVC.Namespace, .PVC.Name,
	// .PVC.UID).
	PathPattern string `yaml:"pathPattern,omitempty" json:"pathPattern,omitempty" jsonschema:"default={{ .PVC.Namespace }}_{{ .PVC.Name }},description=Per-PV subdirectory template (Go text/template; vars: .PVName, .PVC.Namespace, .PVC.Name, .PVC.UID). Drop .PVName for predictable upgrade rebinding; keep it if you need uniqueness across PVC delete+recreate cycles under Retain."`

	// ReclaimPolicy is applied to the y-cluster StorageClass.
	// Retain (default) preserves directories on PVC delete;
	// Delete wipes them.
	ReclaimPolicy string `yaml:"reclaimPolicy,omitempty" json:"reclaimPolicy,omitempty" jsonschema:"enum=Retain,enum=Delete,default=Retain,description=Reclaim policy on the y-cluster StorageClass. Retain preserves PV directories on PVC delete; Delete (the upstream k3s default) wipes them."`
}

// GatewayConfig controls the bundled Envoy Gateway install
// (pkg/provision/envoygateway). Two knobs:
//
//   - skip: false (default)                      install CRDs, controller, default GatewayClass
//   - skip: true                                 no CRDs, controller, or GatewayClass
//   - className: <string> (default "y-cluster")  rename the default GatewayClass
//
// All-or-nothing: there is no "install controller without a default
// GatewayClass" option. A consumer that wants to ship their own
// GatewayClass should also ship their own controller install.
//
// The host-side dial address (where /etc/hosts on the developer
// machine should resolve gateway hostnames to) is intentionally NOT
// a field here. It's derived from PortForwards via HostRoutableIP
// and exposed to consumers as the yolean.se/dns-hint-ip annotation
// on the GatewayClass. No user-facing knob -- the value is a
// physical fact about the host/guest port-forward layer, not a
// preference.
type GatewayConfig struct {
	// Skip omits the entire Envoy Gateway install (CRDs, controller,
	// GatewayClass). Useful for test clusters that don't need HTTP
	// ingress -- saves the ~50 MB image pull and a few seconds of
	// rollout. k3s --disable=traefik is still passed; if you want a
	// different ingress, install it yourself.
	Skip bool `yaml:"skip,omitempty" json:"skip,omitempty" jsonschema:"description=If true, do not install Envoy Gateway. k3s still runs with --disable=traefik."`

	// ClassName names the default GatewayClass y-cluster applies
	// after the EG controller is up. Consumer Gateway resources
	// reference this via gatewayClassName.
	//
	// Default: y-cluster. Set to "eg" to keep compatibility with
	// consumers that hardcoded that name in pre-v0.4 cluster
	// configs (the ystack gateway-v4 surface, for one).
	//
	// Ignored when Skip is true.
	ClassName string `yaml:"className,omitempty" json:"className,omitempty" jsonschema:"default=y-cluster,description=GatewayClass name. Consumer Gateway resources reference this via gatewayClassName. Ignored when skip is true."`

	// Resources tunes resource requests on the EG controller pod
	// and the per-Gateway envoy proxy pod. Defaults target a
	// single-user/dev cluster; bump for production-shaped load.
	// Upstream defaults are 100m/256Mi (controller) and
	// 100m/512Mi (proxy), which oversubscribe a 2GB-RAM
	// appliance node.
	Resources GatewayResources `yaml:"resources,omitempty" json:"resources,omitempty" jsonschema:"description=Resource requests for the bundled EG install. Defaults: controller 10m/64Mi, proxy 10m/128Mi. Limits are left as upstream sets them."`
}

// GatewayResources groups the two pods whose resource requests
// y-cluster manages: the EG controller (Deployment in
// envoy-gateway-system) and the per-Gateway envoy proxy
// (spawned by EG via the EnvoyProxy CR our default GatewayClass
// references).
type GatewayResources struct {
	Controller ResourceRequests `yaml:"controller,omitempty" json:"controller,omitempty" jsonschema:"description=EG controller container requests. Default cpu 10m, memory 64Mi."`
	Proxy      ResourceRequests `yaml:"proxy,omitempty"      json:"proxy,omitempty"      jsonschema:"description=Per-Gateway envoy proxy container requests. Default cpu 10m, memory 128Mi."`
}

// ResourceRequests is a minimal Kubernetes-resource-style
// shape covering CPU + memory requests only. Limits are not
// modelled here on purpose: y-cluster's stance is that bursty
// idle workloads are healthier under upstream's existing
// limits than under tighter ones we'd have to guess at.
type ResourceRequests struct {
	CPU    string `yaml:"cpu,omitempty"    json:"cpu,omitempty"    jsonschema:"description=CPU request in Kubernetes notation (e.g. 10m, 0.5, 1)."`
	Memory string `yaml:"memory,omitempty" json:"memory,omitempty" jsonschema:"description=Memory request in Kubernetes notation (e.g. 64Mi, 256Mi, 1Gi)."`
}

// applyGatewayDefaults fills ClassName + Resources when the
// install is enabled. When Skip is set everything is left as
// the user supplied it so debug logs make the operator's
// intent obvious.
func (c *CommonConfig) applyGatewayDefaults() {
	if c.Gateway.Skip {
		return
	}
	if c.Gateway.ClassName == "" {
		c.Gateway.ClassName = "y-cluster"
	}
	if c.Gateway.Resources.Controller.CPU == "" {
		c.Gateway.Resources.Controller.CPU = "10m"
	}
	if c.Gateway.Resources.Controller.Memory == "" {
		c.Gateway.Resources.Controller.Memory = "64Mi"
	}
	if c.Gateway.Resources.Proxy.CPU == "" {
		c.Gateway.Resources.Proxy.CPU = "10m"
	}
	if c.Gateway.Resources.Proxy.Memory == "" {
		c.Gateway.Resources.Proxy.Memory = "128Mi"
	}
}

// EffectiveGatewayClassName returns the GatewayClass name the
// provisioner should hand to envoygateway.Install. Empty string
// means "do not apply a GatewayClass" (because the whole install
// is skipped).
func (c CommonConfig) EffectiveGatewayClassName() string {
	if c.Gateway.Skip {
		return ""
	}
	return c.Gateway.ClassName
}

// PortForward maps a host port to a guest port. Common to all
// providers: qemu uses it for SLIRP -netdev hostfwd, docker uses
// it for container PortBindings.
type PortForward struct {
	Host  string `yaml:"host"  json:"host"  jsonschema:"description=Host port. Empty string lets the provider pick (qemu: SLIRP-assigned; docker: docker-assigned)."`
	Guest string `yaml:"guest" json:"guest" jsonschema:"description=Guest port to forward to."`
}

// HostRoutableIP returns the IP at which the host reaches the
// cluster's HTTP ingress (Envoy Gateway). Today the only providers
// y-cluster supports (qemu SLIRP, docker port-forwards) bind ingress
// on the host loopback, so the value is "127.0.0.1" whenever guest:80
// is in PortForwards. Empty means "no host-side dial address" --
// either no guest:80 forward, or a future provisioner topology that
// doesn't tunnel through the host (multi-VM bridged, cloud LB).
//
// The provisioner publishes this value to the cluster as the
// yolean.se/dns-hint-ip annotation on the y-cluster GatewayClass,
// so consumer tooling like ystack's y-k8s-ingress-hosts can read it
// without any user-side configuration. The value derives entirely
// from PortForwards -- there is no config field that lets the user
// influence it directly.
func (c CommonConfig) HostRoutableIP() string {
	for _, pf := range c.PortForwards {
		if pf.Guest == "80" {
			return "127.0.0.1"
		}
	}
	return ""
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
// pass can't reach: K3s.Version (data-file driven), PortForwards
// (slice default), GatewayConfig.Name (default y-cluster).
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
	c.applyGatewayDefaults()
	c.applyStorageDefaults()
}

// applyStorageDefaults fills the bundled-local-storage knobs.
// All three values are independent: a user who only overrides
// Path (e.g. customer-side disk mounted at /mnt/customer-data)
// keeps the default pattern + reclaim policy.
func (c *CommonConfig) applyStorageDefaults() {
	if c.Storage.Path == "" {
		c.Storage.Path = "/data/yolean"
	}
	if c.Storage.PathPattern == "" {
		c.Storage.PathPattern = "{{ .PVC.Namespace }}_{{ .PVC.Name }}"
	}
	if c.Storage.ReclaimPolicy == "" {
		c.Storage.ReclaimPolicy = "Retain"
	}
}

// validateCommon checks invariants every provider relies on. The
// per-provider Validate methods call this first, then check their
// own fields.
//
// PortForwards is *not* validated here: providers that tunnel through
// the host (qemu SLIRP, docker port-forwards) need a guest:6443 entry
// to reach the API server and enforce that in their own Validate;
// providers that dial the guest directly (multipass, on its
// hypervisor-managed network) have no use for PortForwards at all.
func (c *CommonConfig) validateCommon(expected string) error {
	if c.Provider != expected {
		return errInvalid("provider must be %q, got %q", expected, c.Provider)
	}
	if c.K3s.Version == "" {
		return errInvalid("k3s.version is empty; check pkg/provision/config/k3s.yaml")
	}
	return nil
}

// requireHostAPIPort enforces the guest:6443 PortForwards invariant
// for host-tunneled providers. qemu and docker call this from their
// own Validate; multipass does not because the host dials the VM IP
// directly and PortForwards has no operational meaning there.
func (c *CommonConfig) requireHostAPIPort() error {
	if c.HostAPIPort() == "" {
		return errInvalid("portForwards must include a guest:6443 entry to reach k3s from the host")
	}
	return nil
}
