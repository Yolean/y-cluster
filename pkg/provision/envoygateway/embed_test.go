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
	got := string(GatewayClassYAML("y-cluster", "", ""))
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
	if strings.Contains(got, "parametersRef") {
		t.Fatalf("empty envoyProxyName should omit parametersRef:\n%s", got)
	}
}

// TestGatewayClassYAML_WithHintIP guards the qemu/docker
// host-loopback shape: the dnsHintIP value lands as a single
// annotation under the GatewayClass metadata.
func TestGatewayClassYAML_WithHintIP(t *testing.T) {
	got := string(GatewayClassYAML("y-cluster", "127.0.0.1", ""))
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
	got := string(GatewayClassYAML("eg", "", ""))
	if !strings.Contains(got, "name: eg") {
		t.Fatalf("missing custom name:\n%s", got)
	}
	if !strings.Contains(got, "gatewayClassName: eg") {
		t.Fatalf("comment should reference the configured name:\n%s", got)
	}
}

// TestGatewayClassYAML_WithEnvoyProxyRef pins the parametersRef
// shape EG expects: group / kind / name / namespace under
// spec.parametersRef, namespace fixed to the EG namespace.
func TestGatewayClassYAML_WithEnvoyProxyRef(t *testing.T) {
	got := string(GatewayClassYAML("y-cluster", "", EnvoyProxyName))
	for _, want := range []string{
		"parametersRef:",
		"group: gateway.envoyproxy.io",
		"kind: EnvoyProxy",
		"name: " + EnvoyProxyName,
		"namespace: " + Namespace,
	} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %q:\n%s", want, got)
		}
	}
}

// TestEnvoyProxyYAML_ShapesResources pins the EnvoyProxy CR
// fields y-cluster actually owns: requests under provider.
// kubernetes.envoyDeployment.container.resources.
func TestEnvoyProxyYAML_ShapesResources(t *testing.T) {
	got := string(EnvoyProxyYAML("10m", "128Mi"))
	for _, want := range []string{
		"apiVersion: gateway.envoyproxy.io/v1alpha1",
		"kind: EnvoyProxy",
		"name: " + EnvoyProxyName,
		"namespace: " + Namespace,
		"cpu: 10m",
		"memory: 128Mi",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %q:\n%s", want, got)
		}
	}
	if strings.Contains(got, "limits:") {
		t.Errorf("CR should declare requests only, not limits:\n%s", got)
	}
}

// TestControllerResourcesYAML_RequestsOnly pins the partial
// Deployment shape: requests-only, container matched by name so
// SSA targets the right container, no limits/image/env claimed
// (so upstream owners keep them).
func TestControllerResourcesYAML_RequestsOnly(t *testing.T) {
	got := string(ControllerResourcesYAML("10m", "64Mi"))
	for _, want := range []string{
		"kind: Deployment",
		"name: " + DeploymentName,
		"namespace: " + Namespace,
		"- name: envoy-gateway",
		"cpu: 10m",
		"memory: 64Mi",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %q:\n%s", want, got)
		}
	}
	if strings.Contains(got, "limits:") {
		t.Errorf("patch should declare requests only:\n%s", got)
	}
	if strings.Contains(got, "image:") {
		t.Errorf("patch must not claim image (would fight upstream owner):\n%s", got)
	}
}
