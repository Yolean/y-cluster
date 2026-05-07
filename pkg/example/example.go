// Package example installs and removes the y-cluster public test
// workload. The workload is the simplest thing that proves the
// path operator-host -> Hetzner LB -> node:80 -> envoy-gateway ->
// HTTPRoute -> backend Pod is wired correctly: a static-text
// hashicorp/http-echo Pod, fronted by an HTTPRoute on the
// per-cluster default Gateway, returning a clearly-public message
// that names the dev-cluster context.
//
// The workload is intentionally safe to expose on a public IP:
//
//   - http-echo binds to a single port and replies with one
//     pre-configured static string, regardless of method, path,
//     or body. No filesystem, no exec, no DB, no upstream calls.
//   - The HTTPRoute hostname constraint scopes which Host: header
//     reaches the workload; envoy-gateway 404s any other Host.
//   - Resource limits cap CPU + memory so a misbehaving client
//     can't starve the rest of the cluster.
//   - Container runs as non-root with a read-only root filesystem
//     and dropped capabilities. http-echo doesn't need any of the
//     Linux capabilities it'd inherit from the default policy.
//
// Inputs come from the cmd/y-cluster `gateway example` subcommand:
// the kubeconfig context, plus the FQDN the operator wants to hit.
// The hostname is required (no default) -- it has to match the
// per-cluster wildcard the default Gateway listens on, and only
// the operator knows their context's FQDN.
package example

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"strings"

	"go.uber.org/zap"
)

// Namespace is where the example Deployment / Service / HTTPRoute
// land. Separate from y-cluster-gateway (the Gateway resource's
// home) so `uninstall` can sweep with a single
// `kubectl delete namespace`.
const Namespace = "y-cluster-example"

// WorkloadName is the conventional name shared by Deployment,
// Service, HTTPRoute, and the app label.
const WorkloadName = "hello"

// Image pins the http-echo build the workload runs. hashicorp's
// official image; tiny (~7MB), well-maintained, and does exactly
// one thing: serve a static text response on a configured port.
const Image = "hashicorp/http-echo:1.0.0"

// PublicResponse is the static text http-echo returns. Crafted
// to be safe on a public IP -- self-describing about what the
// endpoint is, names no internal infrastructure, contains no
// secrets, and points anyone confused to upstream documentation.
const PublicResponse = "y-cluster dev cluster public test endpoint. " +
	"This is a Yolean development environment health-check endpoint -- " +
	"no private data, no upstream calls. " +
	"See https://github.com/Yolean for project context."

// InstallOptions controls the example install. KubectlContext is
// required (the kubeconfig context name); GatewayNamespace +
// GatewayName describe which Gateway the HTTPRoute attaches to;
// Hostname is the FQDN the operator wants to hit -- must match
// the default Gateway's wildcard listener.
type InstallOptions struct {
	KubectlContext   string
	GatewayNamespace string
	GatewayName      string
	Hostname         string
	Logger           *zap.Logger
}

// Install applies the namespace, deployment, service and HTTPRoute
// to the cluster. SSA-style, idempotent: re-running with the same
// hostname is a no-op; changing the hostname re-targets the same
// workload at a new HTTPRoute.
func Install(ctx context.Context, opts InstallOptions) error {
	if opts.KubectlContext == "" {
		return fmt.Errorf("example.Install: KubectlContext is required")
	}
	if opts.Hostname == "" {
		return fmt.Errorf("example.Install: Hostname is required (must match the default Gateway's wildcard listener)")
	}
	if opts.GatewayNamespace == "" {
		return fmt.Errorf("example.Install: GatewayNamespace is required")
	}
	if opts.GatewayName == "" {
		return fmt.Errorf("example.Install: GatewayName is required")
	}
	logger := opts.Logger
	if logger == nil {
		logger = zap.NewNop()
	}
	manifest := Manifest(opts)
	logger.Info("applying example workload",
		zap.String("namespace", Namespace),
		zap.String("hostname", opts.Hostname),
		zap.String("image", Image),
	)
	cmd := exec.CommandContext(ctx, "kubectl",
		"--context="+opts.KubectlContext,
		"apply",
		"--server-side", "--force-conflicts",
		"--field-manager=y-cluster",
		"-f", "-",
	)
	cmd.Stdin = bytes.NewReader(manifest)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("kubectl apply: %w", err)
	}
	return nil
}

