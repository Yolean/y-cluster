// Package docker provisions a k3s cluster inside a single
// privileged Docker container. Faster than the qemu provisioner
// (no VM boot, no cloud image download), runs anywhere Docker
// runs, and consumes the rancher/k3s mirror image y-cluster
// already pins via pkg/provision/config/k3s.yaml.
package docker

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/netip"
	"os"
	"os/exec"
	"strings"
	"time"

	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/api/types/network"
	"github.com/moby/moby/client"
	"go.uber.org/zap"

	"github.com/Yolean/y-cluster/pkg/dockerexec"
	"github.com/Yolean/y-cluster/pkg/kubeconfig"
	"github.com/Yolean/y-cluster/pkg/provision"
	"github.com/Yolean/y-cluster/pkg/provision/config"
	"github.com/Yolean/y-cluster/pkg/provision/envoygateway"
	"github.com/Yolean/y-cluster/pkg/provision/localstorage"
	"github.com/Yolean/y-cluster/pkg/provision/registries"
)

// Compile-time conformance check that the package's Cluster type
// satisfies the cross-provisioner interface.
var _ provision.Cluster = (*Cluster)(nil)

// Cluster is the runtime handle for a running docker
// container. Implements provision.Cluster.
type Cluster struct {
	cfg     config.DockerConfig
	logger  *zap.Logger
	kubecfg *kubeconfig.Manager
	cli     *client.Client
}

// CheckPrerequisites verifies that docker is reachable. It also
// warns about inotify limits that are known to break docker:
// k3s spawns a CNI watcher per node and hits the per-user instance
// cap on systems where the default is 128. CI runners (ubuntu-latest)
// have high enough limits; developer laptops sometimes don't.
//
// docker daemon reachability is now checked through the daemon
// API (Ping) rather than `docker info` so we get typed errors
// instead of "exit status 1": socket-not-found surfaces as
// net.OpError, version mismatch as a typed errdefs error.
func CheckPrerequisites() error {
	if _, err := exec.LookPath("docker"); err != nil {
		return fmt.Errorf("missing docker CLI: install Docker Engine or Docker Desktop")
	}
	cli, err := dockerexec.New()
	if err != nil {
		return fmt.Errorf("docker client: %w", err)
	}
	defer func() { _ = cli.Close() }()
	if _, err := cli.Ping(context.Background(), client.PingOptions{}); err != nil {
		return fmt.Errorf("docker daemon unreachable: %w", err)
	}
	if data, err := readFirstLine("/proc/sys/fs/inotify/max_user_instances"); err == nil {
		if n, err := atoi(data); err == nil && n < 256 {
			return fmt.Errorf(
				"fs.inotify.max_user_instances is %d; docker needs at least 256. "+
					"Run: sudo sysctl fs.inotify.max_user_instances=512", n,
			)
		}
	}
	return nil
}

// readFirstLine reads `path` and returns its first line trimmed.
// We used to shell out to `cat` here; the stdlib version
// surfaces typed errors (os.ErrNotExist when /proc/sys/fs/...
// doesn't exist on a non-Linux host, fs.PathError with
// permission detail) instead of just `exit status 1`.
func readFirstLine(path string) (string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(strings.SplitN(string(data), "\n", 2)[0]), nil
}

func atoi(s string) (int, error) {
	var n int
	for _, c := range s {
		if c < '0' || c > '9' {
			return 0, fmt.Errorf("not a number: %q", s)
		}
		n = n*10 + int(c-'0')
	}
	return n, nil
}

