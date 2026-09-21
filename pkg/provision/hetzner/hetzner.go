// Package hetzner provisions a single-node k3s cluster on Hetzner
// Cloud. cmd/y-cluster dispatches to its package-level Provision /
// Teardown / Stop / Start; unlike the local providers it does not
// implement provision.Cluster, because its verbs address a cluster by
// context name through the Hetzner API rather than through an
// in-memory handle.
//
// What a provision leaves behind, and Teardown reverses:
//
//   - an SSH keypair per context, the public half uploaded as a
//     Hetzner SSHKey resource named after the context;
//   - a server from a public Ubuntu cloud image, with k3s installed
//     over SSH after first boot and Envoy Gateway on top;
//   - a self-signed certificate and a load balancer shared by the
//     lb group, carrying the dns-hint-ip for /etc/hosts tooling;
//   - an in-cluster reaper Job that enforces lifetime.maxRun, so
//     expiry does not depend on the provisioning host being up;
//   - a state sidecar (<context>.json) with the resource ids, so
//     Teardown works without the YAML config in hand.
//
// See HETZNER_PROVISIONER.md in the specs repo (specs/y-cluster/).
package hetzner

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/hetznercloud/hcloud-go/v2/hcloud"
	"go.uber.org/zap"

	"github.com/Yolean/y-cluster/pkg/kubeconfig"
	"github.com/Yolean/y-cluster/pkg/provision/config"
	"github.com/Yolean/y-cluster/pkg/provision/envoygateway"
	"github.com/Yolean/y-cluster/pkg/sshexec"
)

// CacheDirEnv lets tests / multi-tenant CI override the on-disk
// cache root without writing to the operator's home dir.
const CacheDirEnv = "Y_CLUSTER_HETZNER_CACHE_DIR"

// HCloudTokenEnv is the credential the operator places via .env or
// shell. Per-developer scope: each dev has their own token.
const HCloudTokenEnv = "HCLOUD_TOKEN"

// labelManagedBy tags every resource we create so a future reaper
// can list-and-cull without colliding with manual / other-tool
// resources in the same project.
const labelManagedBy = "managed-by=y-cluster"

// labelLBGroup is the label key that says which lb-group's load
// balancer a server or certificate belongs to. The LB targets servers
// by it, and Teardown reads it back when there is no state sidecar.
const labelLBGroup = "lb-group"

// Cluster is the running-state handle Provision returns. Keeps the
// fields cluster.Lookup needs to wire ctr / crictl / RunShell over
// SSH against the public IPv4.
type Cluster struct {
	cfg      config.HetznerConfig
	cacheDir string
	logger   *zap.Logger

	state state
	hc    *hcloud.Client
}

// CacheDir resolves the on-disk cache root. Order: env override,
// then ~/.cache/y-cluster-hetzner.
func CacheDir() string {
	if v := os.Getenv(CacheDirEnv); v != "" {
		return v
	}
	home, err := os.UserHomeDir()
	if err != nil {
		// $HOME unset is exotic; fall back to /tmp so callers
		// still get a writable dir even if it disappears across
		// runs.
		return filepath.Join(os.TempDir(), "y-cluster-hetzner")
	}
	return filepath.Join(home, ".cache", "y-cluster-hetzner")
}

// newClient builds an hcloud client from $HCLOUD_TOKEN. Returns a
// clear error if the token is unset; the operator's .env at the
// repo root is supposed to source it.
func newClient() (*hcloud.Client, error) {
	tok := os.Getenv(HCloudTokenEnv)
	if tok == "" {
		return nil, fmt.Errorf("%s is unset; source ~/Yolean/.yolean-bots-device/y-cluster-hetzner.env (or wherever your token lives) before running this command", HCloudTokenEnv)
	}
	return hcloud.NewClient(append([]hcloud.ClientOption{hcloud.WithToken(tok)}, hcloudClientOptions...)...), nil
}

// The two ways this package reaches outside the process, as variables
// so that tests can run Provision and Teardown against a fake Hetzner
// Cloud API and a fake node. Nothing in this package can be run
// against the real cloud from CI.
var (
	hcloudClientOptions []hcloud.ClientOption
	sshExec             = sshexec.Exec
)

// sshWaitTimeout is how long Provision waits for sshd on a new
// server. A variable so tests of the failure need not wait for it.
var sshWaitTimeout = 3 * time.Minute

