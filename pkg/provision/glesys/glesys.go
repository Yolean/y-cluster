// Package glesys provisions a single-node Talos Linux cluster on a
// GleSYS KVM server. cmd/y-cluster dispatches to its package-level
// Provision / Teardown / Stop / Start; like hetzner it does not
// implement provision.Cluster, because its verbs address a cluster
// by context name through the GleSYS API rather than through an
// in-memory handle.
//
// What a provision leaves behind, and Teardown reverses:
//
//   - a reserved public IPv4, taken before the server exists so the
//     Talos cluster endpoint in the machine config can name it;
//   - a KVM server named after the context, booted from the Talos
//     template, with the generated control-plane machine config
//     delivered as the server's cloudconfig;
//   - a bootstrapped single-node cluster, its admin kubeconfig
//     merged into the operator's kubeconfig under the context;
//   - Envoy Gateway on the node's public address, ports 80 and 443,
//     with a default Gateway that terminates HTTPS with a
//     self-signed certificate for *.<context>.local.test;
//   - a talosconfig and a state sidecar (<context>.json) under
//     CacheDir, so Teardown works without the YAML config in hand
//     and the operator has what talosctl needs.
//
// No storage class is installed; k3s's local-path-provisioner has
// no Talos counterpart here yet.
package glesys

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	glesysapi "github.com/glesys/glesys-go/v8"
	"go.uber.org/zap"

	"github.com/Yolean/y-cluster/pkg/kubeconfig"
	"github.com/Yolean/y-cluster/pkg/provision/config"
	"github.com/Yolean/y-cluster/pkg/provision/envoygateway"
)

// CacheDirEnv lets tests / multi-tenant CI override the on-disk
// cache root without writing to the operator's home dir.
const CacheDirEnv = "Y_CLUSTER_GLESYS_CACHE_DIR"

// The credentials the operator places via .env or shell. GleSYS
// authenticates with HTTP basic auth: the project id (cl12345) is
// the username and the API key the password. The key is created
// per project in the GleSYS Cloud panel, and it has to list the
// operator's public address as an allowed host.
const (
	ProjectEnv = "GLESYS_PROJECT"
	APIKeyEnv  = "GLESYS_API_KEY"
)

// envHint names the file the operator's credentials are expected to
// live in, mirroring the hetzner provisioner's convention.
const envHint = "source ~/Yolean/.yolean-bots-device/y-cluster-glesys.env (or wherever your credentials live) before running this command"

// userAgent is what the GleSYS API logs for our calls.
const userAgent = "y-cluster"

// bandwidthMbit is the server's network cap. Not a config field:
// GleSYS bills it per Mbit, and 100 is the platform default.
const bandwidthMbit = 100

// Timeouts, in the order provision waits for things.
const (
	serverRunningTimeout = 5 * time.Minute
	talosAPITimeout      = 10 * time.Minute
	kubeconfigTimeout    = 10 * time.Minute
	kubeAPIReadyTimeout  = 10 * time.Minute
	bootstrapTimeout     = 5 * time.Minute
)

// CacheDir resolves the on-disk cache root. Order: env override,
// then ~/.cache/y-cluster-glesys.
func CacheDir() string {
	if v := os.Getenv(CacheDirEnv); v != "" {
		return v
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return filepath.Join(os.TempDir(), "y-cluster-glesys")
	}
	return filepath.Join(home, ".cache", "y-cluster-glesys")
}

// newClient builds a GleSYS client from the environment. Returns a
// clear error naming the variable that is unset.
func newClient() (*glesysapi.Client, error) {
	project := os.Getenv(ProjectEnv)
	key := os.Getenv(APIKeyEnv)
	switch {
	case project == "":
		return nil, fmt.Errorf("%s is unset; %s", ProjectEnv, envHint)
	case key == "":
		return nil, fmt.Errorf("%s is unset; %s", APIKeyEnv, envHint)
	}
	return glesysapi.NewClient(project, key, userAgent), nil
}

// Cluster is the running-state handle Provision returns.
type Cluster struct {
	cfg      config.GlesysConfig
	cacheDir string
	logger   *zap.Logger

	state state
	gc    *glesysapi.Client
}

// PublicIPv4 is the server's public address: the Talos and
// Kubernetes API endpoint.
func (c *Cluster) PublicIPv4() string { return c.state.IPv4 }