// Provision starts a docker container, waits for the
// apiserver to write its kubeconfig, extracts that kubeconfig and
// merges it into the host's KUBECONFIG under the configured
// context. Returns when the cluster is reachable from the host.
func Provision(ctx context.Context, cfg config.DockerConfig, logger *zap.Logger) (*Cluster, error) {
	if logger == nil {
		logger = zap.NewNop()
	}

	// Cross-provisioner preflight: host ports + kubeconfig context.
	// Container reuse via dockerexec.Remove is idempotent so we
	// don't preflight the container name; ports are the bit that
	// fails in a non-obvious way (the docker daemon error message
	// names the port but not what to change in y-cluster's config).
	pf := provision.Preflight{
		HostPorts:      dockerHostPorts(cfg),
		ContextName:    cfg.Context,
		ContextCluster: cfg.Name,
		KubeconfigPath: os.Getenv("KUBECONFIG"),
	}
	if err := pf.Run(); err != nil {
		return nil, err
	}

	kubecfg, err := kubeconfig.New(cfg.Context, cfg.Name, logger)
	if err != nil {
		return nil, err
	}

	if err := CheckPrerequisites(); err != nil {
		return nil, err
	}

	// Resolve the container image from the k3s version: prefer the
	// y-cluster mirror, fall back to upstream rancher/k3s when the
	// mirror has no manifest yet. The resolver logs a warning on
	// fallback so an unmirrored version doesn't go unnoticed.
	image, _, err := ResolveImage(ctx, cfg.K3s.Version, nil, logger)
	if err != nil {
		return nil, err
	}

	cli, err := dockerexec.New()
	if err != nil {
		return nil, fmt.Errorf("docker client: %w", err)
	}

	// If a previous container under this name still exists, remove it
	// so the new run can claim the port mappings cleanly. Idempotent
	// (NotFound is treated as success).
	if err := dockerexec.Remove(ctx, cli, cfg.Name); err != nil {
		_ = cli.Close()
		return nil, err
	}
	kubecfg.CleanupStale()

	hostConfig, exposedPorts, err := buildHostConfig(cfg)
	if err != nil {
		_ = cli.Close()
		return nil, err
	}

	logger.Info("starting docker",
		zap.String("image", image),
		zap.String("apiPort", cfg.HostAPIPort()),
		zap.String("memory", cfg.Memory),
		zap.String("cpus", cfg.CPUs),
	)
	// ContainerCreate doesn't auto-pull (unlike `docker run`), so a
	// fresh host -- CI runner, new developer machine -- would error
	// out with "No such image". Pull first when the image isn't
	// already on the daemon.
	if err := dockerexec.PullIfMissing(ctx, cli, image); err != nil {
		_ = cli.Close()
		return nil, err
	}
	createRes, err := cli.ContainerCreate(ctx, client.ContainerCreateOptions{
		Name: cfg.Name,
		// moby's client rejects the request when both top-level
		// Image and Config.Image are set; we use Config.Image so
		// we can set Cmd in the same struct.
		Config: &container.Config{
			Image: image,
			// --disable=traefik because y-cluster bundles Envoy
			// Gateway as the ingress controller; two of them
			// would fight over host:80/:443.
			// --disable=local-storage because y-cluster ships
			// its own local-path-provisioner via
			// pkg/provision/localstorage with the appliance-
			// shape defaults (path /data/yolean, PVC
			// namespace_name pattern, Retain reclaim).
			Cmd: []string{
				"server",
				"--tls-san=127.0.0.1",
				"--disable=traefik",
				"--disable=local-storage",
			},
			// ExposedPorts must list every guest port carried by
			// HostConfig.PortBindings. The Docker CLI auto-fills
			// this when you `-p`; the moby SDK does not. Engine
			// 28+ silently drops bindings that lack an
			// ExposedPorts entry in some request shapes (issue
			// #16: NetworkSettings.Ports == {} on ubuntu-latest
			// when invoked via the released binary from bash,
			// despite the in-process e2e succeeding on the same
			// runner).
			ExposedPorts: exposedPorts,
		},
		HostConfig: hostConfig,
	})
	if err != nil {
		_ = cli.Close()
		return nil, fmt.Errorf("create %s: %w", cfg.Name, err)
	}
	// Stage /etc/rancher/k3s/registries.yaml inside the container
	// before starting it so containerd reads the file on first
	// startup. Skipped when the config carries no registries
	// entries, in which case k3s falls back to its defaults.
	if err := writeRegistriesToContainer(ctx, cli, createRes.ID, cfg.Registries); err != nil {
		_ = cli.Close()
		return nil, fmt.Errorf("write registries: %w", err)
	}
	if _, err := cli.ContainerStart(ctx, createRes.ID, client.ContainerStartOptions{}); err != nil {
		_ = cli.Close()
		return nil, fmt.Errorf("start %s: %w", cfg.Name, err)
	}

	c := &Cluster{cfg: cfg, logger: logger, kubecfg: kubecfg, cli: cli}

	if err := c.waitForKubeconfig(ctx); err != nil {
		// Capture container logs for diagnosis before returning.
		dlogs, _ := dockerexec.Logs(ctx, cli, cfg.Name, "100")
		return nil, fmt.Errorf("wait for k3s: %w\ncontainer logs:\n%s", err, dlogs)
	}

	rawKubeconfig, err := c.extractKubeconfig(ctx)
	if err != nil {
		return nil, fmt.Errorf("extract kubeconfig: %w", err)
	}
	if err := kubecfg.Import(rawKubeconfig); err != nil {
		return nil, fmt.Errorf("merge kubeconfig: %w", err)
	}

	// k3s.yaml landing in the container doesn't mean the host can
	// reach the apiserver yet -- the docker port-forward to the
	// host-mapped 6443 is bound a moment later, and the very next
	// step (envoygateway.Install -> kubectl apply --server-side)
	// would otherwise race against it with "dial tcp 127.0.0.1:
	// 6443: connect: connection refused". Probe the host endpoint
	// through the merged kubeconfig until /readyz succeeds before
	// declaring readiness.
	if err := c.waitForHostAPIServer(ctx); err != nil {
		dlogs, _ := dockerexec.Logs(ctx, cli, cfg.Name, "100")
		return nil, fmt.Errorf("wait for host apiserver: %w\ncontainer logs:\n%s", err, dlogs)
	}

	logger.Info("k3s ready", zap.String("context", cfg.Context))

	// Install the bundled local-path-provisioner (replaces k3s's
	// disabled local-storage addon) before any workload install
	// so the StorageClass exists when consumer PVCs land.
	if err := localstorage.Install(ctx, localstorage.Options{
		ContextName:   cfg.Context,
		Path:          cfg.Storage.Path,
		Pattern:       cfg.Storage.PathPattern,
		ReclaimPolicy: cfg.Storage.ReclaimPolicy,
		Logger:        logger,
	}); err != nil {
		return nil, fmt.Errorf("install local-path-provisioner: %w", err)
	}

	// Install the bundled Envoy Gateway (CRDs + controller +
	// default GatewayClass). Replaces Traefik, which we disabled
	// in the k3s server cmd above. Skipped wholesale when
	// gateway.skip is set in cluster config.
	if cfg.Gateway.Skip {
		logger.Info("envoy gateway install skipped (gateway.skip)")
	} else {
		if err := envoygateway.Install(ctx, envoygateway.Options{
			ContextName:      cfg.Context,
			GatewayClassName: cfg.Gateway.ClassName,
			DNSHintIP:        cfg.HostRoutableIP(),
			Logger:           logger,
		}); err != nil {
			return nil, fmt.Errorf("install envoy gateway: %w", err)
		}
		logger.Info("envoy gateway ready",
			zap.String("version", envoygateway.Version),
			zap.String("gatewayClass", cfg.Gateway.ClassName),
		)
	}

	return c, nil
}

