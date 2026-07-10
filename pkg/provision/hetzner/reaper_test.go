package hetzner

import (
	"context"
	"strings"
	"testing"
	"time"

	"go.uber.org/zap"

	"github.com/Yolean/y-cluster/pkg/provision/config"
)

// kubectlCall records one intercepted kubectlRun invocation.
type kubectlCall struct {
	stdin string
	args  []string
}

// stubKubectl swaps the kubectlRun seam for a recorder that always
// succeeds, restoring the real one on test cleanup.
func stubKubectl(t *testing.T) *[]kubectlCall {
	t.Helper()
	var calls []kubectlCall
	orig := kubectlRun
	kubectlRun = func(_ context.Context, stdin string, args ...string) ([]byte, error) {
		calls = append(calls, kubectlCall{stdin: stdin, args: args})
		return nil, nil
	}
	t.Cleanup(func() { kubectlRun = orig })
	return &calls
}

// argsContain reports whether every want string is among args.
func argsContain(args []string, want ...string) bool {
	have := map[string]bool{}
	for _, a := range args {
		have[a] = true
	}
	for _, w := range want {
		if !have[w] {
			return false
		}
	}
	return true
}

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

// TestInstallReaper_DeleteThenApply: the Job's pod template is
// immutable, so installReaper must delete any previous Job before
// the server-side apply. This ordering is what makes the same
// function serve both first provision (delete is a no-op) and the
// re-arm from Start.
func TestInstallReaper_DeleteThenApply(t *testing.T) {
	calls := stubKubectl(t)

	if err := installReaper(context.Background(), reaperTestOpts(config.OnExpiryStop), zap.NewNop()); err != nil {
		t.Fatalf("installReaper: %v", err)
	}
	if len(*calls) != 2 {
		t.Fatalf("want delete + apply (2 kubectl calls), got %d: %+v", len(*calls), *calls)
	}
	del, apply := (*calls)[0], (*calls)[1]
	if !argsContain(del.args, "--context=alice-dev", "delete", "job", "reaper", "--ignore-not-found", reaperNamespace) {
		t.Errorf("delete call args unexpected: %v", del.args)
	}
	if !argsContain(apply.args, "--context=alice-dev", "apply", "--server-side") {
		t.Errorf("apply call args unexpected: %v", apply.args)
	}
	if !strings.Contains(apply.stdin, `value: "5400"`) || !strings.Contains(apply.stdin, `value: "stop"`) {
		t.Errorf("apply manifest missing window / action:\n%s", apply.stdin)
	}
}

// TestInstallReaper_RejectsPause: pause never reaches the cluster;
// config validation rejects it and installReaper backstops.
func TestInstallReaper_RejectsPause(t *testing.T) {
	calls := stubKubectl(t)
	opts := reaperTestOpts(config.OnExpiryPause)
	err := installReaper(context.Background(), opts, zap.NewNop())
	if err == nil || !strings.Contains(err.Error(), "not supported") {
		t.Fatalf("want not-supported error for pause, got %v", err)
	}
	if len(*calls) != 0 {
		t.Errorf("no kubectl calls expected on rejected action, got %+v", *calls)
	}
}

// TestRemoveReaperJob: Stop's pre-shutdown cleanup issues exactly
// one namespaced delete against the given context. Best-effort by
// contract, so there is no error to assert; the args are the
// load-bearing part.
func TestRemoveReaperJob(t *testing.T) {
	calls := stubKubectl(t)
	removeReaperJob(context.Background(), "alice-dev", zap.NewNop())
	if len(*calls) != 1 {
		t.Fatalf("want 1 kubectl call, got %d: %+v", len(*calls), *calls)
	}
	if !argsContain((*calls)[0].args, "--context=alice-dev", "-n", reaperNamespace, "delete", "job", "reaper", "--ignore-not-found") {
		t.Errorf("delete call args unexpected: %v", (*calls)[0].args)
	}
}

// TestRearmReaper_FreshWindow: Start's re-arm waits for the kube
// API, then delete+applies a Job whose window is the FULL maxRun
// from the sidecar -- not a remainder. That is the local-parity
// contract: a stop ends the budget and a start re-arms from
// scratch.
func TestRearmReaper_FreshWindow(t *testing.T) {
	calls := stubKubectl(t)
	t.Setenv(HCloudTokenEnv, "tok-XYZ")

	st := state{
		Context:          "alice-dev",
		ServerID:         12345,
		LBID:             67890,
		LBGroup:          "alice",
		LifetimeMaxRun:   "90m",
		LifetimeOnExpiry: config.OnExpiryTeardown,
	}
	if err := rearmReaper(context.Background(), st, zap.NewNop()); err != nil {
		t.Fatalf("rearmReaper: %v", err)
	}
	if len(*calls) != 3 {
		t.Fatalf("want readyz + delete + apply (3 kubectl calls), got %d: %+v", len(*calls), *calls)
	}
	if !argsContain((*calls)[0].args, "--context=alice-dev", "get", "--raw=/readyz") {
		t.Errorf("first call should probe /readyz: %v", (*calls)[0].args)
	}
	apply := (*calls)[2]
	if !strings.Contains(apply.stdin, `value: "5400"`) {
		t.Errorf("re-armed manifest should carry the full 90m window:\n%s", apply.stdin)
	}
	if !strings.Contains(apply.stdin, `value: "teardown"`) {
		t.Errorf("re-armed manifest should keep the persisted action:\n%s", apply.stdin)
	}
}

// TestRearmReaper_NoLifetimeNoop: an empty LifetimeMaxRun in the
// sidecar means no lifetime was configured at provision; start
// must not conjure a reaper (or even touch kubectl).
func TestRearmReaper_NoLifetimeNoop(t *testing.T) {
	calls := stubKubectl(t)
	st := state{Context: "alice-dev", ServerID: 12345}
	if err := rearmReaper(context.Background(), st, zap.NewNop()); err != nil {
		t.Fatalf("rearmReaper: %v", err)
	}
	if len(*calls) != 0 {
		t.Errorf("no kubectl calls expected without a lifetime, got %+v", *calls)
	}
}
