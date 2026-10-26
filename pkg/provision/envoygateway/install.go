package envoygateway

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"time"

	"go.uber.org/zap"

	"github.com/Yolean/y-cluster/pkg/provision/upgrade"
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
	// InstallSHA256 is the sha256 of Version's install.yaml. Empty is
	// VersionSHA256 for the constant and an error for any other
	// release (see EnsureOptions).
	InstallSHA256 string
	// AllowMinor lets a cluster that runs an older minor of the same
	// major move to Version. Without it that move is refused, like a
	// downgrade (pkg/provision/upgrade).
	AllowMinor bool
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

	// ProxyImage replaces the envoy image that the release would run
	// for the proxy fleet (see EnvoyProxyYAML). Setting it applies the
	// EnvoyProxy CR like ExternalIPs does.
	ProxyImage string

	// ExternalIPs makes the envoy Service reachable on these node
	// addresses, for a cluster with no load balancer implementation
	// (see EnvoyProxyYAML). Empty on every provider that has one:
	// k3s ServiceLB locally and on hetzner. Setting it applies the
	// EnvoyProxy CR even when no proxy resources are configured.
	ExternalIPs []string

	// DaemonSet runs the envoy fleet as a DaemonSet, one pod per
	// schedulable node, for a cluster that lists several nodes in
	// ExternalIPs (see EnvoyProxyYAML). Applies the EnvoyProxy CR
	// like ExternalIPs does.
	DaemonSet bool

	// MergeGateways makes every Gateway of the class share one proxy
	// fleet and one Service. Needed where the Service is published on
	// a single node's ports (k3s ServiceLB): a second Gateway on :80
	// would get its own LoadBalancer Service that ServiceLB cannot
	// bind. Listeners of merged Gateways are told apart by hostname.
	MergeGateways bool
}

// Rendered is what Install applies, in order. EnvoyProxy and
// GatewayClass are nil when Install applies none.
type Rendered struct {
	Version string
	// Installed is the release the cluster runs, empty when none, and
	// Step the move from it to Version.
	Installed    string
	Step         upgrade.Step
	Install      []byte
	EnvoyProxy   []byte
	GatewayClass []byte
}

// Move reads which release the cluster runs, from the image tag of
// the controller Deployment, and applies pkg/provision/upgrade's rule
// to the move from it to opts.Version. A refused move is an
// *upgrade.RefusedError. Render calls it first, so that nothing is
// rendered, and nothing applied, for a move the rule refuses; callers
// that apply other things first (platform.Install) call it before
// those.
func Move(ctx context.Context, opts Options) (installed string, step upgrade.Step, err error) {
	if opts.ContextName == "" {
		return "", 0, fmt.Errorf("envoygateway: ContextName is required")
	}
	_, installed, step, err = move(ctx, opts)
	return installed, step, err
}

// move is Move, plus the live controller image it read.
func move(ctx context.Context, opts Options) (live, installed string, step upgrade.Step, err error) {
	version := Release(opts.Version)
	if live, err = liveControllerImage(ctx, opts.ContextName); err != nil {
		return "", "", 0, err
	}
	if live != "" {
		if installed, err = ReleaseOf(live); err != nil {
			return "", "", 0, fmt.Errorf("envoy gateway: %w, so y-cluster cannot tell which release is installed and applies none; nothing was changed", err)
		}
	}
	if step, err = upgrade.Check(installed, version, opts.AllowMinor); err != nil {
		return "", "", 0, fmt.Errorf("envoy gateway: %w; nothing was changed", err)
	}
	return live, installed, step, nil
}

// ReleaseOf is the release tag of a controller image, v1.9.1 of
// docker.io/envoyproxy/gateway:v1.9.1 or of a mirror's
// <registry>/envoyproxy/gateway:v1.9.1@sha256:<digest>.
func ReleaseOf(image string) (string, error) {
	ref, _, _ := strings.Cut(image, "@")
	i := strings.LastIndex(ref, ":")
	if i < 0 || strings.Contains(ref[i:], "/") {
		return "", fmt.Errorf("the controller image %q has no tag", image)
	}
	repo, tag := ref[:i], ref[i+1:]
	if repo != "envoyproxy/gateway" && !strings.HasSuffix(repo, "/envoyproxy/gateway") {
		return "", fmt.Errorf("the controller image %q is not envoyproxy/gateway", image)
	}
	return tag, nil
}