// TalosconfigPath is where this cluster's talosctl config was written.
func (c *Cluster) TalosconfigPath() string { return TalosconfigPath(c.cacheDir, c.cfg.Context) }

// Provision creates a GleSYS KVM server matching cfg and takes it to
// a usable single-node Talos cluster: reserved address, machine
// config, server, bootstrap, kubeconfig.
//
// Idempotency: if a server named cfg.Context already exists in the
// project, Provision treats that as an error rather than reusing it
// silently. The operator runs `teardown` first or picks a fresh
// context name.
func Provision(ctx context.Context, cfg config.GlesysConfig, logger *zap.Logger) (*Cluster, error) {
	if logger == nil {
		logger = zap.NewNop()
	}
	if err := cfg.Validate(); err != nil {
		return nil, fmt.Errorf("glesys config: %w", err)
	}
	gc, err := newClient()
	if err != nil {
		return nil, err
	}
	cacheDir := CacheDir()
	if err := os.MkdirAll(cacheDir, 0o755); err != nil {
		return nil, fmt.Errorf("mkdir cache: %w", err)
	}

	if existing, err := findServer(ctx, gc, cfg.Context); err != nil {
		return nil, err
	} else if existing != nil {
		return nil, fmt.Errorf("server %q already exists in this project (id=%s); run `y-cluster teardown -c <dir>` first or pick a different context", cfg.Context, existing.ID)
	}

	// The template check comes before anything is reserved: a
	// template name is the one input the config cannot validate
	// on its own, and the error lists the alternatives.
	if err := checkTemplate(ctx, gc, cfg.Platform, cfg.Template); err != nil {
		return nil, err
	}

	// Sizing was validated as numeric by cfg.Validate.
	memory, _ := positiveInt(cfg.Memory)
	cpus, _ := positiveInt(cfg.CPUs)
	diskGB, _ := config.DiskSizeGB(cfg.ServerDisk)

	// The address first: the machine config names the cluster
	// endpoint, and the endpoint is this address.
	ipv4, err := reserveIPv4(ctx, gc, cfg.DataCenter, cfg.Platform, logger)
	if err != nil {
		return nil, err
	}
	c := &Cluster{cfg: cfg, cacheDir: cacheDir, logger: logger, gc: gc,
		state: state{Context: cfg.Context, DataCenter: cfg.DataCenter, IPv4: ipv4}}
	// From here on a failure leaves something behind; the sidecar
	// is what lets Teardown find it.
	if err := saveState(cacheDir, c.state); err != nil {
		_ = gc.IPs.Release(ctx, ipv4)
		return nil, fmt.Errorf("save state: %w", err)
	}

	mc, err := generateMachineConfigs(cfg.Context, ipv4)
	if err != nil {
		return c, err
	}
	if err := os.WriteFile(machineConfigPath(cacheDir, cfg.Context), mc.controlPlane, 0o600); err != nil {
		return c, fmt.Errorf("write machine config: %w", err)
	}
	if err := mc.talosconfig.Save(c.TalosconfigPath()); err != nil {
		return c, fmt.Errorf("write talosconfig: %w", err)
	}

	logger.Info("creating GleSYS server",
		zap.String("name", cfg.Context),
		zap.String("dataCenter", cfg.DataCenter),
		zap.String("template", cfg.Template),
		zap.Int("memoryMB", memory), zap.Int("cpus", cpus), zap.Int("diskGB", diskGB),
		zap.String("ipv4", ipv4),
	)
	srv, err := gc.Servers.Create(ctx, glesysapi.CreateServerParams{
		Bandwidth:   bandwidthMbit,
		CloudConfig: string(mc.controlPlane),
		CPU:         cpus,
		DataCenter:  cfg.DataCenter,
		Description: "managed-by=y-cluster context=" + cfg.Context,
		Hostname:    cfg.Context,
		IPv4:        ipv4,
		IPv6:        "none",
		Memory:      memory,
		Platform:    cfg.Platform,
		Storage:     diskGB,
		Template:    cfg.Template,
	})
	if err != nil {
		return c, fmt.Errorf("server/create: %w", err)
	}
	c.state.ServerID = srv.ID
	if err := saveState(cacheDir, c.state); err != nil {
		return c, fmt.Errorf("save state: %w", err)
	}

	if err := c.waitForServerRunning(ctx); err != nil {
		return c, err
	}
	if err := c.bootstrapAndMergeKubeconfig(ctx, mc); err != nil {
		return c, err
	}
	if err := c.installGateway(ctx); err != nil {
		return c, err
	}
	return c, nil
}

