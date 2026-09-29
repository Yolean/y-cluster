package cilium

import (
	"strings"
	"testing"
)

// The embedded manifests are what render.sh says they are: the pinned
// version, the shared settings the provisioners depend on, and each
// variant's own.
func TestManifests(t *testing.T) {
	if Version == "unknown" || !strings.HasPrefix(Version, "1.") {
		t.Errorf("version not readable from the manifest: %q", Version)
	}
	shared := []string{
		`enable-wireguard: "true"`,
		`encrypt-node: "true"`,
		`kube-proxy-replacement: "false"`,
		`ipam: "kubernetes"`,
		"kind: DaemonSet",
		"quay.io/cilium/cilium:v" + Version,
	}
	for _, c := range []struct {
		variant Variant
		want    []string
	}{
		{Talos, []string{`cgroup-root: "/sys/fs/cgroup"`}},
		{K3s, []string{
			"cni-chaining-mode: portmap",
			`default-lb-service-ipam: "none"`,
			"/opt/cni/bin",
			"/etc/cni/net.d",
		}},
	} {
		m, err := Manifest(c.variant)
		if err != nil {
			t.Fatal(err)
		}
		for _, want := range append(shared, c.want...) {
			if !strings.Contains(string(m), want) {
				t.Errorf("%s manifest lacks %q", c.variant, want)
			}
		}
	}
	if _, err := Manifest("flannel"); err == nil {
		t.Error("unknown variant accepted")
	}
}

// helm's default for Hubble TLS generates a CA at render time, which
// would be embedded here and shared by every cluster. render.sh uses
// the chart's cronJob generator instead, so no manifest may carry a
// Secret.
func TestManifestsCarryNoSecrets(t *testing.T) {
	for _, v := range []Variant{Talos, K3s} {
		m, _ := Manifest(v)
		for _, doc := range strings.Split(string(m), "\n---") {
			if strings.Contains(doc, "\nkind: Secret\n") || strings.HasPrefix(strings.TrimSpace(doc), "kind: Secret") {
				t.Errorf("%s manifest contains a Secret:\n%.300s", v, doc)
			}
		}
		if !strings.Contains(string(m), "hubble-generate-certs") {
			t.Errorf("%s manifest lacks the in-cluster Hubble certificate generator", v)
		}
	}
}
