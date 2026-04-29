package envoygateway

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"time"

	"go.uber.org/zap"
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
	// GatewayClassName names the default GatewayClass Install
	// applies after the controller is up. Empty means "don't apply
	// a GatewayClass" -- useful when the consumer's kustomize base
	// ships its own.
	//
	// Provision-driven calls fill this from CommonConfig.Gateway.Name
	// (default "y-cluster"); test calls can leave it empty to skip
	// the apply.
	GatewayClassName string
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
//  2. kubectl apply --server-side --force-conflicts install.yaml.
//     kubectl handles CRD-then-CR ordering by retrying on
//     RESTMapping errors -- the upstream EG manifest puts CRDs
//     first so the second pass picks up the in-namespace objects
//     once the CRDs are registered.
//  3. kubectl rollout status deployment/envoy-gateway in
//     envoy-gateway-system (skipped when ReadyTimeout < 0).
//  4. kubectl apply the default GatewayClass with the configured
//     name (skipped when GatewayClassName is empty).
//
// Implementation switched from client-go's typed apply / rollout
// to kubectl shellouts to drop pkg/k8sapply + pkg/k8swait (and
// thereby k8s.io/client-go) from the binary. Stdout / stderr are
// forwarded so the operator sees the same `<kind>/<name>
// serverside-applied` output kubectl prints directly.
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
	if err := kubectlApplyStdin(ctx, opts.ContextName, manifest); err != nil {
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
		if err := kubectlRolloutStatus(ctx, opts.ContextName, DeploymentName, Namespace, timeout); err != nil {
			return fmt.Errorf("wait for %s/%s rollout: %w", Namespace, DeploymentName, err)
		}
	}

	if opts.GatewayClassName != "" {
		logger.Info("applying default GatewayClass",
			zap.String("name", opts.GatewayClassName),
		)
		if err := kubectlApplyStdin(ctx, opts.ContextName, GatewayClassYAML(opts.GatewayClassName)); err != nil {
			return fmt.Errorf("apply GatewayClass: %w", err)
		}
	}
	return nil
}

// kubectlApplyStdin applies the given YAML manifest via:
//
//	kubectl --context=X apply --server-side --force-conflicts \
//	  --field-manager=y-cluster -f -
//
// Stdout/stderr are forwarded to the host process so the operator
// sees `<kind>/<name> serverside-applied` directly. The manifest
// is piped on stdin -- avoids a temp file and leaves no trace on
// disk.
//
// Field manager `y-cluster` matches what pkg/yconverge uses, so a
// re-apply under either path doesn't fight over field ownership.
func kubectlApplyStdin(ctx context.Context, contextName string, manifest []byte) error {
	cmd := exec.CommandContext(ctx, "kubectl",
		"--context="+contextName,
		"apply",
		"--server-side", "--force-conflicts",
		"--field-manager=y-cluster",
		"-f", "-",
	)
	cmd.Stdin = bytes.NewReader(manifest)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("kubectl apply --server-side: %w", err)
	}
	return nil
}

// kubectlRolloutStatus runs `kubectl rollout status deployment/<name>
// -n <ns> --timeout=<timeout>`. The timeout flag accepts Go duration
// strings via .String().
func kubectlRolloutStatus(ctx context.Context, contextName, deployment, namespace string, timeout time.Duration) error {
	cmd := exec.CommandContext(ctx, "kubectl",
		"--context="+contextName,
		"rollout", "status",
		"deployment/"+deployment,
		"-n", namespace,
		"--timeout="+timeout.String(),
	)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("kubectl rollout status: %w", err)
	}
	return nil
}
