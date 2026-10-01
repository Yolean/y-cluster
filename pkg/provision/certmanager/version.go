// Package certmanager installs a pinned cert-manager release during
// provision of clusters that are not local dev clusters (qemu, the
// provider that targets production), plus a self-signed CA
// ClusterIssuer for in-cluster certificates. y-cluster owns it because
// its own features issue certificates: Gateway TLS today comes from
// pkg/provision/devcert's hand-rolled self-signed certs.
//
// # Mechanism
//
// Like pkg/provision/envoygateway: the upstream release manifest
// (cert-manager/cert-manager@<Version>/cert-manager.yaml, CRDs plus the
// controller, cainjector and webhook) is downloaded into the
// per-version cache on first use (ensure.go) and applied with kubectl
// from there, so only the first provision per version needs network.
// The issuers are y-cluster's own and rendered in Go (issuers.go).
// Images() lists the release's images for pre-caching.
//
// # Bumping the pin
//
// Replace the constant and provision a qemu cluster: the install waits
// for the three deployments and for the CA certificate to be issued,
// which exercises the webhook and the controller.
package certmanager

// Version is the pinned cert-manager release. The install manifest is
// fetched per version into the cache, so a bump takes effect on the
// next provision with no other file to refresh.
const Version = "v1.21.2"
