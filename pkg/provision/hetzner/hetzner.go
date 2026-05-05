// Package hetzner provisions a single-node k3s cluster on Hetzner
// Cloud. It mirrors the qemu provisioner's API surface (Provision /
// Teardown / Stop / Start / RunShell) so cmd/y-cluster's dispatch
// picks it up uniformly.
//
// Phase 1 (this commit) lands bare Provision + Teardown:
//
//   - SSH keypair generated per context, uploaded as a Hetzner
//     SSHKey resource named after the context.
//   - Server created from a public Ubuntu cloud image with
//     user_data that creates the unprivileged user + pins
//     cloud-init's datasource_list. NO k3s install yet -- the
//     cloud-init payload stays small; k3s lands via SSH after
//     first boot in a follow-up phase.
//   - State sidecar (<context>.json) tracks server ID + IPv4 +
//     SSH key resource name so Teardown can reverse without the
//     YAML config in hand.
//
// Later phases bolt on:
//
//   - k3s install + envoy-gateway (phase 1 finish)
//   - Auto-teardown via at(1)               (phase 2)
//   - Shared LB + TLS + dns-hint-ip          (phase 3)
//   - images load --from-url=                (phase 4)
//   - Per-dev .env defaults + polish         (phase 5)
//
// See specs/y-cluster/HETZNER_PROVISIONER.md.
package hetzner

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/hetznercloud/hcloud-go/v2/hcloud"
	"go.uber.org/zap"

	"github.com/Yolean/y-cluster/pkg/provision/config"
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
		return nil, fmt.Errorf("%s is unset; source ENV_FILE (typically ~/Yolean/.yolean-bots-device/y-cluster-hetzner-$USER.env) before running this command", HCloudTokenEnv)
	}
	return hcloud.NewClient(hcloud.WithToken(tok)), nil
}

// Provision creates a Hetzner Cloud server matching cfg, generates
// an SSH key for it, and waits for SSH to come up. Returns a
// *Cluster ready for follow-up SSH-driven steps (k3s install in
// phase 1.b, LB attach in phase 3).
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

	// Cloud-init payload. Phase 1: just the user + datasource
	// pin. k3s lands via SSH after boot in phase 1.b.
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
			"lb-group":   cfg.LBGroup,
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

	// Wait for sshd. Phase 1.b: install k3s and merge kubeconfig.
	// Phase 3 layers envoy-gateway + LB on top.
	if err := c.waitForSSH(ctx, 3*time.Minute); err != nil {
		return nil, fmt.Errorf("wait for SSH: %w", err)
	}
	logger.Info("SSH reachable")

	if err := c.installK3s(ctx); err != nil {
		return nil, fmt.Errorf("install k3s: %w", err)
	}
	if err := c.MergeKubeconfig(ctx); err != nil {
		return nil, fmt.Errorf("merge kubeconfig: %w", err)
	}

	// Phase 3.a: ensure the lb-group LB exists and includes us.
	// Server already carries managed-by + lb-group labels, so the
	// label_selector target on the LB picks us up on creation.
	lb, err := ensureLoadBalancer(ctx, hc, lbConfig{
		LBGroup:  cfg.LBGroup,
		Location: cfg.Location,
	}, logger)
	if err != nil {
		return nil, fmt.Errorf("ensure load balancer: %w", err)
	}
	c.state.LBGroup = cfg.LBGroup
	c.state.LBID = lb.ID
	if err := saveState(c.cacheDir, c.state); err != nil {
		return nil, fmt.Errorf("save state with LB id: %w", err)
	}

	// Schedule auto-teardown last: every preceding step needs to
	// have produced a working cluster, otherwise we'd queue a job
	// that fires against a half-provisioned (or already-failed)
	// state. The job id goes back into the state sidecar so
	// Teardown can cancel it.
	if err := c.scheduleAutoTeardown(ctx); err != nil {
		return nil, fmt.Errorf("schedule auto-teardown: %w", err)
	}

	logger.Info("cluster ready", zap.String("context", c.cfg.Context))

	return c, nil
}

// scheduleAutoTeardown queues `y-cluster teardown -c <Dir>` on the
// operator's host via at(1). The shell command captures the
// current binary path so a future-self atd run doesn't depend on
// $PATH; it captures cfg.Dir absolute (load.go has already
// resolved it). Records the job id in the state sidecar.
func (c *Cluster) scheduleAutoTeardown(ctx context.Context) error {
	exe, err := os.Executable()
	if err != nil {
		return fmt.Errorf("resolve own binary path: %w", err)
	}
	shellCmd := fmt.Sprintf("%s teardown -c %s",
		shellQuote(exe), shellQuote(c.cfg.Dir))
	id, err := atSchedule(ctx, c.cfg.AutoTeardownHours, shellCmd, c.logger)
	if err != nil {
		return err
	}
	c.state.AtJobID = id
	if err := saveState(c.cacheDir, c.state); err != nil {
		// Cancel the job we just scheduled rather than leak it
		// against a state sidecar that doesn't know about it.
		atRemove(ctx, id, c.logger)
		return fmt.Errorf("save state with at job id: %w", err)
	}
	return nil
}

