package hetzner

import (
	"bytes"
	"context"
	_ "embed"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"time"

	"go.uber.org/zap"

	"github.com/Yolean/y-cluster/pkg/provision/config"
)

// reaperImage pins the hetznercloud/cli image used by the in-
// cluster expiry Job. Bumping it requires nothing except
// re-provisioning a cluster -- old reaper Jobs still tied to the
// old tag continue to work as long as the tag stays pulled.
//
// alpine-minirootfs base, busybox shell, hcloud binary at
// /ko-app/hcloud, ENTRYPOINT overridden in the Job to /bin/sh
// so we can wrap sleep + hcloud calls in a tiny script.
const reaperImage = "hetznercloud/cli:v1.64.1"

// reaperNamespace is where the Reaper Job + token Secret live.
// Separate namespace makes it easy for the operator to inspect
// (`kubectl -n y-cluster-reaper get jobs,secrets`) and lock down
// via NetworkPolicy if a cluster-side hardening pass ever happens.
const reaperNamespace = "y-cluster-reaper"

// reaperJobName is the Job's metadata.name. Also referenced by the
// rejectUpstream readiness gate (implicit job-name label) and the
// hetzner e2e assertions; keep them in sync when renaming.
const reaperJobName = "reaper"

//go:embed reaper-job.yaml
var reaperJobTemplate string

// kubectlRun executes kubectl with the given args, feeding stdin
// when non-empty and returning combined output. Package-level seam
// so unit tests can intercept the reaper's kubectl interactions
// without a live cluster.
var kubectlRun = func(ctx context.Context, stdin string, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, "kubectl", args...)
	if stdin != "" {
		cmd.Stdin = strings.NewReader(stdin)
	}
	var out bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &out
	err := cmd.Run()
	return out.Bytes(), err
}

// installReaper renders + applies the lifetime expiry Job into the
// cluster. The Job sleeps cfg.MaxRun, then runs cfg.OnExpiry via
// the hcloud API: teardown deletes the server (and the LB when no
// other lb-group members remain); stop issues a graceful ACPI
// shutdown that preserves the server's identity. The token is
// captured at provision time and stored as a Secret in the reaper
// namespace.
//
// Trade-offs that drove this shape (vs. operator-host at(1) or a
// cloud-side scheduled task):
//
//   - Survives operator-machine loss: the trigger lives on the
//     server, so a wiped/retired laptop doesn't strand the
//     resource.
//   - Survives cluster reboots: the Job's backoffLimit lets a
//     node restart (or transient pod failure) re-schedule the
//     Pod, and the sleep starts over, pushing the deadline back
//     rather than firing prematurely.
//   - Limited blast radius: only touches resources whose IDs
//     were captured at provision time. A re-purposed lb-group
//     LB (e.g. another developer's) can't be touched.
//
// What it does NOT cover:
//
//   - Hetzner Certificate cleanup. The reaper only handles the
//     two paid resources (server, LB); cert deletion needs LB
//     detach-then-delete sequencing and is intentionally left
//     to the operator's interactive Teardown. Orphaned certs
//     are free.
//   - SSH key cleanup. Same reasoning -- free, operator can
//     sweep periodically.
//   - Token rotation. The captured HCLOUD_TOKEN value is
//     baked into the Secret. Rotating the token on the
//     operator's side breaks the reaper; documented as a
//     known limitation in the package doc.
func installReaper(ctx context.Context, cfg installReaperOpts, logger *zap.Logger) error {
	if cfg.MaxRun <= 0 {
		return fmt.Errorf("reaper maxRun must be positive, got %s", cfg.MaxRun)
	}
	switch cfg.OnExpiry {
	case "":
		// Empty means the config-level default; resolve it in one
		// place here rather than at every caller.
		cfg.OnExpiry = config.OnExpiryStop
	case config.OnExpiryStop, config.OnExpiryTeardown:
	default:
		return fmt.Errorf("reaper onExpiry %q is not supported; expected %s or %s", cfg.OnExpiry, config.OnExpiryStop, config.OnExpiryTeardown)
	}
	if cfg.HCloudToken == "" {
		return fmt.Errorf("reaper HCLOUD_TOKEN is empty")
	}
	if cfg.ContextName == "" || cfg.ServerID == 0 {
		return fmt.Errorf("reaper context name + server id required")
	}
	manifest := renderReaperManifest(cfg, time.Now())

	logger.Info("installing in-cluster lifetime expiry reaper",
		zap.Duration("maxRun", cfg.MaxRun),
		zap.String("onExpiry", cfg.OnExpiry),
		zap.Int64("serverID", cfg.ServerID),
		zap.Int64("lbID", cfg.LBID),
		zap.String("namespace", reaperNamespace),
	)
	// Pre-clean any previous Job: the pod template is immutable, so
	// a re-arm (start after stop) must delete before apply.
	// --ignore-not-found makes first-provision a no-op; a failure
	// here only warns because the apply below surfaces the real
	// conflict if one exists.
	if out, err := kubectlRun(ctx, "",
		"--context="+cfg.KubectlContext,
		"-n", reaperNamespace,
		"delete", "job", reaperJobName,
		"--ignore-not-found",
	); err != nil {
		logger.Warn("pre-delete of previous reaper Job failed; apply may conflict",
			zap.Error(err), zap.String("output", string(out)))
	}
	if out, err := kubectlRun(ctx, manifest,
		"--context="+cfg.KubectlContext,
		"apply",
		"--server-side", "--force-conflicts",
		"--field-manager=y-cluster",
		"-f", "-",
	); err != nil {
		return fmt.Errorf("kubectl apply reaper: %w: %s", err, string(out))
	}
	return nil
}