// Provision creates a Hetzner Cloud server matching cfg and takes it
// all the way to a usable cluster: SSH key, server, k3s over SSH,
// optional image pre-load, certificate, load balancer, Envoy
// Gateway, expiry reaper, and last the upstream-pull lockdown.
//
// Idempotency: if a server with cfg.Context already exists in the
// project, Provision treats that as an error rather than reusing it
// silently. The operator runs `teardown` first or picks a fresh
// context name. Per-context state sidecars make accidental reuse
// loud.
func Provision(ctx context.Context, cfg config.HetznerConfig, logger *zap.Logger) (*Cluster, error) {
	if logger == nil {
		logger = zap.NewNop()
	}
	if err := cfg.Validate(); err != nil {
		return nil, fmt.Errorf("hetzner config: %w", err)
	}
	hc, err := newClient()
	if err != nil {
		return nil, err
	}
	cacheDir := CacheDir()
	if err := os.MkdirAll(cacheDir, 0o755); err != nil {
		return nil, fmt.Errorf("mkdir cache: %w", err)
	}

	// Refuse to clobber an existing server with the same name.
	// Hetzner's CreateServer would actually error out itself, but
	// our error message names the cleanup recipe.
	existing, _, err := hc.Server.GetByName(ctx, cfg.Context)
	if err != nil {
		return nil, fmt.Errorf("probe existing server %q: %w", cfg.Context, err)
	}
	if existing != nil {
		return nil, fmt.Errorf("server %q already exists in this project (id=%d); run `y-cluster teardown -c <dir>` first or pick a different context", cfg.Context, existing.ID)
	}

	// SSH key: rotate per-provision (matches qemu's per-VM key
	// isolation). The Hetzner SSHKey resource name is the
	// context too, so Teardown can find it without reading the
	// state sidecar.
	keyPath := filepath.Join(cacheDir, cfg.Context+"-ssh")
	logger.Info("generating SSH keypair", zap.String("path", keyPath))
	if err := sshexec.GenerateKey(keyPath); err != nil {
		return nil, fmt.Errorf("generate ssh key: %w", err)
	}
	pubKey, err := os.ReadFile(keyPath + ".pub")
	if err != nil {
		return nil, fmt.Errorf("read public key: %w", err)
	}

	logger.Info("uploading SSH key to Hetzner", zap.String("name", cfg.Context))
	hcKey, _, err := hc.SSHKey.Create(ctx, hcloud.SSHKeyCreateOpts{
		Name:      cfg.Context,
		PublicKey: string(pubKey),
		Labels:    map[string]string{"managed-by": "y-cluster"},
	})
	if err != nil {
		return nil, fmt.Errorf("upload ssh key: %w", err)
	}

	// Cloud-init payload: just the user + datasource pin. k3s is
	// installed over SSH after boot, which keeps user_data small.
	userData := renderCloudInitUserData(cfg.Context, cfg.SSHUser, string(pubKey))

	logger.Info("creating Hetzner server",
		zap.String("name", cfg.Context),
		zap.String("type", cfg.ServerType),
		zap.String("location", cfg.Location),
		zap.String("image", cfg.OSImage),
	)
	createRes, _, err := hc.Server.Create(ctx, hcloud.ServerCreateOpts{
		Name:       cfg.Context,
		ServerType: &hcloud.ServerType{Name: cfg.ServerType},
		Image:      &hcloud.Image{Name: cfg.OSImage},
		Location:   &hcloud.Location{Name: cfg.Location},
		SSHKeys:    []*hcloud.SSHKey{hcKey},
		UserData:   userData,
		Labels: map[string]string{
			"managed-by": "y-cluster",
			"context":    cfg.Context,
			labelLBGroup: cfg.LBGroup,
		},
	})
	if err != nil {
		// Best-effort: clean up the uploaded key so we don't
		// leak the resource on a partial failure.
		_, _ = hc.SSHKey.Delete(ctx, hcKey)
		return nil, fmt.Errorf("create server: %w", err)
	}

	srv := createRes.Server
	if len(createRes.NextActions) > 0 {
		logger.Info("waiting for create actions to complete",
			zap.Int("actionCount", len(createRes.NextActions)))
		if err := waitForActions(ctx, hc, createRes.NextActions); err != nil {
			return nil, fmt.Errorf("wait for create actions: %w", err)
		}
	}

	ipv4 := ""
	if srv.PublicNet.IPv4.IP != nil {
		ipv4 = srv.PublicNet.IPv4.IP.String()
	}
	logger.Info("Hetzner server created",
		zap.Int64("id", srv.ID),
		zap.String("ipv4", ipv4),
	)

	st := state{
		Context:    cfg.Context,
		ServerID:   srv.ID,
		ServerName: srv.Name,
		IPv4:       ipv4,
		SSHUser:    cfg.SSHUser,
		SSHKeyName: cfg.Context,
	}
	// The lifetime policy rides in the sidecar so `y-cluster
	// start` can re-arm the expiry reaper without the YAML config.
	if cfg.Lifetime.Enabled() {
		st.LifetimeMaxRun = cfg.Lifetime.MaxRun
		st.LifetimeOnExpiry = cfg.Lifetime.OnExpiry
	}
	if err := saveState(cacheDir, st); err != nil {
		return nil, fmt.Errorf("save state: %w", err)
	}

	c := &Cluster{
		cfg:      cfg,
		cacheDir: cacheDir,
		logger:   logger,
		state:    st,
		hc:       hc,
	}

	// Wait for sshd, then install k3s and merge the kubeconfig.
	if err := c.waitForSSH(ctx, sshWaitTimeout); err != nil {
		return nil, fmt.Errorf("wait for SSH: %w", err)
	}
	logger.Info("SSH reachable")

	if err := c.installK3s(ctx); err != nil {
		return nil, fmt.Errorf("install k3s: %w", err)
	}
	if err := c.MergeKubeconfig(ctx); err != nil {
		return nil, fmt.Errorf("merge kubeconfig: %w", err)
	}

	// pre-load images from the S3 cache, if configured.
	// Lands here -- after k3s is up (containerd is reachable for
	// `ctr image import`) but before envoy-gateway, so the gateway
	// install benefits from cache hits too.
	if cfg.ImageCache.Enabled() {
		if err := c.preloadFromS3(ctx); err != nil {
			return nil, fmt.Errorf("preload images: %w", err)
		}
	}

	// generate + upload a per-context self-signed cert
	// before the LB so a fresh-LB create can include it in the
	// initial HTTPS service (Hetzner refuses an empty cert list).
	commonName, dnsNames := certSubjectsForContext(cfg.Context, cfg.LBGroup, cfg.FQDNDomain)
	certPEM, keyPEM, err := generateSelfSignedCert(commonName, dnsNames, nil)
	if err != nil {
		return nil, fmt.Errorf("generate self-signed cert: %w", err)
	}
	cert, err := uploadCertificate(ctx, hc, cfg.Context, cfg.LBGroup, certPEM, keyPEM, logger)
	if err != nil {
		return nil, fmt.Errorf("upload certificate: %w", err)
	}
	c.state.CertificateID = cert.ID
	if err := saveState(c.cacheDir, c.state); err != nil {
		return nil, fmt.Errorf("save state with cert id: %w", err)
	}

	// ensure the lb-group LB exists and includes us.
	// Server already carries managed-by + lb-group labels, so the
	// label_selector target on the LB picks us up on creation.
	// On a fresh-LB create we hand cert in as firstCert so the
	// HTTPS-443 service lands with at least one cert; on reuse the
	// follow-up attachCertificateToLB call adds our cert to the
	// existing service's cert list.
	lb, err := ensureLoadBalancer(ctx, hc, lbConfig{
		LBGroup:  cfg.LBGroup,
		Location: cfg.Location,
	}, cert, logger)
	if err != nil {
		return nil, fmt.Errorf("ensure load balancer: %w", err)
	}
	c.state.LBGroup = cfg.LBGroup
	c.state.LBID = lb.ID
	if err := saveState(c.cacheDir, c.state); err != nil {
		return nil, fmt.Errorf("save state with LB id: %w", err)
	}
	if err := attachCertificateToLB(ctx, hc, lb, cert, logger); err != nil {
		return nil, fmt.Errorf("attach cert to LB: %w", err)
	}
	lbIPv4 := ""
	if lb.PublicNet.IPv4.IP != nil {
		lbIPv4 = lb.PublicNet.IPv4.IP.String()
	}

	// install envoy-gateway and stamp the LB IPv4 onto
	// the GatewayClass under yolean.se/dns-hint-ip. ystack's
	// y-k8s-ingress-hosts reads that annotation when populating
	// /etc/hosts on the operator's machine, so consumer kustomize
	// bases that use *.<ctx>.local.test FQDNs resolve to the LB
	// without any user-supplied DNS.
	//
	// NB: the older "dns-hint-ip is for tunnel-NAT only; cloud-LB
	// provisioners leave it empty" guidance does NOT apply here:
	// the dev-cluster shape uses RFC-6761-reserved local.test
	// FQDNs that have no public DNS, so the operator's host needs
	// the hint to reach the LB.
	if cfg.Gateway.Skip {
		logger.Info("envoy gateway install skipped (gateway.skip)")
	} else {
		if err := envoygateway.Install(ctx, envoygateway.Options{
			ContextName:      cfg.Context,
			GatewayClassName: cfg.Gateway.ClassName,
			DNSHintIP:        lbIPv4,
			Logger:           logger,
		}); err != nil {
			return nil, fmt.Errorf("install envoy gateway: %w", err)
		}
		logger.Info("envoy gateway ready",
			zap.String("version", envoygateway.Version),
			zap.String("gatewayClass", cfg.Gateway.ClassName),
			zap.String("dnsHintIP", lbIPv4),
		)
		// Apply the per-cluster Gateway resource. envoy-gateway
		// only spawns its data-plane Pod (and the LoadBalancer
		// Service that klipper-lb binds to host:80) when a Gateway
		// referencing this GatewayClass exists. Without this step
		// the Hetzner LB targets a node with nothing on :80 and
		// the LB health check stays red.
		if err := c.installDefaultGateway(ctx); err != nil {
			return nil, fmt.Errorf("install default Gateway: %w", err)
		}
	}

	// the
	// in-cluster reaper Job is hetzner's expiry mechanism (the
	// analog of GCP's max-run-duration). It sleeps lifetime.maxRun
	// then runs onExpiry via the hcloud API. Installed only when a
	// budget is configured -- an empty lifetime disables expiry
	// entirely, matching local semantics. Lives in the cluster so
	// an operator-machine going away doesn't strand paid Hetzner
	// resources -- the trade earlier at(1)-on-host work didn't
	// pass.
	if cfg.Lifetime.Enabled() {
		maxRun, err := cfg.Lifetime.MaxRunDuration()
		if err != nil {
			return nil, err // unreachable: Validate parsed it above
		}
		if cfg.Lifetime.OnExpiry != config.OnExpiryTeardown {
			// Mirrors the lifetime spec's billing note: unlike a
			// local VM, a stopped Hetzner server is not free.
			logger.Warn("lifetime.onExpiry stop keeps billing on hetzner: a stopped server still bills its resources; identity (server id + IP + disk) is preserved for `y-cluster start`; set onExpiry teardown for cost control")
		}
		if err := installReaper(ctx, installReaperOpts{
			KubectlContext: cfg.Context,
			ContextName:    cfg.Context,
			HCloudToken:    readHCloudToken(),
			MaxRun:         maxRun,
			OnExpiry:       cfg.Lifetime.OnExpiry,
			ServerID:       c.state.ServerID,
			LBID:           c.state.LBID,
			LBGroup:        cfg.LBGroup,
		}, logger); err != nil {
			return nil, fmt.Errorf("install reaper: %w", err)
		}
		logger.Info("lifetime expiry reaper installed",
			zap.Duration("maxRun", maxRun),
			zap.String("onExpiry", cfg.Lifetime.OnExpiry))
	} else {
		logger.Info("no lifetime configured; cluster runs (and bills) until manual stop or teardown")
	}

	// lock down upstream pulls. Lands LAST so the
	// bootstrap (pre-load, envoy-gateway, reaper apply) finishes
	// against an upstream-pull-allowed k3s and only the workload
	// image-pull surface tightens up.
	if cfg.ImageCache.Enabled() && cfg.ImageCache.RejectUpstream {
		if err := c.applyRejectUpstream(ctx); err != nil {
			return nil, fmt.Errorf("apply rejectUpstream: %w", err)
		}
	}

	logger.Info("cluster ready", zap.String("context", c.cfg.Context))

	return c, nil
}

