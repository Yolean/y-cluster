// Package lifetime arms and disarms the host-side timer that fires a
// cluster's auto-expiry action. It is the LOCAL trigger: for a local
// dev cluster the host machine is itself the cost, so a host timer is
// the right place for the trigger (if the host sleeps or logs out,
// the VM is down too, so there is nothing to reap). Paid CLOUD
// resources must NOT be reaped from the provisioning host -- that
// path uses cloud-enforced expiry (GCP max-run-duration) instead and
// never goes through this package.
//
// The timer runs `<y-cluster> lifetime reap --context=<ctx>` at the
// deadline. reap re-reads the persisted deadline and acts only if it
// has truly elapsed, otherwise it re-arms for the remaining window.
// That idempotency makes a stale timer (e.g. one left behind after an
// `extend`) harmless, which is why Disarm is best-effort.
package lifetime

import (
	"fmt"
	"os/exec"
	"strings"
	"time"

	"go.uber.org/zap"
)

// reapInvocation is the argv tail every backend schedules: the
// y-cluster subcommand that performs the expiry check + action.
func reapInvocation(bin, kubeContext string) []string {
	return []string{bin, "lifetime", "reap", "--context=" + kubeContext}
}

// unitName is the transient systemd unit name for a context's timer.
// Sanitized to the systemd unit charset; the context is already
// DNS-label-ish but a kubeconfig context can in principle carry
// characters systemd rejects, so map anything outside [a-z0-9-] to
// '-'.
func unitName(kubeContext string) string {
	var b strings.Builder
	b.WriteString("y-cluster-lifetime-")
	for _, r := range kubeContext {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '-':
			b.WriteRune(r)
		default:
			b.WriteRune('-')
		}
	}
	return b.String()
}

// remainingSeconds clamps a deadline-relative duration to a minimum
// of one second so the timer is always in the future even if the
// deadline is already (nearly) here -- in which case reap fires
// almost immediately and acts.
func remainingSeconds(remaining time.Duration) int {
	s := int(remaining / time.Second)
	if s < 1 {
		return 1
	}
	return s
}

// systemdRunArgs builds the argv for arming via a transient
// `systemd-run --user` timer. `--on-active` is relative to now, so a
// computed remaining window arms the deadline; `--unit` names it so
// status/disarm can find it.
func systemdRunArgs(bin, kubeContext string, remaining time.Duration) []string {
	args := []string{
		"--user",
		"--unit=" + unitName(kubeContext),
		fmt.Sprintf("--on-active=%ds", remainingSeconds(remaining)),
		"--timer-property=AccuracySec=1s",
		"--",
	}
	return append(args, reapInvocation(bin, kubeContext)...)
}

// atTimeSpec renders the `at` time argument. at granularity is
// minutes, so round up to at least one minute.
func atTimeSpec(remaining time.Duration) string {
	mins := int((remaining + time.Minute - 1) / time.Minute)
	if mins < 1 {
		mins = 1
	}
	return fmt.Sprintf("now + %d minutes", mins)
}

// atScript is the shell line piped to `at`. The trailing comment is a
// stable marker so Disarm can find this job among the user's at queue
// (at has no job naming).
func atScript(bin, kubeContext string) string {
	return strings.Join(reapInvocation(bin, kubeContext), " ") +
		" # " + unitName(kubeContext)
}

// Arm schedules the reap for `remaining` from now via systemd-run
// (preferred) or `at` (fallback). It disarms any existing timer for
// the context first so re-arming is idempotent. A nil error means a
// timer is in place; an error means no host timer was armed (the
// persisted deadline still stands, so a manual or external
// `lifetime reap` remains the backstop).
func Arm(bin, kubeContext string, remaining time.Duration, logger *zap.Logger) error {
	if logger == nil {
		logger = zap.NewNop()
	}
	_ = Disarm(kubeContext, logger) // best-effort; ignore "nothing to remove"

	if _, err := exec.LookPath("systemd-run"); err == nil {
		args := systemdRunArgs(bin, kubeContext, remaining)
		out, err := exec.Command("systemd-run", args...).CombinedOutput()
		if err == nil {
			logger.Info("lifetime timer armed (systemd)",
				zap.String("unit", unitName(kubeContext)),
				zap.Duration("in", remaining))
			return nil
		}
		// User bus may be unavailable (e.g. headless without linger);
		// fall through to at rather than failing outright.
		logger.Debug("systemd-run failed; trying at",
			zap.Error(err), zap.ByteString("output", out))
	}

	if _, err := exec.LookPath("at"); err == nil {
		cmd := exec.Command("at", strings.Fields(atTimeSpec(remaining))...)
		cmd.Stdin = strings.NewReader(atScript(bin, kubeContext) + "\n")
		if out, err := cmd.CombinedOutput(); err != nil {
			return fmt.Errorf("at scheduling failed: %w: %s", err, strings.TrimSpace(string(out)))
		}
		logger.Info("lifetime timer armed (at)", zap.Duration("in", remaining))
		return nil
	}

	return fmt.Errorf("no host scheduler available (need systemd-run --user or at); " +
		"the deadline is persisted but will not fire automatically - " +
		"run `y-cluster lifetime reap` from a cron/timer of your choosing")
}

// Disarm removes the context's host timer. Best-effort by design:
// reap re-checks the persisted deadline, so a leftover timer that
// fires is a no-op. Returns nil when nothing needed removing.
func Disarm(kubeContext string, logger *zap.Logger) error {
	if logger == nil {
		logger = zap.NewNop()
	}
	if _, err := exec.LookPath("systemctl"); err == nil {
		// Stopping a transient timer unit also cleans it up.
		_ = exec.Command("systemctl", "--user", "stop", unitName(kubeContext)+".timer").Run()
	}
	if _, err := exec.LookPath("atq"); err == nil {
		for _, id := range atJobIDsFor(kubeContext) {
			if _, err := exec.LookPath("atrm"); err == nil {
				_ = exec.Command("atrm", id).Run()
			}
		}
	}
	return nil
}

// atJobIDsFor returns at(1) job ids whose script carries this
// context's marker. Best-effort: any error yields no ids.
func atJobIDsFor(kubeContext string) []string {
	out, err := exec.Command("atq").Output()
	if err != nil {
		return nil
	}
	marker := unitName(kubeContext)
	var ids []string
	for _, line := range strings.Split(string(out), "\n") {
		fields := strings.Fields(line)
		if len(fields) == 0 {
			continue
		}
		id := fields[0]
		body, err := exec.Command("at", "-c", id).Output()
		if err != nil {
			continue
		}
		if strings.Contains(string(body), marker) {
			ids = append(ids, id)
		}
	}
	return ids
}
