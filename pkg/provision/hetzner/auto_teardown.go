package hetzner

import (
	"bytes"
	"context"
	"fmt"
	"os/exec"
	"regexp"
	"strconv"
	"strings"

	"go.uber.org/zap"
)

// at(1) is the host-side scheduler we lean on for auto-teardown.
// Trade-offs that drove this choice over a daemon / cloud-side
// timer:
//
//   - Zero infrastructure: dev's host already has at(1) on Linux
//     (`apt install at` if missing) and macOS (atrun must be
//     enabled, but most devs don't run hetzner provisions from
//     a Mac).
//   - Survives reboots: atd runs missed jobs at next boot, so a
//     laptop closed past the deadline still cleans up on wake.
//   - Per-context isolation: each provision gets its own job id,
//     so teardowns don't fan out -- tearing down one context
//     leaves siblings on their own clocks.
//
// What it does NOT cover:
//
//   - Operator wipes the laptop or never boots it again: the
//     server keeps running and bills until manually killed.
//     Phase 5 polish should add a reaper Job inside the cluster
//     that calls Hetzner API to self-delete; cluster-side timer
//     is the belt-and-braces. (Tracked in HETZNER_PROVISIONER.md
//     phase 5.)
//
// Implementation note: at(1) writes the job line to STDERR, not
// stdout. Parsing combined output is the simplest correct
// approach.

// atJobIDRE matches the line at(1) emits announcing a scheduled
// job: "job 7 at Mon May 12 18:34:00 2025". Matches the same
// shape across BSD at, GNU at, and busybox at.
var atJobIDRE = regexp.MustCompile(`(?m)^job\s+(\d+)\s+at\s+`)

// atSchedule pipes shellCmd into `at now + <hours> hours` and
// returns the job id at(1) assigned. The shell command is run by
// /bin/sh at the scheduled time, with the env captured at submit
// time -- crucially, $HCLOUD_TOKEN -- so a future-self atd run can
// reach the Hetzner API without help.
//
// Returns a clear error if at(1) isn't on $PATH; the caller decides
// whether that's fatal. We bail out of Provision because the
// user-stated requirement ("auto-teardown after configurable
// hours") is mandatory: a half-working provision that silently
// skips it would leak servers.
func atSchedule(ctx context.Context, hours int, shellCmd string, logger *zap.Logger) (int, error) {
	if hours <= 0 {
		return 0, fmt.Errorf("auto-teardown hours must be > 0, got %d", hours)
	}
	if _, err := exec.LookPath("at"); err != nil {
		return 0, fmt.Errorf("at(1) not on $PATH (install with `apt install at` on Debian/Ubuntu, `brew install at` on macOS, then enable atd); auto-teardown depends on it: %w", err)
	}
	cmd := exec.CommandContext(ctx, "at", "now", "+", strconv.Itoa(hours), "hours")
	cmd.Stdin = strings.NewReader(shellCmd + "\n")
	var combined bytes.Buffer
	cmd.Stdout = &combined
	cmd.Stderr = &combined
	if err := cmd.Run(); err != nil {
		return 0, fmt.Errorf("at(1) failed: %w: %s", err, combined.String())
	}
	m := atJobIDRE.FindStringSubmatch(combined.String())
	if m == nil {
		return 0, fmt.Errorf("at(1) succeeded but no job id parsed from output: %s", combined.String())
	}
	id, err := strconv.Atoi(m[1])
	if err != nil {
		return 0, fmt.Errorf("parse job id %q: %w", m[1], err)
	}
	logger.Info("auto-teardown scheduled",
		zap.Int("atJobID", id),
		zap.Int("hours", hours),
	)
	return id, nil
}

// atRemove removes the at(1) job by id. Idempotent: if the job
// already fired or was manually atrm'd, this is a no-op so
// Teardown can be called repeatedly without surfacing a stale-job
// error. Genuine errors (atrm missing, permission denied) still
// log so the operator knows their host has a stranded job.
func atRemove(ctx context.Context, jobID int, logger *zap.Logger) {
	if jobID == 0 {
		return
	}
	if _, err := exec.LookPath("atrm"); err != nil {
		logger.Warn("atrm not on $PATH; cannot cancel auto-teardown",
			zap.Int("atJobID", jobID))
		return
	}
	out, err := exec.CommandContext(ctx, "atrm", strconv.Itoa(jobID)).CombinedOutput()
	if err != nil {
		// atrm exits non-zero if the job is unknown; treat that
		// as already-cancelled rather than a teardown failure.
		logger.Info("atrm reported job not present (already fired or manually removed)",
			zap.Int("atJobID", jobID),
			zap.String("output", strings.TrimSpace(string(out))),
		)
		return
	}
	logger.Info("auto-teardown cancelled", zap.Int("atJobID", jobID))
}