// installGateway installs Envoy Gateway with the node's public
// address as the envoy Service's externalIP and as the GatewayClass
// dns-hint-ip, then the default Gateway that makes envoy start.
// gateway.skip leaves both out.
func (c *Cluster) installGateway(ctx context.Context) error {
	if c.cfg.Gateway.Skip {
		c.logger.Info("envoy gateway install skipped (gateway.skip)")
		return nil
	}
	if err := envoygateway.Install(ctx, envoygateway.Options{
		ContextName:          c.cfg.Context,
		GatewayClassName:     c.cfg.Gateway.ClassName,
		DNSHintIP:            c.state.IPv4,
		ExternalIPs:          []string{c.state.IPv4},
		ControllerCPURequest: c.cfg.Gateway.Resources.Controller.CPU,
		ControllerMemRequest: c.cfg.Gateway.Resources.Controller.Memory,
		ProxyCPURequest:      c.cfg.Gateway.Resources.Proxy.CPU,
		ProxyMemRequest:      c.cfg.Gateway.Resources.Proxy.Memory,
		Logger:               c.logger,
	}); err != nil {
		return fmt.Errorf("install envoy gateway: %w", err)
	}
	c.logger.Info("envoy gateway ready",
		zap.String("version", envoygateway.Version),
		zap.String("gatewayClass", c.cfg.Gateway.ClassName),
		zap.String("dnsHintIP", c.state.IPv4),
	)
	if err := c.installDefaultGateway(ctx); err != nil {
		return fmt.Errorf("install default Gateway: %w", err)
	}
	return nil
}

// bootstrapAndMergeKubeconfig is the Talos half of Provision: wait
// for apid, bootstrap etcd once, fetch the admin kubeconfig and
// merge it under the context.
func (c *Cluster) bootstrapAndMergeKubeconfig(ctx context.Context, mc *machineConfigs) error {
	tc, callCtx, err := talosClient(ctx, mc.talosconfig, c.state.IPv4)
	if err != nil {
		return err
	}
	defer func() { _ = tc.Close() }()

	c.logger.Info("waiting for the Talos API", zap.String("endpoint", c.state.IPv4+":50000"), zap.Duration("timeout", talosAPITimeout))
	if err := waitForTalosAPI(callCtx, tc, talosAPITimeout, c.logger); err != nil {
		return err
	}
	if !c.state.Bootstrapped {
		c.logger.Info("bootstrapping etcd")
		if err := bootstrap(callCtx, tc, bootstrapTimeout); err != nil {
			return err
		}
		c.state.Bootstrapped = true
		if err := saveState(c.cacheDir, c.state); err != nil {
			return fmt.Errorf("save state: %w", err)
		}
	}
	c.logger.Info("waiting for the Kubernetes API", zap.Duration("timeout", kubeconfigTimeout))
	raw, err := waitForKubeconfig(callCtx, tc, kubeconfigTimeout)
	if err != nil {
		return err
	}
	renamed, err := renameKubeconfig(raw, c.cfg.Context, c.cfg.Context)
	if err != nil {
		return err
	}
	mgr, err := kubeconfig.FromEnv(c.cfg.Context, c.cfg.Context, c.logger)
	if err != nil {
		return fmt.Errorf("kubeconfig manager: %w", err)
	}
	if err := mgr.Import(renamed); err != nil {
		return fmt.Errorf("merge kubeconfig: %w", err)
	}
	c.logger.Info("kubeconfig merged",
		zap.String("context", c.cfg.Context),
		zap.String("server", "https://"+c.state.IPv4+":6443"),
	)
	return waitForKubeAPI(ctx, c.cfg.Context, kubeAPIReadyTimeout, c.logger)
}

