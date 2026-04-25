// Package yconverge provides idempotent Kubernetes convergence with
// CUE-based dependency resolution and post-apply checks.
package yconverge

import (
	"context"
	"fmt"
	"os/exec"
	"time"

	"go.uber.org/zap"
)

// Check represents a single post-apply verification step.
type Check struct {
	Kind        string `json:"kind"`
	Resource    string `json:"resource,omitempty"`
	For         string `json:"for,omitempty"`
	Namespace   string `json:"namespace,omitempty"`
	Timeout     string `json:"timeout,omitempty"`
	Command     string `json:"command,omitempty"`
	Description string `json:"description,omitempty"`
}

// DefaultTimeout is used when a check does not specify a timeout.
const DefaultTimeout = "60s"

// CheckRunner executes checks against a Kubernetes cluster.
type CheckRunner struct {
	Context   string // Kubernetes context name
	Namespace string // resolved namespace
	Logger    *zap.Logger
}

// RunAll executes checks in order. A failing check stops execution.
func (r *CheckRunner) RunAll(ctx context.Context, checks []Check) error {
	for i, check := range checks {
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
	default:
		return fmt.Errorf("unknown check kind: %q", check.Kind)
	}
}

func (r *CheckRunner) runWait(ctx context.Context, check Check, ns string, timeout time.Duration) error {
	desc := check.Description
	if desc == "" {
		desc = fmt.Sprintf("wait %s %s", check.Resource, check.For)
	}
	r.Logger.Info("check",
		zap.String("kind", "wait"),
		zap.String("resource", check.Resource),
		zap.String("description", desc),
	)

	args := []string{"--context=" + r.Context, "wait",
		"--for=" + check.For,
		"--timeout=" + formatDuration(timeout),
	}
	if ns != "" {
		args = append(args, "-n", ns)
	}
	args = append(args, check.Resource)
	return r.kubectl(ctx, args...)
}

func (r *CheckRunner) runRollout(ctx context.Context, check Check, ns string, timeout time.Duration) error {
	desc := check.Description
	if desc == "" {
		desc = fmt.Sprintf("rollout %s", check.Resource)
	}
	r.Logger.Info("check",
		zap.String("kind", "rollout"),
		zap.String("resource", check.Resource),
		zap.String("description", desc),
	)

	args := []string{"--context=" + r.Context, "rollout", "status",
		"--timeout=" + formatDuration(timeout),
	}
	if ns != "" {
		args = append(args, "-n", ns)
	}
	args = append(args, check.Resource)
	return r.kubectl(ctx, args...)
}

func (r *CheckRunner) runExec(ctx context.Context, check Check, timeout time.Duration) error {
	r.Logger.Info("check",
		zap.String("kind", "exec"),
		zap.String("description", check.Description),
	)

	deadline := time.Now().Add(timeout)
	var lastErr error
	for {
		cmd := exec.CommandContext(ctx, "sh", "-c", check.Command)
		cmd.Env = append(cmd.Environ(),
			"CONTEXT="+r.Context,
			"NAMESPACE="+r.Namespace,
		)
		if err := cmd.Run(); err == nil {
			return nil
		} else {
			lastErr = err
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("exec check timed out after %s: %w", timeout, lastErr)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(2 * time.Second):
		}
	}
}

func (r *CheckRunner) kubectl(ctx context.Context, args ...string) error {
	cmd := exec.CommandContext(ctx, "kubectl", args...)
	cmd.Stdout = nil
	cmd.Stderr = nil
	return cmd.Run()
}

func parseDuration(s string) (time.Duration, error) {
	if s == "" {
		s = DefaultTimeout
	}
	return time.ParseDuration(s)
}

func formatDuration(d time.Duration) string {
	return fmt.Sprintf("%ds", int(d.Seconds()))
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
