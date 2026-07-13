package hetzner

import (
	"strings"
	"testing"
)

// TestDefaultGatewayHostnamePattern pins the FQDN shape the
// listener binds to. Three properties matter:
//
//   1. Leading wildcard so any sub-host attaches to this Gateway
//      (otherwise every new HTTPRoute would force a Gateway edit).
//   2. <context>.<lbGroup>.<fqdnDomain> ordering -- mirrors the
//      LB cert SANs (the cert generator uses the same shape) so
//      hostname matching at LB and Gateway align.
//   3. RFC 6761 reserved .local.test default for fqdnDomain --
//      a missed /etc/hosts (or curl --resolve) never accidentally
//      routes to a real domain.
func TestDefaultGatewayHostnamePattern(t *testing.T) {
	got := defaultGatewayHostnamePattern("alice-dev", "alice", "local.test")
	if got != "*.alice-dev.alice.local.test" {
		t.Errorf("hostname pattern: got %q, want %q", got, "*.alice-dev.alice.local.test")
	}
}

// TestDefaultGatewayManifest_Shape pins the load-bearing fields:
//
//   - Namespace + Gateway both labelled managed-by=y-cluster so a
//     `kubectl get -l managed-by=y-cluster` survey shows them.
//   - Listener: HTTP/80 only (LB terminates HTTPS/443 -> HTTP/80).
//   - allowedRoutes.namespaces.from: All (workloads in any
//     namespace can attach without ReferenceGrant boilerplate).
//   - gatewayClassName from the configured class (default
//     "y-cluster"); a renamed class flows through.
func TestDefaultGatewayManifest_Shape(t *testing.T) {
	got := string(defaultGatewayManifest("alice-dev", "alice", "local.test", "y-cluster"))
	for _, want := range []string{
		"name: y-cluster-gateway",   // Namespace
		"managed-by: y-cluster",     // labels
		"name: default",             // Gateway
		"gatewayClassName: y-cluster",
		"protocol: HTTP",
		"port: 80",
		`hostname: "*.alice-dev.alice.local.test"`,
		"from: All",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("manifest missing %q:\n%s", want, got)
		}
	}
	// Negative: no HTTPS listener (LB does TLS termination, the
	// data-plane envoy must not). A double TLS handshake breaks
	// the LB->envoy hop because the LB connects on :80 plain.
	if strings.Contains(got, "protocol: HTTPS") {
		t.Errorf("manifest must not declare an HTTPS listener (LB terminates TLS):\n%s", got)
	}
}

// TestDefaultGatewayManifest_RespectsCustomClass: a renamed
// GatewayClass (operator-side override) flows through. Mirrors
// the existing GatewayClassYAML behaviour where the class name
// is configurable.
func TestDefaultGatewayManifest_RespectsCustomClass(t *testing.T) {
	got := string(defaultGatewayManifest("c", "g", "local.test", "custom-class"))
	if !strings.Contains(got, "gatewayClassName: custom-class") {
		t.Errorf("custom class name not honoured:\n%s", got)
	}
}
