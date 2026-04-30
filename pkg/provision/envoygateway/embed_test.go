package envoygateway

import (
	"strings"
	"testing"
)

// TestGatewayClassYAML_NoHintIP guards the cloud / no-host-routing
// shape: empty dnsHintIP omits the metadata.annotations block
// entirely, so an absent hint is distinguishable from
// "annotation present with empty value".
func TestGatewayClassYAML_NoHintIP(t *testing.T) {
	got := string(GatewayClassYAML("y-cluster", ""))
	if strings.Contains(got, "annotations") {
		t.Fatalf("expected no annotations block:\n%s", got)
	}
	if strings.Contains(got, DNSHintIPAnnotation) {
		t.Fatalf("expected no %s annotation:\n%s", DNSHintIPAnnotation, got)
	}
	if !strings.Contains(got, "name: y-cluster") {
		t.Fatalf("missing class name:\n%s", got)
	}
	if !strings.Contains(got, "controllerName: "+EGControllerName) {
		t.Fatalf("missing controller name:\n%s", got)
	}
}

// TestGatewayClassYAML_WithHintIP guards the qemu/docker
// host-loopback shape: the dnsHintIP value lands as a single
// annotation under the GatewayClass metadata.
func TestGatewayClassYAML_WithHintIP(t *testing.T) {
	got := string(GatewayClassYAML("y-cluster", "127.0.0.1"))
	if !strings.Contains(got, "annotations:") {
		t.Fatalf("missing annotations block:\n%s", got)
	}
	wantLine := DNSHintIPAnnotation + ": 127.0.0.1"
	if !strings.Contains(got, wantLine) {
		t.Fatalf("missing %q:\n%s", wantLine, got)
	}
	// Annotation block must precede spec; otherwise YAML attaches it
	// to the wrong field.
	annoIdx := strings.Index(got, "annotations:")
	specIdx := strings.Index(got, "spec:")
	if annoIdx < 0 || specIdx < 0 || annoIdx > specIdx {
		t.Fatalf("annotations not under metadata before spec:\n%s", got)
	}
}

// TestGatewayClassYAML_RespectsCustomName guards the rename path:
// a non-default ClassName (e.g. "eg" for compat) flows through to
// both metadata.name and the doc comment. The comment header line
// is part of the contract -- consumers grep for it during debug.
func TestGatewayClassYAML_RespectsCustomName(t *testing.T) {
	got := string(GatewayClassYAML("eg", ""))
	if !strings.Contains(got, "name: eg") {
		t.Fatalf("missing custom name:\n%s", got)
	}
	if !strings.Contains(got, "gatewayClassName: eg") {
		t.Fatalf("comment should reference the configured name:\n%s", got)
	}
}
