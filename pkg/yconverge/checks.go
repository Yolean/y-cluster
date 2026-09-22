// Package yconverge provides idempotent Kubernetes convergence with
// CUE-based dependency resolution and post-apply checks.
package yconverge

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"time"

	"go.uber.org/zap"
)

// Check represents a single post-apply verification step.
//
// Fields are a flat union: each kind reads only the subset it
// understands. The CUE schema (cue.mod/.../verify/schema.cue)
// enforces which fields belong to which kind via #Wait /
// #Rollout / #Exec / #Gateway.
type Check struct {
	Kind        string `json:"kind"`
	Resource    string `json:"resource,omitempty"`
	For         string `json:"for,omitempty"`
	Namespace   string `json:"namespace,omitempty"`
	Timeout     string `json:"timeout,omitempty"`
	Command     string `json:"command,omitempty"`
	Description string `json:"description,omitempty"`

	// Gateway-only fields. URL is required; everything else has a
	// documented default. See pkg/yconverge/gateway.go for the
	// dial / discovery / validation semantics.
	URL              string `json:"url,omitempty"`
	ExpectCode       []int  `json:"expectCode,omitempty"`
	ExpectLocation   string `json:"expectLocation,omitempty"`
	Resolve          string `json:"resolve,omitempty"`
	GatewayClassName string `json:"gatewayClassName,omitempty"`
}

// DefaultTimeout is used when a check does not specify a timeout.
const DefaultTimeout = "60s"

// CheckRunner executes checks against a Kubernetes cluster.
type CheckRunner struct {
	Context   string // Kubernetes context name
	Namespace string // resolved namespace
	Logger    *zap.Logger
	// Stdout receives the per-check progress headers ("yconverge
	// check N/total <kind>") and forwarded kubectl output. nil ->
	// os.Stdout. Distinct from Logger (the diagnostic channel)
	// because these lines are part of the user-facing UI, not
	// a log.
	Stdout io.Writer
}

func (r *CheckRunner) progressOut() io.Writer {
	if r.Stdout != nil {
		return r.Stdout
	}
	return os.Stdout
}

// RunAll executes checks in order. A failing check stops execution.
//
// Each check emits a progress header before running:
//
//	yconverge check N/total <kind>
//
// where N is 1-indexed. The header is written before the check
// runs (so a hanging or slow check is visible by the header
// landing without follow-up output) and is silent on success
// past whatever kubectl wait / rollout-status / the exec command
// itself prints.
func (r *CheckRunner) RunAll(ctx context.Context, checks []Check) error {
	out := r.progressOut()
	total := len(checks)
	for i, check := range checks {
		fmt.Fprintf(out, "yconverge check %d/%d %s\n", i+1, total, check.Kind)
		if err := r.runOne(ctx, check); err != nil {
			return &CheckError{
				Index: i,
				Check: check,
				Err:   err,
			}
		}
	}
	return nil
}

func (r *CheckRunner) runOne(ctx context.Context, check Check) error {
	timeout, err := parseDuration(check.Timeout)
	if err != nil {
		return fmt.Errorf("invalid timeout %q: %w", check.Timeout, err)
	}

	ns := check.Namespace
	if ns == "" {
		ns = r.Namespace
	}

	switch check.Kind {
	case "wait":
		return r.runWait(ctx, check, ns, timeout)
	case "rollout":
		return r.runRollout(ctx, check, ns, timeout)
	case "exec":
		return r.runExec(ctx, check, timeout)
	case "gateway":
		return r.runGateway(ctx, check, timeout)
	default:
		return fmt.Errorf("unknown check kind: %q", check.Kind)
	}
}

// runGateway executes a `kind: "gateway"` check: discover the
// Gateway address, launch an in-cluster curl Pod with --resolve
// pinned to that address, validate the response code and (when
// configured) Location header. Retries on failure until timeout
// using the same 2s interval as runExec, since the common
// transient failure modes (Gateway not yet programmed, HTTPRoute
// not yet reconciled, backend not yet Ready) all resolve in
// seconds.
func (r *CheckRunner) runGateway(ctx context.Context, check Check, timeout time.Duration) error {
	if check.URL == "" {
		return fmt.Errorf("gateway check: url is required")
	}
	desc := check.Description
	if desc == "" {
		desc = fmt.Sprintf("gateway %s", check.URL)
	}
	r.Logger.Debug("check",
		zap.String("kind", "gateway"),
		zap.String("url", check.URL),
		zap.String("description", desc),
	)
	opts := gatewayProbeOpts{
		URL:              check.URL,
		ExpectCodes:      check.ExpectCode,
		ExpectLocation:   check.ExpectLocation,
		Resolve:          check.Resolve,
		GatewayClassName: check.GatewayClassName,
	}
	deadline := time.Now().Add(timeout)
	var lastErr error
	for {
		// Bounded like an exec attempt: the probe pod's image pull
		// alone can outlast a short timeout.
		attemptCtx, cancel := context.WithDeadline(ctx, deadline)
		err := runGatewayProbe(attemptCtx, r.Context, opts)
		cancel()
		if err == nil {
			return nil
		}
		lastErr = err
		if time.Now().Add(checkRetryInterval).After(deadline) {
			return fmt.Errorf("gateway check timed out after %s: %w", timeout, lastErr)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(checkRetryInterval):
		}
	}
}

