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
	// DNSHintIP, when non-empty, surfaces on the applied GatewayClass
	// as the yolean.se/dns-hint-ip annotation so consumer tooling
	// (ystack's y-k8s-ingress-hosts) can read the host-side dial IP
	// without any user-supplied config. Provision-driven calls fill
	// this from CommonConfig.HostRoutableIP (derived from
	// PortForwards). Empty means: don't set the annotation -- the
	// natural state for cluster topologies that don't tunnel ingress
	// through the host (multi-VM bridged, cloud LB).
	//
	// Ignored when GatewayClassName is empty (no GatewayClass apply).
	DNSHintIP string

	// ControllerCPURequest / ControllerMemRequest set the
	// controller container's resources.requests via SSA. Empty
	// strings mean "leave upstream's defaults"; the provisioner
	// fills these from CommonConfig.Gateway.Resources.Controller.
	ControllerCPURequest string
	ControllerMemRequest string

	// ProxyCPURequest / ProxyMemRequest land on the EnvoyProxy
	// CR the default GatewayClass references via parametersRef.
	// When both are empty, no EnvoyProxy CR is applied and the
	// GatewayClass has no parametersRef -- EG uses its defaults.
	ProxyCPURequest string
	ProxyMemRequest string
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

	// Patch the controller container's resource requests before
	// the rollout wait so the wait sees the ReplicaSet we shaped.
	// Empty strings here mean "leave upstream alone" -- a test
	// path or a config that explicitly wants stock requests.
	if opts.ControllerCPURequest != "" || opts.ControllerMemRequest != "" {
		logger.Info("patching envoy-gateway controller resources",
			zap.String("cpu", opts.ControllerCPURequest),
			zap.String("memory", opts.ControllerMemRequest),
		)
		if err := kubectlPatch(ctx, opts.ContextName, "deployment", DeploymentName, Namespace,
			ControllerResourcesPatch(opts.ControllerCPURequest, opts.ControllerMemRequest),
		); err != nil {
			return fmt.Errorf("patch controller resources: %w", err)
		}
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

	// Apply EnvoyProxy first so the GatewayClass.parametersRef
	// resolves on first reconcile. When proxy resources aren't
	// configured we skip the CR and leave the GatewayClass
	// parametersRef-less -- EG uses its built-in defaults.
	envoyProxyName := ""
	if opts.ProxyCPURequest != "" || opts.ProxyMemRequest != "" {
		envoyProxyName = EnvoyProxyName
		logger.Info("applying EnvoyProxy CR",
			zap.String("name", envoyProxyName),
			zap.String("cpu", opts.ProxyCPURequest),
			zap.String("memory", opts.ProxyMemRequest),
		)
		if err := kubectlApplyStdin(ctx, opts.ContextName,
			EnvoyProxyYAML(opts.ProxyCPURequest, opts.ProxyMemRequest),
		); err != nil {
			return fmt.Errorf("apply EnvoyProxy: %w", err)
		}
	}

	if opts.GatewayClassName != "" {
		logger.Info("applying default GatewayClass",
			zap.String("name", opts.GatewayClassName),
			zap.String("dnsHintIP", opts.DNSHintIP),
			zap.String("envoyProxyRef", envoyProxyName),
		)
		if err := kubectlApplyStdin(ctx, opts.ContextName, GatewayClassYAML(opts.GatewayClassName, opts.DNSHintIP, envoyProxyName)); err != nil {
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

// kubectlPatch applies a strategic-merge patch to a named
// resource via `kubectl patch <kind> <name> -n <ns> --type=strategic
// --patch <yaml>`. Used for the controller resources tweak
// where a full SSA-apply of a partial Deployment fails kubectl's
// client-side schema validation (selector / image required).
//
// The patch is passed as a flag value (not stdin) because
// `kubectl patch` doesn't read the body from stdin by default;
// `--patch-file=/dev/stdin` works but the inline form keeps the
// shellout symmetric with kubectlApplyStdin.
func kubectlPatch(ctx context.Context, contextName, kind, name, namespace string, patch []byte) error {
	cmd := exec.CommandContext(ctx, "kubectl",
		"--context="+contextName,
		"patch", kind, name,
		"-n", namespace,
		"--type=strategic",
		"--patch", string(patch),
		"--field-manager=y-cluster",
	)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("kubectl patch: %w", err)
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
