// Package localstorage installs y-cluster's bundled
// local-path-provisioner. It replaces k3s's stock local-storage
// addon (provisioners pass --disable=local-storage to the k3s
// install) so y-cluster owns the StorageClass + ConfigMap +
// Deployment and can apply the appliance-shape defaults from
// CommonConfig.Storage:
//
//   - Path (default /data/yolean): nodePathMap default, where
//     PV directories land on the node.
//   - PathPattern (default {{ .PVC.Namespace }}_{{ .PVC.Name }}):
//     the per-PV subdirectory naming template, predictable
//     enough that an appliance upgrade rebinds to the same
//     directory by namespace+name alone.
//   - ReclaimPolicy (default Retain): on the StorageClass, so
//     a stray `kubectl delete pvc` doesn't wipe customer data.
//
// Sibling to pkg/provision/envoygateway: that package owns the
// Gateway controller; this one owns the StorageClass. Both use
// the same kubectl-shellout-with-server-side-apply shape.
package localstorage

import (
	"bytes"
	"context"
	_ "embed"
	"fmt"
	"os"
	"os/exec"
	"text/template"
	"time"

	"go.uber.org/zap"
)

// Namespace is where the local-path-provisioner Deployment runs.
const Namespace = "local-path-storage"

// DeploymentName is what Install waits on with `kubectl rollout
// status` after applying the manifest.
const DeploymentName = "local-path-provisioner"

// StorageClassName is the y-cluster default StorageClass. Named
// "local-path" to match the upstream / k3s convention so consumer
// PVCs that hardcode the name (or rely on the storageclass.kubernetes.io/
// is-default-class annotation) keep working without changes.
const StorageClassName = "local-path"

// DefaultReadyTimeout caps how long Install waits for the
// provisioner Deployment to roll out. 90s is generous for a
// fresh image pull on a slow cluster (the image is ~20 MiB).
const DefaultReadyTimeout = 90 * time.Second

//go:embed install.yaml
var manifestTemplate string

// Options controls Install. Path / Pattern / ReclaimPolicy come
// from CommonConfig.Storage; ContextName is the kubeconfig
// context to apply against.
type Options struct {
	ContextName   string
	Path          string
	Pattern       string
	ReclaimPolicy string
	Logger        *zap.Logger
	// ReadyTimeout overrides DefaultReadyTimeout for the wait
	// step. A negative value skips the wait entirely (used by
	// kwok-based tests where the controller never actually
	// rolls out).
	ReadyTimeout time.Duration
}

// Render produces the manifest YAML Install would apply, without
// touching a cluster. Pure function -- tests pin the rendered
// shape against expected substrings.
func Render(opts Options) ([]byte, error) {
	if opts.Path == "" {
		return nil, fmt.Errorf("localstorage.Render: Path is required")
	}
	if opts.Pattern == "" {
		return nil, fmt.Errorf("localstorage.Render: Pattern is required")
	}
	if opts.ReclaimPolicy == "" {
		return nil, fmt.Errorf("localstorage.Render: ReclaimPolicy is required")
	}
	tpl, err := template.New("localstorage").Parse(manifestTemplate)
	if err != nil {
		return nil, fmt.Errorf("parse install.yaml template: %w", err)
	}
	var buf bytes.Buffer
	if err := tpl.Execute(&buf, opts); err != nil {
		return nil, fmt.Errorf("render install.yaml: %w", err)
	}
	return buf.Bytes(), nil
}

// Install renders the manifest and applies it to the cluster
// pointed at by opts.ContextName, then waits for the provisioner
// Deployment to roll out.
//
// Idempotent: re-running on a cluster that already has the
// y-cluster local-path install reconciles via SSA. Field manager
// `y-cluster` matches what envoygateway and yconverge use, so
// re-applies under any path don't fight over field ownership.
func Install(ctx context.Context, opts Options) error {
	if opts.ContextName == "" {
		return fmt.Errorf("localstorage.Install: ContextName is required")
	}
	logger := opts.Logger
	if logger == nil {
		logger = zap.NewNop()
	}
	manifest, err := Render(opts)
	if err != nil {
		return err
	}

	logger.Info("applying local-path-provisioner",
		zap.String("path", opts.Path),
		zap.String("pattern", opts.Pattern),
		zap.String("reclaimPolicy", opts.ReclaimPolicy),
	)
	if err := kubectlApplyStdin(ctx, opts.ContextName, manifest); err != nil {
		return fmt.Errorf("apply local-path manifest: %w", err)
	}

	if opts.ReadyTimeout >= 0 {
		timeout := opts.ReadyTimeout
		if timeout == 0 {
			timeout = DefaultReadyTimeout
		}
		logger.Info("waiting for local-path-provisioner rollout",
			zap.String("namespace", Namespace),
			zap.String("deployment", DeploymentName),
			zap.Duration("timeout", timeout),
		)
		if err := kubectlRolloutStatus(ctx, opts.ContextName, DeploymentName, Namespace, timeout); err != nil {
			return fmt.Errorf("wait for %s/%s rollout: %w", Namespace, DeploymentName, err)
		}
	}
	return nil
}

// kubectlApplyStdin pipes the manifest into `kubectl apply
// --server-side --force-conflicts --field-manager=y-cluster`.
// Mirrors envoygateway's helper of the same name.
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
// -n <ns> --timeout=<timeout>`.
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
