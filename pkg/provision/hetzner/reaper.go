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

	"go.uber.org/zap"
)

// reaperImage pins the hetznercloud/cli image used by the in-
// cluster auto-teardown Job. Bumping it requires nothing except
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

//go:embed reaper-job.yaml
var reaperJobTemplate string

// installReaper renders + applies the auto-teardown Job into the
// cluster. The Job sleeps cfg.AutoTeardownHours, then issues
// hcloud delete calls for the server and (if no other lb-group
// members remain) the LB. The token is captured at provision
// time and stored as a Secret in the reaper namespace.
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
//   - Limited blast radius: only deletes resources whose IDs
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
	if cfg.Hours <= 0 {
		return fmt.Errorf("reaper hours must be positive, got %d", cfg.Hours)
	}
	if cfg.HCloudToken == "" {
		return fmt.Errorf("reaper HCLOUD_TOKEN is empty")
	}
	if cfg.ContextName == "" || cfg.ServerID == 0 {
		return fmt.Errorf("reaper context name + server id required")
	}
	manifest := strings.NewReplacer(
		"{{IMAGE}}", reaperImage,
		"{{NAMESPACE}}", reaperNamespace,
		"{{HOURS}}", strconv.Itoa(cfg.Hours),
		"{{SERVER_ID}}", strconv.FormatInt(cfg.ServerID, 10),
		"{{LB_ID}}", strconv.FormatInt(cfg.LBID, 10),
		"{{LB_GROUP}}", cfg.LBGroup,
		"{{TOKEN}}", cfg.HCloudToken,
	).Replace(reaperJobTemplate)

	logger.Info("installing in-cluster auto-teardown reaper",
		zap.Int("hours", cfg.Hours),
		zap.Int64("serverID", cfg.ServerID),
		zap.Int64("lbID", cfg.LBID),
		zap.String("namespace", reaperNamespace),
	)
	cmd := exec.CommandContext(ctx, "kubectl",
		"--context="+cfg.KubectlContext,
		"apply",
		"--server-side", "--force-conflicts",
		"--field-manager=y-cluster",
		"-f", "-",
	)
	cmd.Stdin = strings.NewReader(manifest)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("kubectl apply reaper: %w: %s%s", err, stdout.String(), stderr.String())
	}
	return nil
}

// installReaperOpts narrows the inputs the reaper installer needs
// so unit tests (and the existing Provision call site) can wire
// it up explicitly.
type installReaperOpts struct {
	KubectlContext string
	ContextName    string
	HCloudToken    string
	Hours          int
	ServerID       int64
	LBID           int64
	LBGroup        string
}

// readHCloudToken pulls the operator's HCLOUD_TOKEN from the env
// (the same source newClient uses for the API client). Captured
// here as a separate helper so unit tests can stub it.
func readHCloudToken() string { return os.Getenv(HCloudTokenEnv) }
