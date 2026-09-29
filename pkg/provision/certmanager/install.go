package certmanager

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"time"

	"go.uber.org/zap"
)

// Namespace is where the release manifest installs cert-manager.
const Namespace = "cert-manager"

// Deployments are the release's three components, all of which must be
// up before a Certificate can be admitted and issued.
var Deployments = []string{"cert-manager", "cert-manager-cainjector", "cert-manager-webhook"}

// DefaultReadyTimeout caps each wait: a rollout, the issuers being
// admitted, the CA being issued. The first provision pulls the images.
const DefaultReadyTimeout = 3 * time.Minute

// Options for Install. ContextName is required.
type Options struct {
	ContextName   string
	Version       string
	CacheOverride string
	Logger        *zap.Logger
	ReadyTimeout  time.Duration
}

// Install applies the pinned release, waits for its deployments, then
// applies y-cluster's issuers and waits for the CA certificate to be
// issued. Idempotent: a re-run re-applies the same objects.
func Install(ctx context.Context, opts Options) error {
	if opts.ContextName == "" {
		return fmt.Errorf("certmanager.Install: ContextName is required")
	}
	logger := opts.Logger
	if logger == nil {
		logger = zap.NewNop()
	}
	version := opts.Version
	if version == "" {
		version = Version
	}
	timeout := opts.ReadyTimeout
	if timeout == 0 {
		timeout = DefaultReadyTimeout
	}
	path, err := Ensure(ctx, EnsureOptions{Version: version, CacheOverride: opts.CacheOverride, Logger: logger})
	if err != nil {
		return err
	}
	manifest, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("read %s: %w", path, err)
	}
	logger.Info("applying cert-manager manifest", zap.String("version", version), zap.String("namespace", Namespace))
	if err := kubectl(ctx, manifest, "--context="+opts.ContextName, "apply", "--server-side", "--force-conflicts",
		"--field-manager=y-cluster", "-f", "-"); err != nil {
		return fmt.Errorf("apply cert-manager.yaml: %w", err)
	}
	for _, d := range Deployments {
		logger.Info("waiting for cert-manager rollout", zap.String("deployment", d), zap.Duration("timeout", timeout))
		if err := kubectl(ctx, nil, "--context="+opts.ContextName, "-n", Namespace, "rollout", "status",
			"deployment/"+d, "--timeout="+timeout.String()); err != nil {
			return fmt.Errorf("wait for %s/%s rollout: %w", Namespace, d, err)
		}
	}
	// A rolled-out webhook can still refuse admission for a few seconds
	// while cainjector patches its CA bundle, so the first apply of a
	// cert-manager resource is retried.
	logger.Info("applying y-cluster issuers", zap.String("ca", CAIssuer))
	deadline := time.Now().Add(timeout)
	for {
		err := kubectl(ctx, IssuersYAML(), "--context="+opts.ContextName, "apply", "--server-side", "--force-conflicts",
			"--field-manager=y-cluster", "-f", "-")
		if err == nil {
			break
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("apply issuers: %w", err)
		}
		logger.Info("cert-manager webhook not admitting yet, retrying", zap.Error(err))
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(5 * time.Second):
		}
	}
	logger.Info("waiting for the CA certificate", zap.String("certificate", Namespace+"/"+CACertificate))
	if err := kubectl(ctx, nil, "--context="+opts.ContextName, "-n", Namespace, "wait", "--for=condition=Ready",
		"certificate/"+CACertificate, "--timeout="+timeout.String()); err != nil {
		return fmt.Errorf("wait for certificate %s/%s: %w", Namespace, CACertificate, err)
	}
	return nil
}

// kubectl runs kubectl with stdin (nil for none), forwarding its output
// so the operator sees what it applied and waited for.
func kubectl(ctx context.Context, stdin []byte, args ...string) error {
	cmd := exec.CommandContext(ctx, "kubectl", args...)
	if stdin != nil {
		cmd.Stdin = bytes.NewReader(stdin)
	}
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	return cmd.Run()
}
