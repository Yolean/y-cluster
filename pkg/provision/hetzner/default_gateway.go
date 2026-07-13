package hetzner

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"strings"

	"go.uber.org/zap"
)

// DefaultGatewayNamespace holds the y-cluster-managed Gateway
// resource that owns the LB-facing HTTP listener. Separate from
// envoy-gateway-system (the controller's namespace) so cluster
// operators can grep / kubectl describe the Gateway without
// wading through controller objects.
const DefaultGatewayNamespace = "y-cluster-gateway"

// DefaultGatewayName is the conventional Gateway resource name
// inside DefaultGatewayNamespace. Consumer HTTPRoutes set
// `parentRefs[0].name = "default"` to attach.
const DefaultGatewayName = "default"

// defaultGatewayHostnamePattern returns the wildcard hostname the
// Gateway listener binds to. The shape mirrors HETZNER_PROVISIONER.md's
// `<context>.<lbGroup>.<fqdnDomain>` FQDN convention: a leading
// wildcard accepts any subdomain (e.g. `hello.alice-dev.alice.local.test`)
// without forcing operators to update the Gateway every time they
// add a route.
func defaultGatewayHostnamePattern(contextName, lbGroup, fqdnDomain string) string {
	return fmt.Sprintf("*.%s.%s.%s", contextName, lbGroup, fqdnDomain)
}

// defaultGatewayManifest renders the per-cluster Gateway resource:
//
//   - Listens on HTTP/80 only. The LB terminates HTTPS (Phase 3.c.2),
//     forwards as plain HTTP. envoy-gateway therefore needs a
//     plaintext listener; it is the LB's certificate that does
//     transport security at the public-internet boundary.
//   - hostname constrains the listener to the y-cluster FQDN
//     pattern. Anyone hitting the LB IP without a matching Host
//     header gets a 404 from envoy, not a fall-through to whatever
//     route happened to be present. This is a defense-in-depth
//     posture for a public-internet-exposed cluster.
//   - allowedRoutes.namespaces.from: All -- HTTPRoutes from any
//     namespace can attach. The Gateway lives in y-cluster-gateway,
//     workloads typically land in their own namespaces; the
//     all-namespaces grant avoids ReferenceGrant boilerplate per
//     workload.
//
// The y-cluster-managed Namespace + Gateway are both server-side
// applied so re-Provisions reconcile without churn.
func defaultGatewayManifest(contextName, lbGroup, fqdnDomain, gatewayClassName string) []byte {
	hostname := defaultGatewayHostnamePattern(contextName, lbGroup, fqdnDomain)
	return []byte(strings.ReplaceAll(strings.ReplaceAll(strings.ReplaceAll(strings.ReplaceAll(
		`---
apiVersion: v1
kind: Namespace
metadata:
  name: __NAMESPACE__
  labels:
    managed-by: y-cluster
---
apiVersion: gateway.networking.k8s.io/v1
kind: Gateway
metadata:
  name: __NAME__
  namespace: __NAMESPACE__
  labels:
    managed-by: y-cluster
  annotations:
    yolean.se/dns-hint-ip-source: hetzner-lb
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
`,
		"__NAMESPACE__", DefaultGatewayNamespace),
		"__NAME__", DefaultGatewayName),
		"__GATEWAY_CLASS__", gatewayClassName),
		"__HOSTNAME__", hostname))
}

// installDefaultGateway applies the per-cluster Gateway resource
// after envoy-gateway is up. Without this, envoy-gateway never
// spawns its data-plane Pod (and the matching `LoadBalancer`
// Service that klipper-lb binds to host:80) -- so the Hetzner LB
// has no working backend and the public LB IPv4 is silent.
//
// Idempotent SSA: re-Provision against a still-running cluster
// reconciles without churn (the controller-side data plane stays
// untouched as long as the spec hashes match).
func (c *Cluster) installDefaultGateway(ctx context.Context) error {
	manifest := defaultGatewayManifest(
		c.cfg.Context,
		c.cfg.LBGroup,
		c.cfg.FQDNDomain,
		c.cfg.Gateway.ClassName,
	)
	c.logger.Info("applying default Gateway",
		zap.String("namespace", DefaultGatewayNamespace),
		zap.String("name", DefaultGatewayName),
		zap.String("gatewayClass", c.cfg.Gateway.ClassName),
		zap.String("hostname", defaultGatewayHostnamePattern(c.cfg.Context, c.cfg.LBGroup, c.cfg.FQDNDomain)),
	)
	cmd := exec.CommandContext(ctx, "kubectl",
		"--context="+c.cfg.Context,
		"apply",
		"--server-side", "--force-conflicts",
		"--field-manager=y-cluster",
		"-f", "-",
	)
	cmd.Stdin = bytes.NewReader(manifest)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("kubectl apply default Gateway: %w", err)
	}
	return nil
}