// Teardown removes everything Provision made for contextName: the
// server, its certificate (taken off the lb-group's load balancer
// first if that survives), the load balancer if this was its last
// server, the ssh key, the kubeconfig entries, the local keypair and
// the state sidecar. Idempotent: missing resources are not errors.
func Teardown(ctx context.Context, contextName string, logger *zap.Logger) error {
	if logger == nil {
		logger = zap.NewNop()
	}
	hc, err := newClient()
	if err != nil {
		return err
	}
	return teardown(ctx, hc, CacheDir(), contextName, logger)
}

// teardown is Teardown with the client in hand, which is how a failed
// Provision undoes itself.
func teardown(ctx context.Context, hc *hcloud.Client, cacheDir, contextName string, logger *zap.Logger) error {
	// The sidecar is a cache of ids. Everything it holds can be found
	// again by name or label, so a missing one (another machine, a
	// cleaned home directory) does not strand anything.
	st, _ := loadState(cacheDir, contextName)

	var srv *hcloud.Server
	var err error
	if st.ServerID != 0 {
		srv, _, err = hc.Server.GetByID(ctx, st.ServerID)
		if err != nil {
			return fmt.Errorf("describe server id=%d: %w", st.ServerID, err)
		}
	}
	if srv == nil {
		srv, _, err = hc.Server.GetByName(ctx, contextName)
		if err != nil {
			return fmt.Errorf("describe server name=%q: %w", contextName, err)
		}
	}

	var hcCert *hcloud.Certificate
	if st.CertificateID != 0 {
		hcCert, _, err = hc.Certificate.GetByID(ctx, st.CertificateID)
		if err != nil {
			return fmt.Errorf("describe certificate id=%d: %w", st.CertificateID, err)
		}
	}
	if hcCert == nil {
		hcCert, _, err = hc.Certificate.GetByName(ctx, contextName)
		if err != nil {
			return fmt.Errorf("describe certificate name=%q: %w", contextName, err)
		}
	}

	// Which load balancer this context shares is on the labels
	// Provision gave the server and the certificate.
	lbGroup := st.LBGroup
	if lbGroup == "" && srv != nil {
		lbGroup = srv.Labels[labelLBGroup]
	}
	if lbGroup == "" && hcCert != nil {
		lbGroup = hcCert.Labels[labelLBGroup]
	}

	if srv != nil {
		logger.Info("deleting Hetzner server",
			zap.Int64("id", srv.ID), zap.String("name", srv.Name))
		if _, _, err := hc.Server.DeleteWithResult(ctx, srv); err != nil {
			return fmt.Errorf("delete server: %w", err)
		}
	} else {
		logger.Info("no server to delete", zap.String("context", contextName))
	}

	// Order from here is delicate -- Hetzner refuses to delete a
	// Certificate that's still referenced by an LB service. So:
	//
	//   1. If the LB is deleted (we were the last lb-group member),
	//      that releases all cert references; we delete our
	//      Certificate afterwards.
	//   2. If the LB stays (other servers remain), our cert comes
	//      off its 443 service first, THEN is deleted.
	if lbGroup != "" {
		lb, err := findLoadBalancer(ctx, hc, st.LBID, lbGroup)
		if err != nil {
			return err
		}
		if lb != nil {
			servers, err := hc.Server.AllWithOpts(ctx, hcloud.ServerListOpts{
				ListOpts: hcloud.ListOpts{LabelSelector: labelSelectorForGroup(lbGroup)},
			})
			if err != nil {
				return fmt.Errorf("list lb-group %q servers: %w", lbGroup, err)
			}
			switch {
			case len(servers) == 0:
				logger.Info("deleting Hetzner LB (last lb-group member gone)",
					zap.Int64("id", lb.ID), zap.String("name", lb.Name))
				if _, err := hc.LoadBalancer.Delete(ctx, lb); err != nil {
					return fmt.Errorf("delete LB: %w", err)
				}
			case hcCert != nil:
				logger.Info("LB retains members; not deleting",
					zap.String("lbGroup", lbGroup), zap.Int("remainingServers", len(servers)))
				if err := detachCertificateFromLB(ctx, hc, lb, hcCert.ID, logger); err != nil {
					return fmt.Errorf("detach cert from LB: %w", err)
				}
			}
		}
	}

	if hcCert != nil {
		logger.Info("deleting Hetzner Certificate",
			zap.Int64("id", hcCert.ID), zap.String("name", hcCert.Name))
		if _, err := hc.Certificate.Delete(ctx, hcCert); err != nil {
			return fmt.Errorf("delete certificate: %w", err)
		}
	}

	keyName := contextName
	if st.SSHKeyName != "" {
		keyName = st.SSHKeyName
	}
	hcKey, _, err := hc.SSHKey.GetByName(ctx, keyName)
	if err != nil {
		return fmt.Errorf("describe ssh key %q: %w", keyName, err)
	}
	if hcKey != nil {
		logger.Info("deleting Hetzner SSH key", zap.String("name", keyName))
		if _, err := hc.SSHKey.Delete(ctx, hcKey); err != nil {
			return fmt.Errorf("delete ssh key: %w", err)
		}
	}

	// The context names an address Hetzner hands to its next
	// customer.
	if mgr, err := kubeconfig.FromEnv(contextName, contextName, logger); err != nil {
		logger.Warn("kubeconfig manager", zap.Error(err))
	} else {
		mgr.CleanupStale()
	}

	keyPath := filepath.Join(cacheDir, contextName+"-ssh")
	for _, p := range []string{keyPath, keyPath + ".pub"} {
		if err := os.Remove(p); err != nil && !os.IsNotExist(err) {
			logger.Warn("remove local key file", zap.String("path", p), zap.Error(err))
		}
	}
	if err := deleteState(cacheDir, contextName); err != nil {
		logger.Warn("remove state sidecar", zap.Error(err))
	}
	return nil
}