func (r *CheckRunner) runWait(ctx context.Context, check Check, ns string, timeout time.Duration) error {
	desc := check.Description
	if desc == "" {
		desc = fmt.Sprintf("wait %s %s", check.Resource, check.For)
	}
	r.Logger.Debug("check",
		zap.String("kind", "wait"),
		zap.String("resource", check.Resource),
		zap.String("description", desc),
	)
	if err := kubectlWait(ctx, r.progressOut(), r.Context, check.Resource, ns, check.For, timeout); err != nil {
		r.Logger.Error("wait check failed",
			zap.String("resource", check.Resource),
			zap.String("for", check.For),
			zap.Error(err),
		)
		return err
	}
	return nil
}

func (r *CheckRunner) runRollout(ctx context.Context, check Check, ns string, timeout time.Duration) error {
	desc := check.Description
	if desc == "" {
		desc = fmt.Sprintf("rollout %s", check.Resource)
	}
	r.Logger.Debug("check",
		zap.String("kind", "rollout"),
		zap.String("resource", check.Resource),
		zap.String("description", desc),
	)
	if err := kubectlRolloutStatus(ctx, r.progressOut(), r.Context, check.Resource, ns, timeout); err != nil {
		r.Logger.Error("rollout check failed",
			zap.String("resource", check.Resource),
			zap.Error(err),
		)
		return err
	}
	return nil
}

func (r *CheckRunner) runExec(ctx context.Context, check Check, timeout time.Duration) error {
	r.Logger.Debug("check",
		zap.String("kind", "exec"),
		zap.String("description", check.Description),
	)

	deadline := time.Now().Add(timeout)
	var lastErr error
	var lastOut []byte
	for {
		out, err := r.execAttempt(ctx, check.Command, deadline)
		if err == nil {
			// Only the attempt that passed is shown. Failed
			// attempts are the normal shape of waiting for a
			// condition and would bury it.
			_, _ = r.progressOut().Write(out)
			return nil
		}
		lastErr, lastOut = err, out
		// An attempt that cannot start before the deadline is not
		// made: it would be killed at once and replace the last real
		// failure with "context deadline exceeded".
		if time.Now().Add(checkRetryInterval).After(deadline) {
			return fmt.Errorf("exec check timed out after %s: %w%s", timeout, lastErr, execOutputSuffix(lastOut))
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(checkRetryInterval):
		}
	}
}

// checkRetryInterval spaces the attempts of exec and gateway checks.
const checkRetryInterval = 2 * time.Second

// execAttempt runs the check command once and returns its combined
// output. The attempt is bounded by the check's deadline, so a
// command that hangs cannot outlive the timeout it was given.
func (r *CheckRunner) execAttempt(ctx context.Context, command string, deadline time.Time) ([]byte, error) {
	attemptCtx, cancel := context.WithDeadline(ctx, deadline)
	defer cancel()
	cmd := exec.CommandContext(attemptCtx, "sh", "-c", command)
	cmd.Env = append(cmd.Environ(),
		"CONTEXT="+r.Context,
		"NAMESPACE="+r.Namespace,
	)
	// Killing sh leaves a child it started (sleep, curl, kubectl)
	// holding the output pipe. Without a WaitDelay, CombinedOutput
	// would wait for that child instead of returning at the deadline.
	cmd.WaitDelay = time.Second
	return cmd.CombinedOutput()
}

// execOutputMax bounds how much of a failed command's output goes
// into the error.
const execOutputMax = 2000

// execOutputSuffix renders the last attempt's output for the timeout
// error: "exit status 1" alone says nothing about why.
func execOutputSuffix(out []byte) string {
	trimmed := strings.TrimSpace(string(out))
	if trimmed == "" {
		return ""
	}
	if len(trimmed) > execOutputMax {
		trimmed = "..." + trimmed[len(trimmed)-execOutputMax:]
	}
	return "\nlast attempt's output:\n" + trimmed
}

func parseDuration(s string) (time.Duration, error) {
	if s == "" {
		s = DefaultTimeout
	}
	return time.ParseDuration(s)
}

// CheckError wraps a check failure with index and check context.
type CheckError struct {
	Index int
	Check Check
	Err   error
}

func (e *CheckError) Error() string {
	desc := e.Check.Description
	if desc == "" {
		desc = fmt.Sprintf("%s %s", e.Check.Kind, e.Check.Resource)
	}
	return fmt.Sprintf("check %d (%s): %v", e.Index, desc, e.Err)
}

func (e *CheckError) Unwrap() error { return e.Err }
