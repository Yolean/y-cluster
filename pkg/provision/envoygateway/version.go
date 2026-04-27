// Package envoygateway bundles a pinned Envoy Gateway release
// (CRDs + controller install + default GatewayClass) into the
// y-cluster binary, and applies it during provision.
//
// # Why bundled
//
// k3s ships with Traefik. y-cluster provisions with
// `--disable=traefik` and installs Envoy Gateway instead so both
// providers (qemu and docker) expose the same HTTPRoute / GRPCRoute
// contract to consumer kustomize bases. Bundling rather than
// fetching at provision time means a fresh provision works
// offline and can't drift from what y-cluster's e2e tested.
//
// # Layout
//
//   - assets/install.yaml is the official EG release manifest at
//     Version (envoyproxy/gateway@<Version>/install.yaml). It
//     carries the Gateway API CRDs at a matching bundle version
//     plus the EG controller, namespace, RBAC, services and
//     configmaps in envoy-gateway-system.
//   - assets/gatewayclass.yaml declares the default `eg`
//     GatewayClass pointing at EG's controller. Consumers that
//     want a different class name or controller can apply their
//     own.
//
// # Bumping the pin
//
// Replace the constant, refresh both files from the matching
// upstream release, and re-run the docker e2e. The package's
// Images() helper enumerates the container images consumers may
// want to mirror or pre-cache.
package envoygateway

// Version is the pinned Envoy Gateway release whose assets are
// embedded in this package. Bumping this without refreshing
// assets/install.yaml will cause the apply step to ship the old
// objects under a new version label -- the test in TestVersion
// guards against that drift by reading the controller image from
// the embedded install.yaml.
const Version = "v1.7.2"