// target is how the operator's host reaches the node over ssh.
func (c *Cluster) target() sshexec.Target {
	return sshexec.Target{
		Host:    c.state.IPv4,
		Port:    "22",
		User:    c.cfg.SSHUser,
		KeyPath: filepath.Join(c.cacheDir, c.cfg.Context+"-ssh"),
	}
}

// NodeExec runs a shell command on the node, optionally piping stdin
// into it, and returns the combined output.
func (c *Cluster) NodeExec(ctx context.Context, command string, stdin io.Reader) ([]byte, error) {
	return sshExec(ctx, c.target(), command, stdin)
}

// SSH is NodeExec without stdin.
func (c *Cluster) SSH(ctx context.Context, cmd string) ([]byte, error) {
	return c.NodeExec(ctx, cmd, nil)
}

// PublicIPv4 is what the operator's host hits for SSH and the k3s
// API. HTTP/HTTPS go through the lb group's load balancer.
func (c *Cluster) PublicIPv4() string { return c.state.IPv4 }

// State returns a snapshot of the persisted sidecar. Tests use it
// to assert what Provision wrote without poking the file directly.
func (c *Cluster) State() state { return c.state }

// Stop issues a graceful ACPI shutdown via the Hetzner API. The
// server stays in the project (and thus continues billing the disk;
// Hetzner has no "stopped, not billed" state for cx-class servers);
// the value of stop/start over teardown/provision is preserving the
// cluster identity (server ID, IPv4, kubeconfig context, in-cluster
// state) across breaks without re-installing k3s.
//
// A manual stop also ends the current lifetime window: the expiry
// reaper Job is deleted (best-effort) before the shutdown, and
// Start re-arms a fresh full window -- the same semantics as the
// local providers' host timer.
//
// Operators wanting to free billing entirely should run
// `y-cluster teardown` instead.
func Stop(ctx context.Context, contextName string, logger *zap.Logger) error {
	if logger == nil {
		logger = zap.NewNop()
	}
	hc, err := newClient()
	if err != nil {
		return err
	}
	srv, _, err := hc.Server.GetByName(ctx, contextName)
	if err != nil {
		return fmt.Errorf("describe server %q: %w", contextName, err)
	}
	if srv == nil {
		return fmt.Errorf("no Hetzner server named %q in this project", contextName)
	}
	// Lifecycle parity with local lifetime semantics: a manual stop
	// ends this run's budget. Remove the reaper Job while the
	// cluster is still reachable -- once the server is down there
	// is no API to delete it through. Best-effort by design.
	removeReaperJob(ctx, contextName, logger)
	logger.Info("hcloud server shutdown",
		zap.Int64("id", srv.ID), zap.String("name", srv.Name))
	action, _, err := hc.Server.Shutdown(ctx, srv)
	if err != nil {
		return fmt.Errorf("shutdown server: %w", err)
	}
	if action != nil {
		if err := waitForActions(ctx, hc, []*hcloud.Action{action}); err != nil {
			return fmt.Errorf("wait for shutdown: %w", err)
		}
	}
	return nil
}