// Render resolves the per-version install.yaml from cache (downloading
// on first need) and renders every manifest Install applies, without
// changing the cluster. It reads the cluster once: which controller
// image runs, which decides whether the move to opts.Version is
// allowed at all (Move) and whether the chart's hook Jobs are applied
// (see RenderInstall).
func Render(ctx context.Context, opts Options) (Rendered, error) {
	if opts.ContextName == "" {
		return Rendered{}, fmt.Errorf("envoygateway: ContextName is required")
	}
	logger := opts.Logger
	if logger == nil {
		logger = zap.NewNop()
	}
	version := opts.Version
	if version == "" {
		version = Version
	}
	live, installed, step, err := move(ctx, opts)
	if err != nil {
		return Rendered{}, err
	}
	switch step {
	case upgrade.Patch:
		logger.Info("envoy gateway patch upgrade", zap.String("from", installed), zap.String("to", version))
	case upgrade.Minor:
		logger.Warn("envoy gateway minor upgrade: best-effort, CRDs and defaults may change, and objects the older release had are not removed",
			zap.String("from", installed), zap.String("to", version))
	}
	installPath, err := Ensure(ctx, EnsureOptions{
		Version:       version,
		InstallSHA256: opts.InstallSHA256,
		CacheOverride: opts.CacheOverride,
		Logger:        logger,
	})
	if err != nil {
		return Rendered{}, err
	}
	upstream, err := os.ReadFile(installPath)
	if err != nil {
		return Rendered{}, fmt.Errorf("read %s: %w", installPath, err)
	}
	install, err := RenderInstall(upstream, opts.ControllerCPURequest, opts.ControllerMemRequest, live)
	if err != nil {
		return Rendered{}, err
	}
	r := Rendered{Version: version, Installed: installed, Step: step, Install: install}
	// EnvoyProxy is applied before the GatewayClass so its
	// parametersRef resolves on first reconcile. Without proxy
	// settings there is no CR and the GatewayClass has no
	// parametersRef: EG uses its built-in defaults.
	envoyProxyName := ""
	if opts.ProxyCPURequest != "" || opts.ProxyMemRequest != "" || len(opts.ExternalIPs) > 0 || opts.DaemonSet || opts.MergeGateways || opts.ProxyImage != "" {
		envoyProxyName = EnvoyProxyName
		r.EnvoyProxy = EnvoyProxyYAML(opts.ProxyCPURequest, opts.ProxyMemRequest, opts.ExternalIPs, opts.DaemonSet, opts.MergeGateways, opts.ProxyImage)
	}
	if opts.GatewayClassName != "" {
		r.GatewayClass = GatewayClassYAML(opts.GatewayClassName, opts.DNSHintIP, envoyProxyName)
	}
	return r, nil
}

// liveControllerImage is the image of the controller Deployment on the
// cluster, or empty when there is none.
func liveControllerImage(ctx context.Context, contextName string) (string, error) {
	out, err := exec.CommandContext(ctx, "kubectl", "--context="+contextName,
		"-n", Namespace, "get", "deployment", DeploymentName, "--ignore-not-found",
		"-o", `jsonpath={.spec.template.spec.containers[?(@.name=="`+DeploymentName+`")].image}`).Output()
	if err != nil {
		var stderr []byte
		if ee, ok := err.(*exec.ExitError); ok {
			stderr = ee.Stderr
		}
		return "", fmt.Errorf("read the live envoy-gateway Deployment: %s: %w", bytes.TrimSpace(stderr), err)
	}
	return string(bytes.TrimSpace(out)), nil
}

// Install applies what Render renders and waits for the controller
// Deployment to roll out before the EnvoyProxy and the GatewayClass.
//
// Idempotent: a re-run with the same options on a cluster that has
// this release changes nothing (see RenderInstall). The cache lookup
// is cheap when the version directory already holds install.yaml.
//
// An upgrade is the same apply at another release, once Move allows
// it. Nothing is pruned: what the older release had and the newer one
// does not ship stays on the cluster.
//
// Order:
//
//  1. kubectl apply --server-side --force-conflicts install.yaml.
//     kubectl handles CRD-then-CR ordering by retrying on
//     RESTMapping errors -- the upstream EG manifest puts CRDs
//     first so the second pass picks up the in-namespace objects
//     once the CRDs are registered.
//  2. kubectl rollout status deployment/envoy-gateway in
//     envoy-gateway-system (skipped when ReadyTimeout < 0).
//  3. the EnvoyProxy CR, when proxy settings ask for one.
//  4. the default GatewayClass with the configured name (skipped
//     when GatewayClassName is empty).
//
// Everything goes through kubectl shellouts, which keeps
// k8s.io/client-go out of the binary. Stdout / stderr are
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
	r, err := Render(ctx, opts)
	if err != nil {
		return err
	}

	logger.Info("applying envoy-gateway install manifest",
		zap.String("version", r.Version),
		zap.String("installed", r.Installed),
		zap.Stringer("step", r.Step),
		zap.String("namespace", Namespace),
		zap.String("controllerCPU", opts.ControllerCPURequest),
		zap.String("controllerMemory", opts.ControllerMemRequest),
	)
	if err := kubectlApplyStdin(ctx, opts.ContextName, r.Install); err != nil {
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

	if r.EnvoyProxy != nil {
		logger.Info("applying EnvoyProxy CR",
			zap.String("name", EnvoyProxyName),
			zap.String("cpu", opts.ProxyCPURequest),
			zap.String("memory", opts.ProxyMemRequest),
			zap.Strings("externalIPs", opts.ExternalIPs),
			zap.Bool("daemonSet", opts.DaemonSet),
			zap.Bool("mergeGateways", opts.MergeGateways),
			zap.String("proxyImage", opts.ProxyImage),
		)
		if err := kubectlApplyStdin(ctx, opts.ContextName, r.EnvoyProxy); err != nil {
			return fmt.Errorf("apply EnvoyProxy: %w", err)
		}
	}

	if r.GatewayClass != nil {
		logger.Info("applying default GatewayClass",
			zap.String("name", opts.GatewayClassName),
			zap.String("dnsHintIP", opts.DNSHintIP),
		)
		if err := kubectlApplyStdin(ctx, opts.ContextName, r.GatewayClass); err != nil {
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
