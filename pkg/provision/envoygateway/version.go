// Package envoygateway installs a pinned Envoy Gateway release
// (CRDs + controller + a rendered default GatewayClass) during
// provision.
//
// # Why pinned
//
// k3s ships with Traefik. y-cluster provisions with
// `--disable=traefik` and installs Envoy Gateway instead so all
// providers expose the same HTTPRoute / GRPCRoute contract to
// consumer kustomize bases. Pinning the version means a provision
// can't drift from what y-cluster's e2e tested.
//
// # Mechanism
//
// The official release manifest
// (envoyproxy/gateway@<Version>/install.yaml, which carries the
// Gateway API CRDs plus the EG controller, namespace, RBAC,
// services and configmaps) is downloaded into the per-version
// cache directory on first use (ensure.go) and kubectl-applied
// from there; subsequent provisions reuse the cached copy, so
// only the FIRST provision per version needs network. The
// GatewayClass (default name "y-cluster", configurable via
// gateway.className) is rendered in Go, not shipped as an asset
// -- see embed.go.
//
// # Bumping the pin
//
// Replace the constant and re-run the e2e suites (the docker leg
// applies the manifest against real k3s; the multipass/kvm legs
// prove the actual controller rollout). The package's Images()
// helper enumerates the container images consumers may want to
// mirror or pre-cache; images.go derives them from the cached
// install.yaml.
package envoygateway

// Version is the pinned Envoy Gateway release. The install
// manifest is fetched per-version into the cache, so a bump takes
// effect on the next provision with no other file to refresh.
const Version = "v1.7.5"