// waitForServerRunning polls server/details until GleSYS reports the
// server running and unlocked. Creation on KVM is a template copy
// plus first boot; a minute is typical.
func (c *Cluster) waitForServerRunning(ctx context.Context) error {
	c.logger.Info("waiting for the server to run", zap.String("id", c.state.ServerID))
	deadline := time.Now().Add(serverRunningTimeout)
	for {
		d, err := c.gc.Servers.Details(ctx, c.state.ServerID)
		if err == nil && d.IsRunning && !d.IsLocked {
			return nil
		}
		if time.Now().After(deadline) {
			if err != nil {
				return fmt.Errorf("server %s not running after %s: %w", c.state.ServerID, serverRunningTimeout, err)
			}
			return fmt.Errorf("server %s not running after %s (state %q, locked %v)", c.state.ServerID, serverRunningTimeout, d.State, d.IsLocked)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(5 * time.Second):
		}
	}
}

// findServer returns the project's server with the given hostname,
// or nil. GleSYS has no name lookup; the list is short.
func findServer(ctx context.Context, gc *glesysapi.Client, hostname string) (*glesysapi.Server, error) {
	servers, err := gc.Servers.List(ctx)
	if err != nil {
		return nil, fmt.Errorf("server/list: %w", err)
	}
	for _, s := range *servers {
		if s.Hostname == hostname {
			return &s, nil
		}
	}
	return nil, nil
}

// checkTemplate refuses a template name the platform does not offer.
// The error lists the Talos templates that are offered, since the
// name is the one thing about a config the operator has to know
// from the provider's catalogue.
func checkTemplate(ctx context.Context, gc *glesysapi.Client, platform, template string) error {
	templates, err := gc.Servers.Templates(ctx)
	if err != nil {
		return fmt.Errorf("server/templates: %w", err)
	}
	var offered []glesysapi.ServerPlatformTemplateDetails
	switch platform {
	case "KVM":
		offered = templates.KVM
	case "VMware":
		offered = templates.VMware
	}
	return matchTemplate(offered, platform, template)
}

// matchTemplate is checkTemplate's decision, apart from the API call.
func matchTemplate(offered []glesysapi.ServerPlatformTemplateDetails, platform, template string) error {
	var talos []string
	for _, t := range offered {
		if t.Name == template {
			return nil
		}
		if strings.Contains(strings.ToLower(t.Name), "talos") || strings.Contains(strings.ToLower(t.OS), "talos") {
			talos = append(talos, t.Name)
		}
	}
	if len(talos) == 0 {
		return fmt.Errorf("template %q is not offered on %s, and no Talos template is; the provisioner needs one (server/templates lists %d %s templates, none named talos)", template, platform, len(offered), platform)
	}
	return fmt.Errorf("template %q is not offered on %s; the Talos templates are: %s", template, platform, strings.Join(talos, ", "))
}

// reserveIPv4 takes the first free public IPv4 in the datacenter.
// Reserved before the server so the machine config can carry it;
// released by Teardown along with the server.
func reserveIPv4(ctx context.Context, gc *glesysapi.Client, dataCenter, platform string, logger *zap.Logger) (string, error) {
	avail, err := gc.IPs.Available(ctx, glesysapi.AvailableIPsParams{DataCenter: dataCenter, Platform: platform, Version: 4})
	if err != nil {
		return "", fmt.Errorf("ip/listfree: %w", err)
	}
	if avail == nil || len(*avail) == 0 {
		return "", fmt.Errorf("no free IPv4 in %s for %s", dataCenter, platform)
	}
	addr := (*avail)[0].Address
	logger.Info("reserving IPv4", zap.String("address", addr), zap.String("dataCenter", dataCenter))
	if _, err := gc.IPs.Reserve(ctx, addr); err != nil {
		return "", fmt.Errorf("ip/take %s: %w", addr, err)
	}
	return addr, nil
}

