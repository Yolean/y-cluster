package hetzner

import (
	"strings"
	"testing"
)

// TestReaperManifestSubstitutions pins the placeholder list +
// rendered shape so a future tweak to reaper-job.yaml that loses
// a placeholder (or adds one without updating installReaper)
// fails loudly. The actual kubectl apply is exercised by the
// Hetzner e2e in /e2e (not here).
func TestReaperManifestSubstitutions(t *testing.T) {
	rendered := strings.NewReplacer(
		"{{IMAGE}}", reaperImage,
		"{{NAMESPACE}}", reaperNamespace,
		"{{HOURS}}", "8",
		"{{SERVER_ID}}", "12345",
		"{{LB_ID}}", "67890",
		"{{LB_GROUP}}", "alice",
		"{{TOKEN}}", "tok-XYZ",
	).Replace(reaperJobTemplate)

	for _, want := range []string{
		"image: hetznercloud/cli:",   // pinned image
		"name: y-cluster-reaper",     // namespace
		`value: "8"`,                 // hours
		`value: "12345"`,             // server id
		`value: "67890"`,             // lb id
		`value: "alice"`,             // lb-group
		`token: "tok-XYZ"`,           // captured operator token
		"hcloud server delete",       // canonical reap step
		"hcloud load-balancer delete",
		"managed-by: y-cluster",
		"sleep $((HOURS * 3600))",
	} {
		if !strings.Contains(rendered, want) {
			t.Errorf("rendered manifest missing %q\n%s", want, rendered)
		}
	}

	// Sanity: no placeholders left after substitution.
	if strings.Contains(rendered, "{{") {
		t.Errorf("rendered manifest still contains placeholders:\n%s", rendered)
	}
}