// Start powers on a server stopped by Stop. Returns the refreshed
// public IPv4 in case Hetzner rotated it (it doesn't, in practice,
// but the contract leaves the door open for floating-IP setups).
// When the state sidecar carries a lifetime policy, Start waits for
// the kube API and re-installs the expiry reaper with a fresh full
// window (the budget counts from cluster start, matching local
// lifetime semantics).
func Start(ctx context.Context, contextName string, logger *zap.Logger) (string, error) {
	if logger == nil {
		logger = zap.NewNop()
	}
	hc, err := newClient()
	if err != nil {
		return "", err
	}
	srv, _, err := hc.Server.GetByName(ctx, contextName)
	if err != nil {
		return "", fmt.Errorf("describe server %q: %w", contextName, err)
	}
	if srv == nil {
		return "", fmt.Errorf("no Hetzner server named %q in this project; nothing to start (run `y-cluster provision` to create one)", contextName)
	}
	logger.Info("hcloud server poweron",
		zap.Int64("id", srv.ID), zap.String("name", srv.Name))
	action, _, err := hc.Server.Poweron(ctx, srv)
	if err != nil {
		return "", fmt.Errorf("poweron server: %w", err)
	}
	if action != nil {
		if err := waitForActions(ctx, hc, []*hcloud.Action{action}); err != nil {
			return "", fmt.Errorf("wait for poweron: %w", err)
		}
	}
	// Re-fetch to pick up the post-boot public-net snapshot. The id
	// is kept apart: GetByID returns a nil server along with its
	// error, and the error message is where the id is needed.
	serverID := srv.ID
	srv, _, err = hc.Server.GetByID(ctx, serverID)
	if err != nil {
		return "", fmt.Errorf("re-describe server id=%d: %w", serverID, err)
	}
	ipv4 := ""
	if srv != nil && srv.PublicNet.IPv4.IP != nil {
		ipv4 = srv.PublicNet.IPv4.IP.String()
	}
	// Lifecycle parity with local lifetime semantics: start re-arms
	// a fresh full window. The sidecar carries the policy captured
	// at provision; a missing sidecar (exotic: Start is gated on
	// HasState) can only warn since there is no policy to re-arm.
	if st, lerr := loadState(CacheDir(), contextName); lerr != nil {
		logger.Warn("no state sidecar; cannot re-arm the expiry reaper", zap.Error(lerr))
	} else if err := rearmReaper(ctx, st, logger); err != nil {
		// Fatal, not best-effort: an un-armed reaper on a running
		// server means unbounded billing, the exact failure mode
		// the lifetime feature exists to prevent. The server IS
		// running at this point; say so.
		return ipv4, fmt.Errorf("server is running but the expiry reaper was not re-armed: %w; retry `y-cluster start` or run `y-cluster stop` / `y-cluster teardown`", err)
	}
	return ipv4, nil
}