// Teardown destroys the server, releases its address, removes the
// kubeconfig context and deletes the state sidecar and talosconfig.
// Idempotent: missing resources are not errors.
func Teardown(ctx context.Context, contextName string, logger *zap.Logger) error {
	if logger == nil {
		logger = zap.NewNop()
	}
	gc, err := newClient()
	if err != nil {
		return err
	}
	cacheDir := CacheDir()
	st, _ := loadState(cacheDir, contextName) // ignore missing

	// Prefer the sidecar's server id, fall back to the name so a
	// missing sidecar does not strand the server.
	serverID := st.ServerID
	if serverID == "" {
		srv, err := findServer(ctx, gc, contextName)
		if err != nil {
			return err
		}
		if srv != nil {
			serverID = srv.ID
		}
	}
	if serverID != "" {
		logger.Info("destroying GleSYS server", zap.String("id", serverID), zap.String("name", contextName))
		if err := gc.Servers.Destroy(ctx, serverID, glesysapi.DestroyServerParams{KeepIP: false}); err != nil {
			if !isNotFound(err) {
				return fmt.Errorf("server/destroy %s: %w", serverID, err)
			}
			logger.Info("server already gone", zap.String("id", serverID))
		}
	} else {
		logger.Info("no server to destroy", zap.String("context", contextName))
	}
	// The address outlives a server that was never created (a
	// provision that failed between ip/take and server/create), and
	// destroy with keepip=false has already released it otherwise.
	// Either way a release that finds nothing is not a failure.
	if st.IPv4 != "" {
		if err := gc.IPs.Release(ctx, st.IPv4); err != nil {
			logger.Debug("ip/release", zap.String("address", st.IPv4), zap.Error(err))
		}
	}

	if mgr, err := kubeconfig.FromEnv(contextName, contextName, logger); err == nil {
		mgr.CleanupStale()
	} else {
		logger.Warn("kubeconfig context not removed", zap.Error(err))
	}
	if err := deleteState(cacheDir, contextName); err != nil {
		return fmt.Errorf("delete state: %w", err)
	}
	return nil
}

// Stop powers the server off (a soft stop, so Talos shuts down
// cleanly). The reserved address, disk and sidecar stay, which is
// what `y-cluster start` needs. Billing continues; operators who
// want to free it run `y-cluster teardown`.
func Stop(ctx context.Context, contextName string, logger *zap.Logger) error {
	if logger == nil {
		logger = zap.NewNop()
	}
	gc, err := newClient()
	if err != nil {
		return err
	}
	serverID, err := resolveServerID(ctx, gc, contextName)
	if err != nil {
		return err
	}
	logger.Info("GleSYS server stop", zap.String("id", serverID), zap.String("name", contextName))
	if err := gc.Servers.Stop(ctx, serverID, glesysapi.StopServerParams{Type: "soft"}); err != nil {
		return fmt.Errorf("server/stop: %w", err)
	}
	return nil
}

// Start powers on a server stopped by Stop and returns its public
// IPv4. The Talos cluster comes back on its own: etcd and the
// machine config are on the disk.
func Start(ctx context.Context, contextName string, logger *zap.Logger) (string, error) {
	if logger == nil {
		logger = zap.NewNop()
	}
	gc, err := newClient()
	if err != nil {
		return "", err
	}
	serverID, err := resolveServerID(ctx, gc, contextName)
	if err != nil {
		return "", err
	}
	logger.Info("GleSYS server start", zap.String("id", serverID), zap.String("name", contextName))
	if err := gc.Servers.Start(ctx, serverID); err != nil {
		return "", fmt.Errorf("server/start: %w", err)
	}
	st, err := loadState(CacheDir(), contextName)
	if err != nil {
		return "", fmt.Errorf("no state sidecar for %q: %w", contextName, err)
	}
	return st.IPv4, nil
}

// resolveServerID is the sidecar-then-name lookup Stop and Start
// share.
func resolveServerID(ctx context.Context, gc *glesysapi.Client, contextName string) (string, error) {
	if st, err := loadState(CacheDir(), contextName); err == nil && st.ServerID != "" {
		return st.ServerID, nil
	}
	srv, err := findServer(ctx, gc, contextName)
	if err != nil {
		return "", err
	}
	if srv == nil {
		return "", fmt.Errorf("no GleSYS server named %q in this project", contextName)
	}
	return srv.ID, nil
}

// isNotFound recognises the API's answer for a server that no longer
// exists. glesys-go surfaces API errors as text, so this is a match
// on the status text GleSYS uses.
func isNotFound(err error) bool {
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "not found") || strings.Contains(msg, "does not exist") || strings.Contains(msg, "404")
}

// positiveInt mirrors config's parser for the fields Validate has
// already accepted.
func positiveInt(s string) (int, error) {
	var n int
	_, err := fmt.Sscanf(strings.TrimSpace(s), "%d", &n)
	return n, err
}