// Teardown deletes the Hetzner server and its uploaded SSH key
// resource, cancels the at(1) auto-teardown job, removes the state
// sidecar, and shreds the local keypair. Idempotent: missing
// resources are not errors.
//
// LB detach lands in phase 3; this teardown is server + key + at
// job + local files.
func Teardown(ctx context.Context, contextName string, logger *zap.Logger) error {
	if logger == nil {
		logger = zap.NewNop()
	}
	hc, err := newClient()
	if err != nil {
		return err
	}
	cacheDir := CacheDir()

	// Server delete: prefer the state sidecar's server ID, fall
	// back to a name-based lookup so a missing sidecar (e.g. the
	// operator deleted it manually) doesn't strand the server.
	st, _ := loadState(cacheDir, contextName) // ignore missing

	// Cancel the at(1) auto-teardown first. If we deleted the
	// server first and then crashed before cancelling, the at job
	// would still fire and call Teardown again -- harmless but
	// noisy. Reverse order keeps logs cleaner.
	atRemove(ctx, st.AtJobID, logger)

	var srv *hcloud.Server
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
	if srv != nil {
		logger.Info("deleting Hetzner server",
			zap.Int64("id", srv.ID), zap.String("name", srv.Name))
		_, _, err := hc.Server.DeleteWithResult(ctx, srv)
		if err != nil {
			return fmt.Errorf("delete server: %w", err)
		}
	} else {
		logger.Info("no server to delete", zap.String("context", contextName))
	}

	// LB delete IFF this server was the last lb-group member. The
	// label_selector target on the LB drops the just-deleted
	// server from rotation automatically, so we just need to
	// count remaining managed-by=y-cluster,lb-group=<grp> servers.
	if err := deleteLBIfEmpty(ctx, hc, st.LBGroup, st.LBID, logger); err != nil {
		return fmt.Errorf("teardown LB: %w", err)
	}

	// SSH key delete.
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

	// Local keypair + state sidecar.
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

// SSH runs cmd on the cluster's node over SSH. Mirrors qemu's
// Cluster.SSH so the cmd/y-cluster ctr / crictl / RunShell paths
// can dispatch uniformly via cluster.Lookup.
func (c *Cluster) SSH(ctx context.Context, cmd string) ([]byte, error) {
	out, err := sshexec.Exec(ctx, sshexec.Target{
		Host:    c.state.IPv4,
		Port:    "22",
		User:    c.cfg.SSHUser,
		KeyPath: filepath.Join(c.cacheDir, c.cfg.Context+"-ssh"),
	}, cmd, nil)
	if err != nil {
		return out, err
	}
	return out, nil
}

// PublicIPv4 is what the operator's host hits for SSH and (until
// the LB lands in phase 3) HTTP/HTTPS too.
func (c *Cluster) PublicIPv4() string { return c.state.IPv4 }

// State returns a snapshot of the persisted sidecar. Tests use it
// to assert what Provision wrote without poking the file directly.
func (c *Cluster) State() state { return c.state }

// waitForSSH polls the configured ystack@<ipv4>:22 until a trivial
// remote `true` succeeds or timeout fires. Hetzner servers usually
// reach SSH within ~30s of create-action completion; the longer
// ceiling guards against image-pull / cloud-init slowness.
func (c *Cluster) waitForSSH(ctx context.Context, timeout time.Duration) error {
	c.logger.Info("waiting for SSH", zap.String("host", c.state.IPv4))
	deadline := time.Now().Add(timeout)
	keyPath := filepath.Join(c.cacheDir, c.cfg.Context+"-ssh")
	for {
		_, err := sshexec.Exec(ctx, sshexec.Target{
			Host:    c.state.IPv4,
			Port:    "22",
			User:    c.cfg.SSHUser,
			KeyPath: keyPath,
		}, "true", nil)
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
	for _, a := range actions {
		if _, errCh := hc.Action.WatchProgress(ctx, a); errCh != nil {
			if err := <-errCh; err != nil {
				return fmt.Errorf("action %d (%s): %w", a.ID, a.Command, err)
			}
		}
	}
	return nil
}

// labelSelectorForGroup returns a Hetzner-API label selector
// matching every server we created for the given lbGroup. Used in
// phase 3 to enumerate group members for the shared LB; defined
// here so phase 1's create flow and phase 3's lookup flow share
// the same label vocabulary.
func labelSelectorForGroup(lbGroup string) string {
	return strings.Join([]string{
		"managed-by=y-cluster",
		"lb-group=" + lbGroup,
	}, ",")
}