// waitForSSH polls the configured ystack@<ipv4>:22 until a trivial
// remote `true` succeeds or timeout fires. Hetzner servers usually
// reach SSH within ~30s of create-action completion; the longer
// ceiling guards against image-pull / cloud-init slowness.
func (c *Cluster) waitForSSH(ctx context.Context, timeout time.Duration) error {
	c.logger.Info("waiting for SSH", zap.String("host", c.state.IPv4))
	deadline := time.Now().Add(timeout)
	for {
		_, err := c.NodeExec(ctx, "true", nil)
		if err == nil {
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("SSH not reachable after %s: %w", timeout, err)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(3 * time.Second):
		}
	}
}

// waitForActions blocks until every action in `actions` reaches a
// terminal state. Any single action erroring fails the whole wait;
// caller decides whether to clean up partial work.
func waitForActions(ctx context.Context, hc *hcloud.Client, actions []*hcloud.Action) error {
	return hc.Action.WaitForFunc(ctx, func(update *hcloud.Action) error {
		if update.Status == hcloud.ActionStatusError {
			return fmt.Errorf("action %d (%s): %w", update.ID, update.Command, update.Error())
		}
		return nil
	}, actions...)
}

// labelSelectorForGroup returns a Hetzner-API label selector
// matching every server we created for the given lbGroup. The
// create flow labels servers and the shared-LB flow enumerates them;
// both go through here so they share one label vocabulary.
func labelSelectorForGroup(lbGroup string) string {
	return strings.Join([]string{
		labelManagedBy,
		labelLBGroup + "=" + lbGroup,
	}, ",")
}