// writeRegistriesToContainer stages the configured registries.yaml
// in the container's filesystem before it boots. Runs after
// ContainerCreate but before ContainerStart so containerd reads
// the file when k3s starts. Empty registries config is a no-op.
//
// CopyToContainer doesn't auto-mkdir, so the tar produced by
// registries.Tar carries explicit dir entries for /etc/rancher/k3s/.
func writeRegistriesToContainer(ctx context.Context, cli *client.Client, containerID string, r config.Registries) error {
	body, err := registries.Marshal(r)
	if err != nil {
		return err
	}
	if body == nil {
		return nil
	}
	archive, err := registries.Tar(body)
	if err != nil {
		return err
	}
	if _, err := cli.CopyToContainer(ctx, containerID, client.CopyToContainerOptions{
		DestinationPath: "/",
		Content:         bytes.NewReader(archive),
	}); err != nil {
		return fmt.Errorf("copy %s into container: %w", registries.Path, err)
	}
	return nil
}

// dockerHostPorts gathers every host port docker.Provision will
// bind. Empty Host entries are skipped (docker daemon picks a
// free port); the preflight only checks what the user pinned.
func dockerHostPorts(cfg config.DockerConfig) []string {
	ports := make([]string, 0, len(cfg.PortForwards))
	for _, pf := range cfg.PortForwards {
		if pf.Host != "" {
			ports = append(ports, pf.Host)
		}
	}
	return ports
}