// renderReaperManifest substitutes the template placeholders. The
// expires-at annotation is computed here from `now` (injected for
// testability) so the manifest itself carries the earliest possible
// firing time; a pod retry restarts the full sleep and can only
// push the real firing later.
func renderReaperManifest(cfg installReaperOpts, now time.Time) string {
	return strings.NewReplacer(
		"{{IMAGE}}", reaperImage,
		"{{NAMESPACE}}", reaperNamespace,
		"{{SLEEP_SECONDS}}", strconv.FormatInt(int64(cfg.MaxRun.Seconds()), 10),
		"{{MAX_RUN}}", cfg.MaxRun.String(),
		"{{ON_EXPIRY}}", cfg.OnExpiry,
		"{{EXPIRES_AT}}", now.Add(cfg.MaxRun).UTC().Format(time.RFC3339),
		"{{SERVER_ID}}", strconv.FormatInt(cfg.ServerID, 10),
		"{{LB_ID}}", strconv.FormatInt(cfg.LBID, 10),
		"{{LB_GROUP}}", cfg.LBGroup,
		"{{TOKEN}}", cfg.HCloudToken,
	).Replace(reaperJobTemplate)
}

// installReaperOpts narrows the inputs the reaper installer needs
// so unit tests (and the Provision / Start call sites) can wire
// it up explicitly.
type installReaperOpts struct {
	KubectlContext string
	ContextName    string
	HCloudToken    string
	// MaxRun is the sleep window before OnExpiry fires. Sourced
	// from the standard lifetime config (lifetime.maxRun).
	MaxRun time.Duration
	// OnExpiry is config.OnExpiryStop or config.OnExpiryTeardown;
	// empty resolves to stop (the config-level default). Pause is
	// rejected at config validation already (no Hetzner Cloud
	// primitive).
	OnExpiry string
	ServerID int64
	LBID     int64
	LBGroup  string
}

// removeReaperJob deletes the expiry reaper Job (if any) via the
// cluster API. Called from Stop while the cluster is still
// reachable, BEFORE the server shutdown -- lifecycle parity with
// local lifetime semantics where a manual stop ends this run's
// budget. Best-effort: a frozen Job on a stopped server is
// harmless but stale, so a failed delete only warns.
func removeReaperJob(ctx context.Context, kubectlContext string, logger *zap.Logger) {
	out, err := kubectlRun(ctx, "",
		"--context="+kubectlContext,
		"-n", reaperNamespace,
		"delete", "job", reaperJobName,
		"--ignore-not-found",
	)
	if err != nil {
		logger.Warn("could not delete expiry reaper Job before stop; the stale Job re-runs with a full window if the server is started outside y-cluster",
			zap.Error(err), zap.String("output", string(out)))
		return
	}
	logger.Info("expiry reaper Job removed; `y-cluster start` re-arms a fresh window",
		zap.String("context", kubectlContext))
}

// rearmReaper re-installs the expiry reaper with a fresh full
// window. Parity with local lifetime semantics: the budget counts
// from cluster start, so every start re-anchors the deadline.
// No-op when the sidecar carries no lifetime (empty maxRun =
// disabled). Called from Start after poweron; waits for the kube
// API to answer first since the Job apply needs it.
func rearmReaper(ctx context.Context, st state, logger *zap.Logger) error {
	if st.LifetimeMaxRun == "" {
		return nil
	}
	maxRun, err := time.ParseDuration(st.LifetimeMaxRun)
	if err != nil {
		return fmt.Errorf("state lifetimeMaxRun %q: %w", st.LifetimeMaxRun, err)
	}
	if err := waitForKubeAPI(ctx, st.Context, 5*time.Minute, logger); err != nil {
		return err
	}
	return installReaper(ctx, installReaperOpts{
		KubectlContext: st.Context,
		ContextName:    st.Context,
		HCloudToken:    readHCloudToken(),
		MaxRun:         maxRun,
		OnExpiry:       st.LifetimeOnExpiry,
		ServerID:       st.ServerID,
		LBID:           st.LBID,
		LBGroup:        st.LBGroup,
	}, logger)
}

// waitForKubeAPI polls the context's API server /readyz until it
// answers or timeout fires. k3s needs tens of seconds after a
// poweron; anything that wants to kubectl-apply right after Start
// (the reaper re-arm) has to wait this out.
func waitForKubeAPI(ctx context.Context, kubectlContext string, timeout time.Duration, logger *zap.Logger) error {
	logger.Info("waiting for the kube API", zap.String("context", kubectlContext))
	deadline := time.Now().Add(timeout)
	for {
		out, err := kubectlRun(ctx, "", "--context="+kubectlContext, "get", "--raw=/readyz")
		if err == nil {
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("kube API for context %q not ready after %s: %w: %s", kubectlContext, timeout, err, string(out))
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(3 * time.Second):
		}
	}
}

// readHCloudToken pulls the operator's HCLOUD_TOKEN from the env
// (the same source newClient uses for the API client). Captured
// here as a separate helper so unit tests can stub it.
func readHCloudToken() string { return os.Getenv(HCloudTokenEnv) }
