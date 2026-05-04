package echo

import (
	"strings"
	"testing"
)

func TestRender_Defaults(t *testing.T) {
	got, err := Render(Options{})
	if err != nil {
		t.Fatal(err)
	}
	body := string(got)

	for _, want := range []string{
		"kind: Namespace",
		"kind: Gateway",
		"kind: Deployment",
		"kind: Service",
		"kind: HTTPRoute",
		"namespace: y-cluster",
		"gatewayClassName: y-cluster",
		"image: ghcr.io/yolean/envoy:echo-v1.38.0@sha256:",
		"value: /q/envoy/echo",
		"type: PathPrefix",
		"name: POD_NAME",
		"name: NODE_NAME",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("rendered manifest missing %q\n%s", want, body)
		}
	}

	// HTTPRoute must NOT pin a hostname -- the request was "any
	// hostname", which Gateway API expresses as the absence of
	// `hostnames:` on the HTTPRoute.
	if strings.Contains(body, "hostnames:") {
		t.Errorf("HTTPRoute should not pin hostnames; body:\n%s", body)
	}
}

// TestRender_RespectsOverrides pins the namespace and
// gateway-class overrides so a custom appliance config (e.g.
// gateway.className: eg) renders manifests that line up with
// the cluster's actual GatewayClass.
func TestRender_RespectsOverrides(t *testing.T) {
	got, err := Render(Options{
		Namespace:    "test-ns",
		GatewayClass: "eg",
		Image:        "ghcr.io/yolean/echo:test",
	})
	if err != nil {
		t.Fatal(err)
	}
	body := string(got)

	for _, want := range []string{
		"namespace: test-ns",
		"name: test-ns", // the Namespace resource itself
		"gatewayClassName: eg",
		"image: ghcr.io/yolean/echo:test",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("rendered manifest missing %q\n%s", want, body)
		}
	}
	if strings.Contains(body, "namespace: y-cluster") {
		t.Errorf("default namespace leaked into customised render:\n%s", body)
	}
}

func TestDeploy_RequiresContext(t *testing.T) {
	err := Deploy(t.Context(), Options{})
	if err == nil {
		t.Fatal("Deploy with empty ContextName should error")
	}
	if !strings.Contains(err.Error(), "ContextName is required") {
		t.Fatalf("error should name the missing field: %v", err)
	}
}
