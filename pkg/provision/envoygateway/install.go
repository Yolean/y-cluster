package envoygateway

import (
	"context"
	"fmt"
	"os"
	"time"

	"go.uber.org/zap"

	"github.com/Yolean/y-cluster/pkg/k8sapply"
	"github.com/Yolean/y-cluster/pkg/k8swait"
)

// Namespace is where the EG controller and its services live in
// the bundled install.
const Namespace = "envoy-gateway-system"

// DeploymentName is the EG controller Deployment k8swait.RolloutStatus
// targets to know when EG is ready to handle CRs.
const DeploymentName = "envoy-gateway"

// DefaultReadyTimeout caps how long Install waits for the EG
// controller Deployment to roll out. 3 minutes is generous for a
// fresh image pull on a slow cluster.
const DefaultReadyTimeout = 3 * time.Minute

// Options controls Install. ContextName is the only required
// field; everything else has a sensible zero value.
type Options struct {
	// ContextName picks the kubeconfig context to apply against.
	ContextName string
	// Version overrides the pinned constant. Empty -> Version.
	Version string
	// CacheOverride redirects the per-version cache root. Empty
	// -> pkg/cache resolution order (XDG default).
	CacheOverride string
	// Logger receives the per-step log lines. nil -> discard.
	Logger *zap.Logger
	// ReadyTimeout overrides DefaultReadyTimeout for the wait
	// step. A negative value skips the wait entirely (used by
	// kwok-based tests where the controller never actually rolls
	// out a real Deployment).
	ReadyTimeout time.Duration
	// SkipGatewayClass omits applying the default `eg`
	// GatewayClass. Useful when the consumer's kustomize base
	// declares its own GatewayClass under a different name.
	SkipGatewayClass bool
}

// Install resolves the per-version install.yaml from cache
// (downloading on first need), applies it to the cluster, waits
// for the controller Deployment to roll out, and applies the
// default `eg` GatewayClass.
//
// Idempotent: re-running on a cluster that already has EG
// installed reconciles via SSA. The cache lookup is cheap when
// the version directory already holds install.yaml.
//
// Order:
//
//  1. Ensure install.yaml is cached for the resolved version.
//  2. Apply install.yaml (CRDs first via k8sapply's CRD-aware
//     ordering, then the rest).
//  3. Wait for envoy-gateway Deployment in envoy-gateway-system
//     to finish rolling out (skipped when ReadyTimeout < 0).
//  4. Apply the default `eg` GatewayClass (skipped when
//     SkipGatewayClass is set).
func Install(ctx context.Context, opts Options) error {
	if opts.ContextName == "" {
		return fmt.Errorf("envoygateway.Install: ContextName is required")
	}
	logger := opts.Logger
	if logger == nil {
		logger = zap.NewNop()
	}
	version := opts.Version
	if version == "" {
		version = Version
	}

	installPath, err := Ensure(ctx, EnsureOptions{
		Version:       version,
		CacheOverride: opts.CacheOverride,
		Logger:        logger,
	})
	if err != nil {
		return err
	}
	manifest, err := os.ReadFile(installPath)
	if err != nil {
		return fmt.Errorf("read %s: %w", installPath, err)
	}

	logger.Info("applying envoy-gateway install manifest",
		zap.String("version", version),
		zap.String("namespace", Namespace),
	)
	if err := k8sapply.ApplyYAML(ctx, opts.ContextName, manifest, k8sapply.DryRunNone, logger); err != nil {
		return fmt.Errorf("apply install.yaml: %w", err)
	}

	if opts.ReadyTimeout >= 0 {
		timeout := opts.ReadyTimeout
		if timeout == 0 {
			timeout = DefaultReadyTimeout
		}
		logger.Info("waiting for envoy-gateway rollout",
			zap.String("namespace", Namespace),
			zap.String("deployment", DeploymentName),
			zap.Duration("timeout", timeout),
		)
		if err := k8swait.RolloutStatus(ctx, opts.ContextName,
			"deployment/"+DeploymentName, Namespace, timeout); err != nil {
			return fmt.Errorf("wait for %s/%s rollout: %w", Namespace, DeploymentName, err)
		}
	}

	if !opts.SkipGatewayClass {
		logger.Info("applying default GatewayClass eg")
		if err := k8sapply.ApplyYAML(ctx, opts.ContextName, gatewayClassYAML, k8sapply.DryRunNone, logger); err != nil {
			return fmt.Errorf("apply GatewayClass: %w", err)
		}
	}
	return nil
}
