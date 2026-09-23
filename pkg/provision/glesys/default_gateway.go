package glesys

import (
	"bytes"
	"context"
	"encoding/base64"
	"fmt"
	"net"
	"os"
	"os/exec"
	"strings"

	"go.uber.org/zap"

	"github.com/Yolean/y-cluster/pkg/provision/devcert"
)

// DefaultGatewayNamespace and DefaultGatewayName are the same names
// hetzner uses, so `y-cluster gateway example install` and consumer
// HTTPRoutes with parentRefs[0].name = "default" work unchanged.
const (
	DefaultGatewayNamespace = "y-cluster-gateway"
	DefaultGatewayName      = "default"
)

// tlsSecretName holds the self-signed certificate the HTTPS listener
// terminates with. Lives next to the Gateway, in its namespace.
const tlsSecretName = "default-tls"

// fqdnDomain is the parent domain for the per-context FQDNs the
// default Gateway serves: <context>.local.test and everything
// below. The RFC 6761 reserved test TLD, so an /etc/hosts miss
// never routes to a real domain. Hetzner has this as a config
// field with the same default, and its yaml key (fqdnDomain) is
// hetzner's, so here it is a constant until a second provider
// needs it configurable.
const fqdnDomain = "local.test"

// DefaultGatewayHostnamePattern is the wildcard the Gateway's
// listeners bind to: any subdomain of <context>.local.test, so a
// new route needs no Gateway change. Shorter than hetzner's
// *.<context>.<lbGroup>.<domain>: with no shared load balancer
// there is no group to disambiguate by.
func DefaultGatewayHostnamePattern(contextName string) string {
	return "*." + contextName + "." + fqdnDomain
}

// certSubjects is the SAN list the certificate covers: the
// context's own FQDN, the wildcard below it, and the node's public
// address for a client that dials the IP without /etc/hosts.
func certSubjects(contextName, ipv4 string) (commonName string, dnsNames []string, ipSANs []net.IP) {
	commonName = contextName + "." + fqdnDomain
	dnsNames = []string{commonName, "*." + commonName}
	if ip := net.ParseIP(ipv4); ip != nil {
		ipSANs = []net.IP{ip}
	}
	return commonName, dnsNames, ipSANs
}

// defaultGatewayManifest renders the per-cluster Gateway plus the
// Secret its HTTPS listener terminates with:
//
//   - http/80 and https/443, both constrained to the context's
//     wildcard hostname. A request to the node address without a
//     matching Host gets a 404 from envoy, not a fall-through to
//     whatever route is present: the address is on the public
//     internet.
//   - TLS terminates in envoy with the self-signed certificate. The
//     hetzner path terminates at the Hetzner load balancer instead;
//     here there is nothing in front of the node, so the listener
//     has to do it.
//   - allowedRoutes from All namespaces, so a workload in its own
//     namespace attaches without a ReferenceGrant.
//
// Server-side applied so a re-provision reconciles without churn.
func defaultGatewayManifest(contextName, gatewayClassName string, certPEM, keyPEM []byte) []byte {
	hostname := DefaultGatewayHostnamePattern(contextName)
	r := strings.NewReplacer(
		"__NAMESPACE__", DefaultGatewayNamespace,
		"__NAME__", DefaultGatewayName,
		"__SECRET__", tlsSecretName,
		"__GATEWAY_CLASS__", gatewayClassName,
		"__HOSTNAME__", hostname,
		"__CERT__", base64.StdEncoding.EncodeToString(certPEM),
		"__KEY__", base64.StdEncoding.EncodeToString(keyPEM),
	)
	return []byte(r.Replace(`---
apiVersion: v1
kind: Namespace
metadata:
  name: __NAMESPACE__
  labels:
    managed-by: y-cluster
---
apiVersion: v1
kind: Secret
metadata:
  name: __SECRET__
  namespace: __NAMESPACE__
  labels:
    managed-by: y-cluster
type: kubernetes.io/tls
data:
  tls.crt: __CERT__
  tls.key: __KEY__
---
apiVersion: gateway.networking.k8s.io/v1
kind: Gateway
metadata:
  name: __NAME__
  namespace: __NAMESPACE__
  labels:
    managed-by: y-cluster
  annotations:
    yolean.se/dns-hint-ip-source: glesys-node
spec:
  gatewayClassName: __GATEWAY_CLASS__
  listeners:
  - name: http
    protocol: HTTP
    port: 80
    hostname: "__HOSTNAME__"
    allowedRoutes:
      namespaces:
        from: All
  - name: https
    protocol: HTTPS
    port: 443
    hostname: "__HOSTNAME__"
    tls:
      mode: Terminate
      certificateRefs:
      - kind: Secret
        name: __SECRET__
    allowedRoutes:
      namespaces:
        from: All
`))
}

// installDefaultGateway applies the per-cluster Gateway after
// envoy-gateway is up. Envoy Gateway spawns its data-plane pod and
// Service only once a Gateway references the GatewayClass; the
// Service carries the node address as an externalIP (see
// envoygateway.EnvoyProxyYAML), which is what puts envoy on
// <ipv4>:80 and :443.
func (c *Cluster) installDefaultGateway(ctx context.Context) error {
	commonName, dnsNames, ipSANs := certSubjects(c.cfg.Context, c.state.IPv4)
	certPEM, keyPEM, err := devcert.GenerateSelfSigned(commonName, dnsNames, ipSANs)
	if err != nil {
		return fmt.Errorf("self-signed certificate: %w", err)
	}
	c.logger.Info("applying default Gateway",
		zap.String("namespace", DefaultGatewayNamespace),
		zap.String("name", DefaultGatewayName),
		zap.String("gatewayClass", c.cfg.Gateway.ClassName),
		zap.String("hostname", DefaultGatewayHostnamePattern(c.cfg.Context)),
	)
	cmd := exec.CommandContext(ctx, "kubectl",
		"--context="+c.cfg.Context,
		"apply", "--server-side", "--force-conflicts", "--field-manager=y-cluster",
		"-f", "-",
	)
	cmd.Stdin = bytes.NewReader(defaultGatewayManifest(c.cfg.Context, c.cfg.Gateway.ClassName, certPEM, keyPEM))
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("kubectl apply default Gateway: %w", err)
	}
	return nil
}
