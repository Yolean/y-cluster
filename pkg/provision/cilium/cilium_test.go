package cilium

import (
	"strings"
	"testing"
)

// The embedded manifest is what render.sh says it is: the pinned
// version, and the two settings the glesys provider depends on.
func TestManifest(t *testing.T) {
	if Version == "unknown" || !strings.HasPrefix(Version, "1.") {
		t.Errorf("version not readable from the manifest: %q", Version)
	}
	for _, want := range []string{
		`enable-wireguard: "true"`,
		`encrypt-node: "true"`,
		`kube-proxy-replacement: "false"`,
		`ipam: "kubernetes"`,
		"kind: DaemonSet",
	} {
		if !strings.Contains(string(manifest), want) {
			t.Errorf("manifest lacks %q", want)
		}
	}
}
