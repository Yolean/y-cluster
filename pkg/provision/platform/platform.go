// Package platform installs what y-cluster runs on a k3s cluster
// whose server was started for Cilium (k3s.CiliumServerSettings):
// Cilium as the CNI, the bundled local-path-provisioner, Envoy Gateway
// with merged Gateways, and cert-manager with y-cluster's CA issuer.
// Everything goes through a kubeconfig context, so it works the same
// on a cluster y-cluster provisioned (qemu calls Install) and on a k3s
// host it did not create (`y-cluster platform install`).
package platform

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"time"

	"go.uber.org/zap"

	"github.com/Yolean/y-cluster/pkg/provision/certmanager"
	"github.com/Yolean/y-cluster/pkg/provision/cilium"
	"github.com/Yolean/y-cluster/pkg/provision/config"
	"github.com/Yolean/y-cluster/pkg/provision/envoygateway"
	"github.com/Yolean/y-cluster/pkg/provision/localstorage"
)

// CiliumReadyTimeout caps the Cilium rollout and the nodes turning
// Ready after it; the first install pulls Cilium's images.
const CiliumReadyTimeout = 5 * time.Minute

// Options for Install and Check. Storage, Gateway and CertManager are
// the defaulted config sections of the cluster's
// y-cluster-provision.yaml.
type Options struct {
	Context string
	// Nodes is how many nodes must be Ready once Cilium is up; 0
	// means 1.
	Nodes       int
	Storage     config.StorageConfig
	Gateway     config.GatewayConfig
	CertManager config.CertManagerConfig
	// DNSHintIP is published on the GatewayClass (see
	// envoygateway.DNSHintIPAnnotation); empty publishes none.
	DNSHintIP string
	Logger    *zap.Logger
}

func (o Options) logger() *zap.Logger {
	if o.Logger == nil {
		return zap.NewNop()
	}
	return o.Logger
}

func (o Options) localStorage() localstorage.Options {
	return localstorage.Options{
		ContextName:   o.Context,
		Path:          o.Storage.Path,
		Pattern:       o.Storage.PathPattern,
		ReclaimPolicy: o.Storage.ReclaimPolicy,
		Logger:        o.logger(),
	}
}

func (o Options) envoyGateway() envoygateway.Options {
	return envoygateway.Options{
		ContextName:          o.Context,
		GatewayClassName:     o.Gateway.ClassName,
		DNSHintIP:            o.DNSHintIP,
		Logger:               o.logger(),
		ControllerCPURequest: o.Gateway.Resources.Controller.CPU,
		ControllerMemRequest: o.Gateway.Resources.Controller.Memory,
		ProxyCPURequest:      o.Gateway.Resources.Proxy.CPU,
		ProxyMemRequest:      o.Gateway.Resources.Proxy.Memory,
		// One node, ServiceLB: Gateways beyond y-cluster's own
		// (ystack's, a site's) must share the proxy on :80/:443.
		MergeGateways: true,
	}
}

func (o Options) certManager() certmanager.Options {
	return certmanager.Options{ContextName: o.Context, Logger: o.logger()}
}

// Install applies the platform and waits for each part as provision
// does: Cilium first, since nothing schedules on a node without a CNI,
// then local-path storage, so its StorageClass exists before any
// workload asks for a volume, then Envoy Gateway and cert-manager
// unless the config skips them. Every step is a server-side apply of
// rendered manifests, so a second run on an installed cluster changes
// nothing.
func Install(ctx context.Context, opts Options) error {
	if opts.Context == "" {
		return errors.New("platform: a kubeconfig context is required")
	}
	logger := opts.logger()
	nodes := opts.Nodes
	if nodes == 0 {
		nodes = 1
	}
	if err := cilium.Install(ctx, cilium.K3s, opts.Context, nodes, CiliumReadyTimeout, logger); err != nil {
		return fmt.Errorf("install cilium: %w", err)
	}
	if err := localstorage.Install(ctx, opts.localStorage()); err != nil {
		return fmt.Errorf("install local-path-provisioner: %w", err)
	}
	if opts.Gateway.Skip {
		logger.Info("envoy gateway install skipped (gateway.skip)")
	} else {
		if err := envoygateway.Install(ctx, opts.envoyGateway()); err != nil {
			return fmt.Errorf("install envoy gateway: %w", err)
		}
		logger.Info("envoy gateway ready",
			zap.String("version", envoygateway.Version),
			zap.String("gatewayClass", opts.Gateway.ClassName),
		)
	}
	if opts.CertManager.Skip {
		logger.Info("cert-manager install skipped (certManager.skip)")
	} else {
		if err := certmanager.Install(ctx, opts.certManager()); err != nil {
			return fmt.Errorf("install cert-manager: %w", err)
		}
		logger.Info("cert-manager ready", zap.String("version", certmanager.Version), zap.String("issuer", certmanager.CAIssuer))
	}
	return nil
}