// buildHostConfig translates the YAML-shaped DockerConfig into
// the moby HostConfig + the matching Config.ExposedPorts set.
// Both are required for the daemon to publish bindings:
//
//   - HostConfig.PortBindings tells the daemon "publish these
//     guest ports on these host ports."
//   - Config.ExposedPorts declares the same guest ports as the
//     container's exposed surface. The Docker CLI's `docker run
//     -p ...` auto-fills both; the SDK's ContainerCreate does
//     NOT and Engine 28+ silently drops bindings when ExposedPorts
//     is missing in some request shapes -- the container starts
//     but NetworkSettings.Ports comes back as `{}` and the host
//     never sees a forward (issue #16).
//
// PortBindings come straight from cfg.PortForwards, which is
// where the API port (6443) and any ingress ports (80/443/...)
// are declared. Validation in CommonConfig guarantees a 6443
// entry exists, so the host's kubectl can reach the API server.
func buildHostConfig(cfg config.DockerConfig) (*container.HostConfig, network.PortSet, error) {
	bindings := network.PortMap{}
	exposed := network.PortSet{}
	for _, pf := range cfg.PortForwards {
		guest, err := network.ParsePort(pf.Guest)
		if err != nil {
			return nil, nil, fmt.Errorf("parse guest port %q: %w", pf.Guest, err)
		}
		// HostIP must be set explicitly to a valid netip.Addr.
		// The zero value is `invalid IP`, which moby v1.54+
		// renders as an empty JSON string and the Docker Engine
		// daemon (28.x) silently drops the binding from
		// NetworkSettings.Ports. Mirroring `docker run -p ...`
		// semantics, IPv4Unspecified ("0.0.0.0") binds on every
		// interface. HostPort empty lets docker pick a free port.
		bindings[guest] = append(bindings[guest], network.PortBinding{
			HostIP:   netip.IPv4Unspecified(),
			HostPort: pf.Host,
		})
		// Same guest port lands in ExposedPorts; the daemon
		// requires the pair for `docker run -p`-equivalent
		// publish semantics on Engine 28+.
		exposed[guest] = struct{}{}
	}
	hc := &container.HostConfig{
		Privileged: true,
		Tmpfs: map[string]string{
			"/run":     "",
			"/var/run": "",
		},
		PortBindings: bindings,
	}
	if cfg.Memory != "" {
		mb, err := atoi(cfg.Memory)
		if err != nil {
			return nil, nil, fmt.Errorf("parse memory %q: %w", cfg.Memory, err)
		}
		hc.Memory = int64(mb) * 1024 * 1024
	}
	if cfg.CPUs != "" {
		// Accept whole-CPU values; --cpus 1.5 isn't required for our
		// use-case and would need float parsing.
		n, err := atoi(cfg.CPUs)
		if err != nil {
			return nil, nil, fmt.Errorf("parse cpus %q: %w", cfg.CPUs, err)
		}
		hc.NanoCPUs = int64(n) * 1_000_000_000
	}
	return hc, exposed, nil
}

// Stop gracefully shuts down the docker container. The Docker
// daemon sends SIGTERM to PID 1 (k3s) and waits up to 60s for
// the guest to exit before escalating to SIGKILL. 60s vs the
// CLI default of 10s because k3s + containerd's overlayfs
// snapshot writes can take longer to flush than the default
// allows; on a faster timeout we'd see the same "exec format
// error" crash loops on next start that we hit on qemu when
// SIGTERM exited the qemu process in 200ms.
//
// Container is preserved for a follow-up `docker container
// start` (the y-cluster equivalent isn't wired yet for the
// docker backend; this is the lower half of that lifecycle).
//
// NotFound is treated as success.
func Stop(ctx context.Context, name string, logger *zap.Logger) error {
	if logger == nil {
		logger = zap.NewNop()
	}
	cli, err := dockerexec.New()
	if err != nil {
		return fmt.Errorf("docker daemon: %w", err)
	}
	defer func() { _ = cli.Close() }()
	logger.Info("stopping docker container", zap.String("name", name))
	return dockerexec.Stop(ctx, cli, name, dockerStopTimeoutSecs)
}

// dockerStopTimeoutSecs is the SIGTERM-to-SIGKILL grace the
// Stop function passes to the Docker daemon. Tunable via the
// var so tests can shorten it.
var dockerStopTimeoutSecs = 60

// Teardown removes the container. keepDisk is ignored -- k3s state
// lives entirely inside the container, so there is no persistent
// disk to keep across teardowns.
func (c *Cluster) Teardown(keepDisk bool) error {
	_ = keepDisk // ignored
	c.logger.Info("removing docker container", zap.String("name", c.cfg.Name))
	if err := dockerexec.Remove(context.Background(), c.cli, c.cfg.Name); err != nil {
		return err
	}
	if c.kubecfg != nil {
		c.kubecfg.CleanupTeardown()
	}
	return nil
}

