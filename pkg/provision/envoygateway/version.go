// Package envoygateway installs an Envoy Gateway release (CRDs +
// controller + a rendered default GatewayClass) during provision and
// platform install.
//
// # Why pinned
//
// k3s ships with Traefik. y-cluster provisions with
// `--disable=traefik` and installs Envoy Gateway instead so all
// providers expose the same HTTPRoute / GRPCRoute contract to
// consumer kustomize bases. Pinning the version means a provision
// can't drift from what y-cluster's e2e tested.
//
// The pin is the default. A cluster's config can name another release
// (gateway.version, with the sha256 of its install.yaml), so that a
// cluster takes a patch release without a y-cluster release. How far
// a cluster may move from the release it runs is pkg/provision/upgrade's
// rule, applied before anything is applied (Move).
//
// # Mechanism
//
// The official release manifest
// (envoyproxy/gateway@<version>/install.yaml, which carries the
// Gateway API CRDs plus the EG controller, namespace, RBAC,
// services and configmaps) is downloaded into the per-version
// cache directory on first use (ensure.go), verified against its
// sha256, and kubectl-applied from there; subsequent provisions
// verify and reuse the cached copy, so only the FIRST provision per
// version needs network. The GatewayClass (default name "y-cluster",
// configurable via gateway.className) is rendered in Go, not shipped
// as an asset -- see embed.go.
//
// Nothing is pruned: an object an older release installed and a newer
// one no longer ships stays on the cluster after an upgrade.
//
// # Bumping the pin
//
// Replace Version and VersionSHA256 (`curl -sL <release
// URL>/install.yaml | sha256sum`) and re-run the e2e suites (the
// docker leg applies the manifest against real k3s; the multipass/kvm
// legs prove the actual controller rollout, and the kvm leg's
// TestHost_GatewayUpgrade moves a cluster between real releases).
// A bump moves clusters whose config names no gateway.version on
// their next converge, by the same rule as a configured version.
// The package's Images() helper enumerates the container images
// consumers may want to mirror or pre-cache; images.go derives them
// from the cached install.yaml.
package envoygateway

// Version is the Envoy Gateway release y-cluster installs when the
// config names none. The install manifest is fetched per-version into
// the cache, so a bump takes effect on the next provision with no
// other file to refresh.
const Version = "v1.9.1"

// VersionSHA256 is the sha256 of Version's install.yaml.
const VersionSHA256 = "72b3971364f172eb0b9636c7142cc84ff695467bc065897958bde85a3c06cfd5"

// Release is the release y-cluster installs for a configured version
// (gateway.version): that one, or Version when none is configured.
func Release(configured string) string {
	if configured == "" {
		return Version
	}
	return configured
}