// Manifest is one server-side apply that Install makes.
type Manifest struct {
	// Name says what the manifest is, e.g. "cilium".
	Name string
	Body []byte
}

// Manifests renders, in order, every manifest Install applies to the
// cluster behind opts.Context. Rendering reads the cluster but never
// changes it: Envoy Gateway's install depends on the release already
// running (envoygateway.RenderInstall).
func Manifests(ctx context.Context, opts Options) ([]Manifest, error) {
	if opts.Context == "" {
		return nil, errors.New("platform: a kubeconfig context is required")
	}
	ciliumManifest, err := cilium.Manifest(cilium.K3s)
	if err != nil {
		return nil, err
	}
	storage, err := localstorage.Render(opts.localStorage())
	if err != nil {
		return nil, err
	}
	out := []Manifest{
		{"cilium " + cilium.Version, ciliumManifest},
		{"local-path-provisioner", storage},
	}
	if !opts.Gateway.Skip {
		r, err := envoygateway.Render(ctx, opts.envoyGateway())
		if err != nil {
			return nil, err
		}
		out = append(out, Manifest{"envoy-gateway " + r.Version, r.Install})
		if r.EnvoyProxy != nil {
			out = append(out, Manifest{"envoy-gateway EnvoyProxy", r.EnvoyProxy})
		}
		if r.GatewayClass != nil {
			out = append(out, Manifest{"envoy-gateway GatewayClass", r.GatewayClass})
		}
	}
	if !opts.CertManager.Skip {
		path, err := certmanager.Ensure(ctx, certmanager.EnsureOptions{Logger: opts.Logger})
		if err != nil {
			return nil, err
		}
		release, err := os.ReadFile(path)
		if err != nil {
			return nil, err
		}
		out = append(out,
			Manifest{"cert-manager " + certmanager.Version, release},
			Manifest{"cert-manager issuers", certmanager.IssuersYAML()},
		)
	}
	return out, nil
}

// Check reports what Install would change, without changing anything:
// each manifest goes through `kubectl diff --server-side` with
// Install's field manager and conflict handling. It writes every diff
// to w and returns the names of the manifests that differ. An empty
// result means a converged cluster.
func Check(ctx context.Context, opts Options, w io.Writer) ([]string, error) {
	manifests, err := Manifests(ctx, opts)
	if err != nil {
		return nil, err
	}
	var drift []string
	for _, m := range manifests {
		diff, changed, err := kubectlDiff(ctx, opts.Context, m.Body)
		if err != nil {
			return drift, fmt.Errorf("check %s: %w", m.Name, err)
		}
		if changed {
			drift = append(drift, m.Name)
			fmt.Fprintf(w, "# %s: would change\n%s", m.Name, diff)
		} else {
			fmt.Fprintf(w, "# %s: no change\n", m.Name)
		}
	}
	return drift, nil
}

// kubectlDiff runs a server-side dry-run apply of manifest and
// returns the diff. kubectl diff exits 1 when there is one and above
// 1 when it failed.
func kubectlDiff(ctx context.Context, contextName string, manifest []byte) (string, bool, error) {
	cmd := exec.CommandContext(ctx, "kubectl", "--context="+contextName,
		"diff", "--server-side", "--force-conflicts", "--field-manager=y-cluster", "-f", "-")
	cmd.Stdin = bytes.NewReader(manifest)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	err := cmd.Run()
	var exitErr *exec.ExitError
	switch {
	case err == nil:
		return "", false, nil
	case errors.As(err, &exitErr) && exitErr.ExitCode() == 1:
		return stdout.String(), true, nil
	default:
		return "", false, fmt.Errorf("kubectl diff: %s: %w", bytes.TrimSpace(stderr.Bytes()), err)
	}
}