// Uninstall removes the namespace and everything in it. Idempotent
// (kubectl delete --ignore-not-found).
func Uninstall(ctx context.Context, kubectlContext string, logger *zap.Logger) error {
	if kubectlContext == "" {
		return fmt.Errorf("example.Uninstall: kubectlContext is required")
	}
	if logger == nil {
		logger = zap.NewNop()
	}
	logger.Info("removing example workload",
		zap.String("namespace", Namespace),
	)
	cmd := exec.CommandContext(ctx, "kubectl",
		"--context="+kubectlContext,
		"delete", "namespace", Namespace,
		"--ignore-not-found",
	)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("kubectl delete namespace: %w", err)
	}
	return nil
}

// Manifest renders the example workload manifests. Pure function
// so unit tests can pin the rendered shape without running
// kubectl.
func Manifest(opts InstallOptions) []byte {
	r := strings.NewReplacer(
		"__NAMESPACE__", Namespace,
		"__NAME__", WorkloadName,
		"__IMAGE__", Image,
		"__GATEWAY_NS__", opts.GatewayNamespace,
		"__GATEWAY_NAME__", opts.GatewayName,
		"__HOSTNAME__", opts.Hostname,
		"__RESPONSE__", PublicResponse,
	)
	return []byte(r.Replace(rawManifest))
}

const rawManifest = `---
apiVersion: v1
kind: Namespace
metadata:
  name: __NAMESPACE__
  labels:
    managed-by: y-cluster
    y-cluster-example: ""
---
apiVersion: apps/v1
kind: Deployment
metadata:
  name: __NAME__
  namespace: __NAMESPACE__
  labels:
    managed-by: y-cluster
    app: __NAME__
spec:
  replicas: 1
  selector:
    matchLabels:
      app: __NAME__
  template:
    metadata:
      labels:
        app: __NAME__
        managed-by: y-cluster
    spec:
      automountServiceAccountToken: false
      securityContext:
        runAsNonRoot: true
        runAsUser: 65532
        runAsGroup: 65532
        seccompProfile:
          type: RuntimeDefault
      containers:
      - name: __NAME__
        image: __IMAGE__
        args:
        - "-listen=:8080"
        - "-text=__RESPONSE__"
        ports:
        - containerPort: 8080
          name: http
        readinessProbe:
          httpGet:
            path: /
            port: 8080
          initialDelaySeconds: 1
          periodSeconds: 5
        resources:
          requests:
            cpu: 10m
            memory: 16Mi
          limits:
            cpu: 100m
            memory: 32Mi
        securityContext:
          allowPrivilegeEscalation: false
          readOnlyRootFilesystem: true
          capabilities:
            drop: ["ALL"]
---
apiVersion: v1
kind: Service
metadata:
  name: __NAME__
  namespace: __NAMESPACE__
  labels:
    managed-by: y-cluster
spec:
  selector:
    app: __NAME__
  ports:
  - name: http
    port: 8080
    targetPort: 8080
---
apiVersion: gateway.networking.k8s.io/v1
kind: HTTPRoute
metadata:
  name: __NAME__
  namespace: __NAMESPACE__
  labels:
    managed-by: y-cluster
spec:
  parentRefs:
  - name: __GATEWAY_NAME__
    namespace: __GATEWAY_NS__
  hostnames:
  - "__HOSTNAME__"
  rules:
  - matches:
    - path:
        type: PathPrefix
        value: /
    backendRefs:
    - name: __NAME__
      port: 8080
`
