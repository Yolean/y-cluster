// Package echo deploys the standard appliance-test workload: a
// Gateway listening on :80, an envoy-echo Deployment, a Service,
// and an HTTPRoute matching /q/envoy/echo on any hostname.
//
// The point is a self-contained workload the appliance e2e can
// curl through Envoy Gateway after a provision (and, eventually,
// after a stop/start or import round-trip) to prove the cluster
// still serves traffic. The response body dumps every request
// header plus pod info, so tests can assert routing AND identity
// in one curl.
//
// Sibling to pkg/provision/envoygateway: that package owns the
// Gateway controller install at provision time; this one owns
// the test workload that lands on top of it. Both shell out to
// kubectl with --server-side --field-manager=y-cluster so
// re-applies converge.
package echo

import (
	"bytes"
	"context"
	_ "embed"
	"fmt"
	"os"
	"os/exec"
	"text/template"
)

// DefaultImage is the Yolean envoy echo image: distroless envoy
// plus a Rust dynamic-modules .so that intercepts requests at
// `/q/envoy/echo` and replies with a JSON dump of headers, request
// info, and pod identity. The bundled /etc/envoy/envoy.yaml wires
// the dynamic-modules filter in front of the router, so the
// container is a drop-in upstream service for any HTTPRoute.
//
// Replaces the older registry.k8s.io/echoserver:1.10 (nginx-lua
// behind a self-signed wrapper) -- the envoy variant matches what
// production traffic flows through (real Envoy data plane), the
// response is structured JSON instead of plain-text, and the
// bundled config requires no init scaffolding (no /var/lib/nginx
// emptyDir, no /run shim, no /certs writability).
//
// Pinned by digest so a re-tag of echo-v1.38.0 upstream can't
// change the bits we ship inside the appliance. The tag stays in
// the ref for human readability of `kubectl get pod -o yaml`.
// Multi-arch (linux/amd64 + linux/arm64); pulling on either host
// arch resolves to the right manifest.
const DefaultImage = "ghcr.io/yolean/envoy:echo-v1.38.0@sha256:e86a32467f2583e3c57b76e69535fc36fb0ca3af524f4c43d19b193b70f9dd60"

// DefaultNamespace is what `y-cluster echo deploy` defaults to
// when -n is omitted. Matches the GatewayClass name convention.
const DefaultNamespace = "y-cluster"

// DefaultGatewayClass is the GatewayClass name the bundled Envoy
// Gateway install creates (pkg/provision/config/common.go's
// GatewayConfig.ClassName). Override on Options when the cluster
// pinned a different class via gateway.className.
const DefaultGatewayClass = "y-cluster"

// PathPrefix is the HTTPRoute path the standard workload responds
// to. Tracks the bundled envoy.yaml in the upstream
// ghcr.io/yolean/envoy:echo-* image, which intercepts at
// /q/envoy/echo by default. Override on the HTTPRoute side AND
// the filter's path_prefix in a custom envoy.yaml if you need a
// different prefix; both must agree.
const PathPrefix = "/q/envoy/echo"

//go:embed template.yaml
var manifestTemplate string

// Options controls Deploy. ContextName is required; everything
// else has a documented default.
type Options struct {
	ContextName  string // required: kubectl context to apply against
	Namespace    string // empty -> DefaultNamespace
	GatewayClass string // empty -> DefaultGatewayClass
	Image        string // empty -> DefaultImage
}

// Deploy applies the standard echo workload. Idempotent (server-
// side apply with the y-cluster field manager) so re-running
// converges -- safe to call from a Provision that already ran it.
//
// What lands:
//
//   - Namespace (default y-cluster)
//   - Gateway "y-cluster" listening :80, attached to the configured
//     GatewayClass, accepting routes from any namespace and any
//     hostname.
//   - Deployment "echo" with downward-API env vars so the response
//     body's "Pod Information" section is populated.
//   - Service "echo" (ClusterIP) :80 -> :8080.
//   - HTTPRoute "echo" matching PathPrefix /q/envoy/echo on
//     any hostname.
//
// Probe (after rollout): curl http://<host-port>/q/envoy/echo
// where <host-port> is whatever port the provisioner forwarded
// to guest 80.
func Deploy(ctx context.Context, opts Options) error {
	if opts.ContextName == "" {
		return fmt.Errorf("echo.Deploy: ContextName is required")
	}
	if opts.Namespace == "" {
		opts.Namespace = DefaultNamespace
	}
	if opts.GatewayClass == "" {
		opts.GatewayClass = DefaultGatewayClass
	}
	if opts.Image == "" {
		opts.Image = DefaultImage
	}
	manifest, err := Render(opts)
	if err != nil {
		return err
	}
	return kubectlApplyServerSide(ctx, opts.ContextName, manifest)
}

// Render produces the YAML manifest Deploy would apply, without
// touching a cluster. Pure function -- tests pin the rendered
// shape against expected substrings, and a future `y-cluster echo
// manifest` subcommand could surface this for inspection.
func Render(opts Options) ([]byte, error) {
	if opts.Namespace == "" {
		opts.Namespace = DefaultNamespace
	}
	if opts.GatewayClass == "" {
		opts.GatewayClass = DefaultGatewayClass
	}
	if opts.Image == "" {
		opts.Image = DefaultImage
	}
	tpl, err := template.New("echo").Parse(manifestTemplate)
	if err != nil {
		return nil, fmt.Errorf("parse echo template: %w", err)
	}
	var buf bytes.Buffer
	if err := tpl.Execute(&buf, opts); err != nil {
		return nil, fmt.Errorf("render echo manifest: %w", err)
	}
	return buf.Bytes(), nil
}

// kubectlApplyServerSide pipes manifest through `kubectl apply
// --server-side --force-conflicts --field-manager=y-cluster -f -`.
// Field manager matches what pkg/yconverge and pkg/provision/
// envoygateway use, so re-applies under any path don't fight
// over field ownership.
func kubectlApplyServerSide(ctx context.Context, contextName string, manifest []byte) error {
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
		return fmt.Errorf("kubectl apply: %w", err)
	}
	return nil
}
