package envoygateway

import (
	"strings"
	"testing"

	"sigs.k8s.io/yaml"
)

// miniInstall has the shape of upstream install.yaml where
// RenderInstall looks: a CRD, the controller Deployment with upstream
// requests and limits, and the certgen hook Job.
const miniInstall = `# Source: gateway-helm/crds/x.yaml
apiVersion: apiextensions.k8s.io/v1
kind: CustomResourceDefinition
metadata:
  name: envoyproxies.gateway.envoyproxy.io
spec:
  group: gateway.envoyproxy.io
---
# Source: gateway-helm/templates/envoy-gateway-deployment.yaml
apiVersion: apps/v1
kind: Deployment
metadata:
  name: envoy-gateway
  namespace: envoy-gateway-system
spec:
  template:
    spec:
      containers:
      - name: envoy-gateway
        image: envoyproxy/gateway:v9.9.9
        resources:
          limits:
            memory: 1024Mi
          requests:
            cpu: 100m
            memory: 256Mi
---
apiVersion: batch/v1
kind: Job
metadata:
  name: eg-gateway-helm-certgen
  namespace: envoy-gateway-system
  annotations:
    "helm.sh/hook": pre-install, pre-upgrade
spec:
  ttlSecondsAfterFinished: 30
---
`

func renderedKinds(t *testing.T, out []byte) []string {
	t.Helper()
	var kinds []string
	for _, d := range splitDocuments(out) {
		h, err := header(d)
		if err != nil {
			t.Fatal(err)
		}
		kinds = append(kinds, h.Kind)
	}
	return kinds
}

func TestRenderInstall_ControllerRequestsInTheDeployment(t *testing.T) {
	out, err := RenderInstall([]byte(miniInstall), "10m", "64Mi", "")
	if err != nil {
		t.Fatal(err)
	}
	var dep struct {
		Spec struct {
			Template struct {
				Spec struct {
					Containers []struct {
						Image     string `json:"image"`
						Resources struct {
							Limits   map[string]string `json:"limits"`
							Requests map[string]string `json:"requests"`
						} `json:"resources"`
					} `json:"containers"`
				} `json:"spec"`
			} `json:"template"`
		} `json:"spec"`
	}
	docs := splitDocuments(out)
	if err := yaml.Unmarshal(docs[1], &dep); err != nil {
		t.Fatal(err)
	}
	c := dep.Spec.Template.Spec.Containers[0]
	if c.Resources.Requests["cpu"] != "10m" || c.Resources.Requests["memory"] != "64Mi" {
		t.Errorf("requests %v", c.Resources.Requests)
	}
	if c.Resources.Limits["memory"] != "1024Mi" || c.Image != "envoyproxy/gateway:v9.9.9" {
		t.Errorf("upstream limits and image are kept: %+v", c)
	}
	// The CRD is passed through as it is, comments included.
	if !strings.Contains(string(docs[0]), "# Source: gateway-helm/crds/x.yaml") {
		t.Errorf("CRD document changed:\n%s", docs[0])
	}
}

func TestRenderInstall_EmptyRequestsLeaveUpstream(t *testing.T) {
	out, err := RenderInstall([]byte(miniInstall), "", "", "")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(out), "cpu: 100m") || !strings.Contains(string(out), "# Source: gateway-helm/templates/envoy-gateway-deployment.yaml") {
		t.Errorf("Deployment changed without requests to set:\n%s", out)
	}
}

// The certgen Job is applied when the release is not running yet
// (install) or a different one is (upgrade), and left out on a
// re-apply of the running release.
func TestRenderInstall_HookJobsOnInstallAndUpgradeOnly(t *testing.T) {
	for _, c := range []struct {
		live    string
		wantJob bool
	}{
		{"", true},
		{"envoyproxy/gateway:v9.9.8", true},
		{"envoyproxy/gateway:v9.9.9", false},
	} {
		out, err := RenderInstall([]byte(miniInstall), "10m", "64Mi", c.live)
		if err != nil {
			t.Fatal(err)
		}
		kinds := strings.Join(renderedKinds(t, out), ",")
		if got := strings.Contains(kinds, "Job"); got != c.wantJob {
			t.Errorf("live %q: kinds %s, want the Job %v", c.live, kinds, c.wantJob)
		}
		if !strings.HasPrefix(kinds, "CustomResourceDefinition,Deployment") {
			t.Errorf("live %q: kinds %s", c.live, kinds)
		}
	}
}

func TestControllerImage(t *testing.T) {
	got, err := ControllerImage([]byte(miniInstall))
	if err != nil || got != "envoyproxy/gateway:v9.9.9" {
		t.Errorf("ControllerImage = %q, %v", got, err)
	}
	if _, err := ControllerImage([]byte("kind: ConfigMap\n")); err == nil {
		t.Error("a manifest without the controller must be refused")
	}
}

func TestSplitDocuments(t *testing.T) {
	docs := splitDocuments([]byte("---\n# only a comment\n---\na: 1\n---  \nb: 2\n"))
	if len(docs) != 2 || string(docs[0]) != "a: 1\n" || string(docs[1]) != "b: 2\n" {
		t.Errorf("docs %q", docs)
	}
}
