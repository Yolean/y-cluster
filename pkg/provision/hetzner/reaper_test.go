package hetzner

import (
	"strings"
	"testing"
	"time"

	"github.com/Yolean/y-cluster/pkg/provision/config"
)

// reaperTestOpts is the baseline installReaperOpts the rendering
// tests vary. 90m makes the seconds substitution (5400) visibly
// distinct from an hours-based value.
func reaperTestOpts(onExpiry string) installReaperOpts {
	return installReaperOpts{
		KubectlContext: "alice-dev",
		ContextName:    "alice-dev",
		HCloudToken:    "tok-XYZ",
		MaxRun:         90 * time.Minute,
		OnExpiry:       onExpiry,
		ServerID:       12345,
		LBID:           67890,
		LBGroup:        "alice",
	}
}

// reaperTestNow anchors the expires-at annotation assertions.
var reaperTestNow = time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)

// TestReaperManifest_Teardown pins the placeholder list + rendered
// shape for the teardown action so a future tweak to
// reaper-job.yaml that loses a placeholder (or adds one without
// updating renderReaperManifest) fails loudly. The actual kubectl
// apply is exercised by the Hetzner e2e in /e2e (not here).
func TestReaperManifest_Teardown(t *testing.T) {
	rendered := renderReaperManifest(reaperTestOpts(config.OnExpiryTeardown), reaperTestNow)

	for _, want := range []string{
		"image: hetznercloud/cli:", // pinned image
		"name: y-cluster-reaper",   // namespace
		"name: reaper",             // job name (rejectUpstream gate + e2e depend on it)
		// The window: 90m as sleep seconds AND as the readable
		// annotation value.
		`value: "5400"`,
		`y-cluster.yolean.se/max-run: "1h30m0s"`,
		// The action, as env + label + annotation.
		`value: "teardown"`,
		"y-cluster.yolean.se/on-expiry: teardown",
		`y-cluster.yolean.se/on-expiry: "teardown"`,
		// expires-at computed at install: now + maxRun.
		`y-cluster.yolean.se/expires-at: "2026-01-02T04:34:05Z"`,
		`value: "12345"`,       // server id
		`value: "67890"`,       // lb id
		`value: "alice"`,       // lb-group
		`token: "tok-XYZ"`,     // captured operator token
		"hcloud server delete", // canonical teardown step
		"hcloud load-balancer delete",
		"managed-by: y-cluster",
		`sleep "${SLEEP_SECONDS}"`,
		// backoffLimit > 0 is what lets the reaper survive a node
		// reboot / transient pod failure; 0 would permanently kill
		// the expiry action on the first pod loss.
		"backoffLimit: 6",
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

// TestReaperManifest_Stop: the stop action renders the same Job
// with ON_EXPIRY=stop, taking the script's shutdown branch (the
// branch itself is runtime shell; here we pin that the shutdown
// step exists and that the action value flows into env + label +
// annotations).
func TestReaperManifest_Stop(t *testing.T) {
	rendered := renderReaperManifest(reaperTestOpts(config.OnExpiryStop), reaperTestNow)

	for _, want := range []string{
		`value: "stop"`,
		"y-cluster.yolean.se/on-expiry: stop",
		`y-cluster.yolean.se/on-expiry: "stop"`,
		`y-cluster.yolean.se/max-run: "1h30m0s"`,
		`y-cluster.yolean.se/expires-at: "2026-01-02T04:34:05Z"`,
		`value: "5400"`,
		// Graceful ACPI shutdown; no delete of server or LB in
		// this branch.
		"hcloud server shutdown",
		"backoffLimit: 6",
	} {
		if !strings.Contains(rendered, want) {
			t.Errorf("rendered manifest missing %q\n%s", want, rendered)
		}
	}
	if strings.Contains(rendered, `value: "teardown"`) {
		t.Errorf("stop render leaked teardown action value:\n%s", rendered)
	}
	if strings.Contains(rendered, "{{") {
		t.Errorf("rendered manifest still contains placeholders:\n%s", rendered)
	}
}