// Context implements provision.Cluster.
func (c *Cluster) Context() string { return c.cfg.Context }

// NodeExec implements provision.Cluster. Runs a shell command
// inside the container via docker exec. stdin (when non-nil) is
// piped through so callers can stream OCI tarballs into
// `ctr image import`.
func (c *Cluster) NodeExec(ctx context.Context, command string, stdin io.Reader) ([]byte, error) {
	var buf bytes.Buffer
	if err := dockerexec.Exec(ctx, c.cli, c.cfg.Name, []string{"sh", "-c", command}, stdin, &buf, &buf); err != nil {
		return buf.Bytes(), err
	}
	return buf.Bytes(), nil
}

// waitForKubeconfig polls until the k3s-managed kubeconfig appears
// inside the container. k3s writes /etc/rancher/k3s/k3s.yaml when
// the in-container apiserver socket is bound; the host-side port
// forward and full apiserver readiness lag behind, so the host
// must additionally probe via waitForHostAPIServer before any
// kubectl call against the merged kubeconfig.
func (c *Cluster) waitForKubeconfig(ctx context.Context) error {
	deadline := time.Now().Add(2 * time.Minute)
	for {
		out, err := c.NodeExec(ctx, "test -s /etc/rancher/k3s/k3s.yaml && echo ok", nil)
		if err == nil && strings.Contains(string(out), "ok") {
			return nil
		}
		if time.Now().After(deadline) {
			return errors.New("/etc/rancher/k3s/k3s.yaml never appeared within 2 minutes")
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(2 * time.Second):
		}
	}
}

const (
	hostAPIServerReadyTimeout  = 60 * time.Second
	hostAPIServerReadyInterval = time.Second
)

// waitForHostAPIServer polls `kubectl get --raw=/readyz` against
// the merged context until the apiserver responds with a 200. Two
// host-side concerns lag behind kubeconfig-in-container readiness:
// docker's userland port forward to 127.0.0.1:HostAPIPort needs a
// moment to bind, and the apiserver itself takes a beat to advance
// from "listening" to "ready". /readyz covers both -- a connection
// refused, a 503 from a still-starting apiserver, or a transport
// error are all retried.
//
// We shell out to kubectl rather than dialing the apiserver
// directly because envoygateway.Install drives the host kubeconfig
// the same way -- using kubectl here keeps the readiness probe on
// the same code path that the very next caller will use.
func (c *Cluster) waitForHostAPIServer(ctx context.Context) error {
	return c.pollHostAPIServerReadyz(ctx, hostAPIServerReadyTimeout, hostAPIServerReadyInterval)
}

// pollHostAPIServerReadyz is the parameterised body of
// waitForHostAPIServer; the timeout and interval are arguments so
// tests can drive the loop with a fake kubectl on $PATH at sub-second
// resolution.
func (c *Cluster) pollHostAPIServerReadyz(ctx context.Context, timeout, interval time.Duration) error {
	c.logger.Info("waiting for host apiserver", zap.String("context", c.cfg.Context))
	deadline := time.Now().Add(timeout)
	var lastErr error
	for {
		probe := exec.CommandContext(ctx, "kubectl",
			"--context="+c.cfg.Context,
			"get", "--raw=/readyz",
		)
		// Discard noisy intermediate failures; surface only the
		// final state via lastErr if we time out.
		out, err := probe.CombinedOutput()
		if err == nil {
			return nil
		}
		lastErr = fmt.Errorf("%w: %s", err, strings.TrimSpace(string(out)))
		if time.Now().After(deadline) {
			return fmt.Errorf("apiserver /readyz never returned 200 within %s on context %q: %v", timeout, c.cfg.Context, lastErr)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(interval):
		}
	}
}

// extractKubeconfig reads the container's kubeconfig and rewrites
// the embedded server URL to the host-mapped API port so the host's
// kubectl can reach it.
func (c *Cluster) extractKubeconfig(ctx context.Context) ([]byte, error) {
	out, err := c.NodeExec(ctx, "cat /etc/rancher/k3s/k3s.yaml", nil)
	if err != nil {
		return nil, fmt.Errorf("read kubeconfig: %s: %w", out, err)
	}
	return bytes.ReplaceAll(out, []byte("127.0.0.1:6443"), []byte("127.0.0.1:"+c.cfg.HostAPIPort())), nil
}

// ContainerName returns the docker container name. Test helpers use
// this to docker-exec into a known target.
func (c *Cluster) ContainerName() string { return c.cfg.Name }
