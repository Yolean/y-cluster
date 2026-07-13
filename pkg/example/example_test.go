package example

import (
	"strings"
	"testing"
)

// TestManifest_Shape pins the load-bearing fields of the rendered
// manifests. The HTTPRoute hostname must match exactly what
// envoy-gateway sees in the Host header (the curl --resolve trick
// works precisely because routing is by Host, not by IP).
func TestManifest_Shape(t *testing.T) {
	got := string(Manifest(InstallOptions{
		KubectlContext:   "alice-dev",
		GatewayNamespace: "y-cluster-gateway",
		GatewayName:      "default",
		Hostname:         "hello.alice-dev.alice.local.test",
	}))

	for _, want := range []string{
		// Namespace
		"name: y-cluster-example",
		"managed-by: y-cluster",
		// Deployment
		"image: hashicorp/http-echo:1.0.0",
		`-text=y-cluster dev cluster public test endpoint`,
		"runAsNonRoot: true",
		"readOnlyRootFilesystem: true",
		`drop: ["ALL"]`,
		// Service
		"port: 8080",
		// HTTPRoute
		"name: default",
		"namespace: y-cluster-gateway",
		`- "hello.alice-dev.alice.local.test"`,
	} {
		if !strings.Contains(got, want) {
			t.Errorf("manifest missing %q:\n%s", want, got)
		}
	}
}

// TestManifest_HostnameUniquePerInstall: the HTTPRoute hostname is
// the only thing curl --resolve / /etc/hosts will translate to
// the LB IP. Each installation specifies its own hostname; the
// renderer must not bake in a default. Empty hostname is rejected
// at Install time, but the renderer itself just substitutes.
func TestManifest_HostnameUniquePerInstall(t *testing.T) {
	for _, host := range []string{
		"hello.alice-dev.alice.local.test",
		"hello.bob-dev.bob.local.test",
		"foo.bar.example.com",
	} {
		got := string(Manifest(InstallOptions{
			KubectlContext:   "x",
			GatewayNamespace: "y-cluster-gateway",
			GatewayName:      "default",
			Hostname:         host,
		}))
		want := `- "` + host + `"`
		if !strings.Contains(got, want) {
			t.Errorf("hostname %q not in HTTPRoute manifest:\n%s", host, got)
		}
	}
}

// TestPublicResponse_NoSecrets pins that the static response
// remains conservative -- no infrastructure names, no auth
// hints, no request reflection. The test fails if anyone
// edits PublicResponse to mention specific internal hostnames
// or credentials.
func TestPublicResponse_NoSecrets(t *testing.T) {
	for _, banned := range []string{
		"keycloak", "kafka", "redpanda", "mysql", "kubeconfig",
		"HCLOUD_TOKEN", "secret", "password", "auth",
	} {
		if strings.Contains(strings.ToLower(PublicResponse), strings.ToLower(banned)) {
			t.Errorf("PublicResponse mentions %q -- public IP exposure should not name internal infra", banned)
		}
	}
	// Positive: response self-describes as a test endpoint.
	if !strings.Contains(strings.ToLower(PublicResponse), "test") {
		t.Errorf("PublicResponse should self-describe as a test endpoint")
	}
}
