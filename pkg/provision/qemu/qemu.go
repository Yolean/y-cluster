// Package qemu provides a QEMU/KVM-based Kubernetes cluster provisioner.
//
// It creates an Ubuntu VM using cloud images, installs k3s via SSH,
// configures registry mirrors, and extracts a kubeconfig for kubectl.
//
// Prerequisites: qemu-system-x86_64, qemu-img, cloud-localds, /dev/kvm
package qemu

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"go.uber.org/zap"

	"github.com/Yolean/y-cluster/pkg/kubeconfig"
	"github.com/Yolean/y-cluster/pkg/provision"
	"github.com/Yolean/y-cluster/pkg/provision/config"
	"github.com/Yolean/y-cluster/pkg/provision/envoygateway"
	"github.com/Yolean/y-cluster/pkg/provision/localstorage"
	"github.com/Yolean/y-cluster/pkg/provision/registries"
	"github.com/Yolean/y-cluster/pkg/sshexec"
)

// Compile-time assertion that *Cluster satisfies provision.Cluster.
// Lives here rather than in a _test.go because the contract is part
// of the package's public surface.
var _ provision.Cluster = (*Cluster)(nil)

// PortForward maps a host port to a guest port.
type PortForward struct {
	Host  string // host port (empty = auto)
	Guest string // guest port
}

// Config is the runtime VM and cluster shape used by Provision and
// Teardown. The on-disk shape lives in
// github.com/Yolean/y-cluster/pkg/provision/config.QEMUConfig and
// translates here via FromConfig. Defaults and validation belong to
// the config package; runtime fields (Kubeconfig path) are filled
// from the environment here.
type Config struct {
	Name         string
	DiskSize     string
	Memory       string
	CPUs         string
	SSHPort      string
	PortForwards []PortForward
	Context      string
	CacheDir     string
	Kubeconfig   string
	K3s          K3s
	Registries   config.Registries
	Gateway      config.GatewayConfig
	Storage      config.StorageConfig

	// DataDisk is the operator-owned external qcow2 attached as a
	// labeled `y-cluster-data` volume at /data/yolean. Empty means
	// "no external data disk; /data/yolean lives on the boot disk".
	// Provision creates the file if missing; Teardown leaves it.
	DataDisk     string
	DataDiskSize string
}

// K3s carries the runtime view of K3sConfig: which version to
// install and which install method. The container image is not
// modelled here because qemu installs k3s via the upstream binary +
// airgap tarball from GitHub releases, not from a container
// registry. The docker provisioner derives its image from Version
// at provision time (pkg/provision/docker.ResolveImage).
type K3s struct {
	Version string
	Install string
}

// preflightHostPorts gathers every host port the qemu Provision
// will bind: the SSH forward plus every PortForward.Host entry.
// Empty Host entries are skipped (qemu picks via SLIRP).
func preflightHostPorts(c Config) []string {
	ports := make([]string, 0, 1+len(c.PortForwards))
	if c.SSHPort != "" {
		ports = append(ports, c.SSHPort)
	}
	for _, pf := range c.PortForwards {
		if pf.Host != "" {
			ports = append(ports, pf.Host)
		}
	}
	return ports
}

// hostAPIPort scans the configured port forwards and returns the
// host-side port that maps to guest 6443. Empty string means no
// such forward is configured -- in which case Provision can't
// reach the k3s API from the host and aborts.
func (c Config) hostAPIPort() string {
	for _, pf := range c.PortForwards {
		if pf.Guest == "6443" {
			return pf.Host
		}
	}
	return ""
}

// hostRoutableIP returns the host-side IP at which the host reaches
// the cluster's HTTP ingress. Same derivation as
// config.CommonConfig.HostRoutableIP -- duplicated here because the
// runtime Config already carries a translated PortForwards slice
// and would otherwise need a back-reference to the on-disk config.
// Empty string means no host-side override; the call site uses it
// as the DNSHintIP option, which an empty value omits.
func (c Config) hostRoutableIP() string {
	for _, pf := range c.PortForwards {
		if pf.Guest == "80" {
			return "127.0.0.1"
		}
	}
	return ""
}

// FromConfig translates the on-disk QEMUConfig (already
// defaults-applied and validated by configfile.Load) into the
// runtime Config consumed by Provision/Teardown.
//
// CacheDir defaults here rather than in config because it depends on
// the runtime user's home directory, which the schema author
// shouldn't have to spell out. PortForwards default to the
// y-cluster convention (6443/80/443) at the config layer
// (CommonConfig.applyCommonDefaults) so both providers share the
// shape; we just translate the slice here.
//
// Kubeconfig is always read from $KUBECONFIG; surfacing it in the
// schema would invite confusion since it's a per-user environmental
// concern, not something a y-cluster-provision.yaml should pin.
func FromConfig(c *config.QEMUConfig) Config {
	cacheDir := c.CacheDir
	if cacheDir == "" {
		home, _ := os.UserHomeDir()
		cacheDir = filepath.Join(home, ".cache", "y-cluster-qemu")
	}
	pfs := make([]PortForward, 0, len(c.PortForwards))
	for _, p := range c.PortForwards {
		pfs = append(pfs, PortForward{Host: p.Host, Guest: p.Guest})
	}
	// Relative DataDisk resolves against the directory the YAML lived
	// in (the same convention configfile.Load applies to other path
	// fields). An absolute path passes through.
	dataDisk := c.DataDisk
	if dataDisk != "" && !filepath.IsAbs(dataDisk) && c.Dir != "" {
		dataDisk = filepath.Join(c.Dir, dataDisk)
	}
	dataDiskSize := c.DataDiskSize
	if dataDisk != "" && dataDiskSize == "" {
		dataDiskSize = "10G"
	}
	return Config{
		Name:         c.Name,
		DiskSize:     c.DiskSize,
		Memory:       c.Memory,
		CPUs:         c.CPUs,
		SSHPort:      c.SSHPort,
		PortForwards: pfs,
		Context:      c.Context,
		CacheDir:     cacheDir,
		Kubeconfig:   os.Getenv("KUBECONFIG"),
		K3s: K3s{
			Version: c.K3s.Version,
			Install: c.K3s.Install,
		},
		Registries:   c.Registries,
		Gateway:      c.Gateway,
		Storage:      c.Storage,
		DataDisk:     dataDisk,
		DataDiskSize: dataDiskSize,
	}
}

// Cluster represents a running QEMU-based k3s cluster.
type Cluster struct {
	cfg        Config
	sshKey     string
	pidFile    string
	logger     *zap.Logger
	Kubeconfig *kubeconfig.Manager
	// extraDisks is appended after boot+seed in startVM. Set by
	// Provision when Config.DataDisk is configured (production
	// code path for the disk-reuse primitive), and by
	// StartForDiagnosticWithDisks for the e2e tests that
	// exercise the appliance's pre-baked LABEL fstab.
	extraDisks []string
}

// CheckPrerequisites verifies that required binaries and /dev/kvm exist.
func CheckPrerequisites() error {
	for _, bin := range []string{"qemu-system-x86_64", "qemu-img", "cloud-localds"} {
		if _, err := exec.LookPath(bin); err != nil {
			return fmt.Errorf("missing %s: install qemu-system-x86 qemu-utils cloud-image-utils", bin)
		}
	}
	if _, err := os.Stat("/dev/kvm"); err != nil {
		return fmt.Errorf("/dev/kvm not found: KVM not available")
	}
	return nil
}

// IsRunning checks if a VM with this config is already running.
func (c Config) IsRunning() (bool, int) {
	pidFile := filepath.Join(c.CacheDir, c.Name+".pid")
	data, err := os.ReadFile(pidFile)
	if err != nil {
		return false, 0
	}
	var pid int
	if _, err := fmt.Sscanf(strings.TrimSpace(string(data)), "%d", &pid); err != nil {
		return false, 0
	}
	if !pidAlive(pid) {
		return false, 0
	}
	return true, pid
}

// Provision creates and starts a QEMU VM with k3s installed.
func Provision(ctx context.Context, cfg Config, logger *zap.Logger) (*Cluster, error) {
	// Cross-provisioner preflight: every host port we'll bind is
	// free, and the kubeconfig context isn't pointing at a
	// different cluster (which a second-cluster mistake would
	// silently clobber). Fail with the full list of conflicts so
	// the user fixes them in one config edit, not three.
	pf := provision.Preflight{
		HostPorts:      preflightHostPorts(cfg),
		ContextName:    cfg.Context,
		ContextCluster: clusterName(cfg.Name),
		KubeconfigPath: cfg.Kubeconfig,
	}
	if err := pf.Run(); err != nil {
		return nil, err
	}

	// Initialize kubeconfig manager early — validates KUBECONFIG env
	kubecfg, err := kubeconfig.New(cfg.Context, clusterName(cfg.Name), logger)
	if err != nil {
		return nil, err
	}

	if running, pid := cfg.IsRunning(); running {
		return nil, fmt.Errorf("VM already running (pid %d). Teardown first", pid)
	}

	// Clean up stale entries from previous provisions
	kubecfg.CleanupStale()

	if err := os.MkdirAll(cfg.CacheDir, 0o755); err != nil {
		return nil, fmt.Errorf("create cache dir: %w", err)
	}

	c := &Cluster{
		cfg:        cfg,
		sshKey:     filepath.Join(cfg.CacheDir, cfg.Name+"-ssh"),
		pidFile:    filepath.Join(cfg.CacheDir, cfg.Name+".pid"),
		logger:     logger,
		Kubeconfig: kubecfg,
	}

	// Two provision shapes:
	//
	//   - Fresh: <CacheDir>/<Name>.qcow2 does not exist. Build a
	//     new boot disk from the upstream cloud image, install
	//     k3s + local-path-provisioner + envoy-gateway on it,
	//     extract a kubeconfig.
	//
	//   - Staged-disk: <CacheDir>/<Name>.qcow2 EXISTS. This is
	//     the path `y-cluster import` set up -- a customer-side
	//     boot of a freshly-imported appliance image. k3s and
	//     the in-cluster surface (local-path-provisioner +
	//     envoy-gateway) were baked in by the source appliance
	//     build; this provision only needs to drop a fresh
	//     SSH keypair + cloud-init seed, boot the VM, and
	//     extract the kubeconfig context. Re-installing k3s
	//     here would clobber the appliance's pre-baked state.
	//
	// The staged-disk branch closes the import->boot deadlock
	// (provision used to error "disk already exists; run start"
	// while start errored "no kubeconfig context"). After a
	// successful staged-disk provision, the kubeconfig is
	// populated and subsequent stop/start cycles take the
	// existing-cluster path.
	diskPath := filepath.Join(cfg.CacheDir, cfg.Name+".qcow2")
	stagedDisk := false
	if _, err := os.Stat(diskPath); err == nil {
		stagedDisk = true
		logger.Info("using staged disk from prior import (skipping cloud-image fetch + k3s install)",
			zap.String("disk", diskPath))
	} else {
		cloudImg, err := c.ensureCloudImage(ctx)
		if err != nil {
			return nil, err
		}
		if err := c.ensureDisk(ctx, cloudImg, diskPath); err != nil {
			return nil, err
		}
	}

	// If DataDisk is configured, ensure it exists (creates a
	// labeled qcow2 on first run; reuses an existing one on
	// subsequent runs -- that's the disk-reuse contract). The
	// cloud-init seed below adds the matching fstab entry so the
	// kernel mounts the labeled volume at /data/yolean
	// regardless of whether the boot disk has been through
	// prepare-export.
	if cfg.DataDisk != "" {
		if err := checkDataDiskTools(cfg.DataDisk); err != nil {
			return nil, err
		}
		if err := ensureDataDisk(ctx, cfg.DataDisk, cfg.DataDiskSize, logger); err != nil {
			return nil, err
		}
		c.extraDisks = []string{cfg.DataDisk}
	}

	// Generate a fresh SSH keypair (always; never reused across
	// provisions). The public half lands in the disk via the
	// cloud-init seed below.
	if err := c.ensureSSHKey(); err != nil {
		return nil, err
	}

	seedPath, err := c.createCloudInitSeed()
	if err != nil {
		return nil, err
	}

	// Start VM
	if err := c.startVM(ctx, diskPath, seedPath); err != nil {
		return nil, err
	}

	// Persist launch parameters so `y-cluster start` can re-launch
	// the same shape after a stop. Best-effort: a failed write
	// here doesn't unwind a working Provision; the operator can
	// teardown if they care that the sidecar is missing.
	if err := saveState(cfg); err != nil {
		logger.Warn("could not save state sidecar (start will not work without it)", zap.Error(err))
	}

	// Wait for SSH
	if err := c.waitForSSH(ctx); err != nil {
		return nil, err
	}

	logger.Info("VM ready")

	if stagedDisk {
		// Pre-baked appliance: k3s + addons + registries are
		// already on the disk. Wait for k3s to come up (which
		// it does on its own at boot via systemd), pull its
		// kubeconfig, merge it into the host's, and return.
		// installK3s, localstorage.Install, envoygateway.Install
		// are skipped wholesale -- re-running them here would
		// clobber the appliance's pre-baked state (newer
		// install.yaml apply, racey GatewayClass overwrite,
		// etc.).
		if err := c.waitForK3sReady(ctx); err != nil {
			return nil, fmt.Errorf("staged disk: wait for k3s: %w", err)
		}
		rawKubeconfig, err := c.extractKubeconfig(ctx)
		if err != nil {
			return nil, fmt.Errorf("staged disk: extract kubeconfig: %w", err)
		}
		if err := kubecfg.Import(rawKubeconfig); err != nil {
			return nil, fmt.Errorf("staged disk: merge kubeconfig: %w", err)
		}
		logger.Info("staged disk ready", zap.String("context", cfg.Context))
		return c, nil
	}

	// Stage /etc/rancher/k3s/registries.yaml before installing k3s
	// so containerd reads it on first start. Skipped when the user
	// hasn't configured any mirrors or auth.
	if err := c.writeRegistries(ctx); err != nil {
		return nil, fmt.Errorf("write registries: %w", err)
	}

	// Install k3s. Method (script vs airgap) and version come from
	// the user's QEMUConfig.K3s (defaulted by the config package).
	if cfg.hostAPIPort() == "" {
		return nil, fmt.Errorf("portForwards must include a guest:6443 entry to reach k3s from the host")
	}
	if err := c.installK3s(ctx); err != nil {
		return nil, fmt.Errorf("install k3s: %w", err)
	}

	// Pull kubeconfig out of /etc/rancher/k3s/k3s.yaml, rewrite the
	// server URL to the host-mapped port, and merge into the host's
	// kubeconfig under the configured context name.
	rawKubeconfig, err := c.extractKubeconfig(ctx)
	if err != nil {
		return nil, fmt.Errorf("extract kubeconfig: %w", err)
	}
	if err := kubecfg.Import(rawKubeconfig); err != nil {
		return nil, fmt.Errorf("merge kubeconfig: %w", err)
	}
	logger.Info("k3s ready", zap.String("context", cfg.Context))

	// Install the bundled local-path-provisioner (replaces k3s's
	// disabled local-storage addon). Runs before any workload
	// install so the StorageClass exists when consumer PVCs land.
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
	// default GatewayClass). Replaces the Traefik k3s would
	// otherwise have run; --disable=traefik passed to k3s above
	// keeps that one out of the picture. Skipped wholesale when
	// gateway.skip is set in cluster config -- the cluster comes
	// up with no ingress controller (k3s still has --disable=traefik
	// so traefik doesn't sneak back in).
	if cfg.Gateway.Skip {
		logger.Info("envoy gateway install skipped (gateway.skip)")
	} else {
		if err := envoygateway.Install(ctx, envoygateway.Options{
			ContextName:          cfg.Context,
			GatewayClassName:     cfg.Gateway.ClassName,
			DNSHintIP:            cfg.hostRoutableIP(),
			Logger:               logger,
			ControllerCPURequest: cfg.Gateway.Resources.Controller.CPU,
			ControllerMemRequest: cfg.Gateway.Resources.Controller.Memory,
			ProxyCPURequest:      cfg.Gateway.Resources.Proxy.CPU,
			ProxyMemRequest:      cfg.Gateway.Resources.Proxy.Memory,
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

// Teardown stops the VM and optionally removes the disk.
func (c *Cluster) Teardown(keepDisk bool) error {
	return TeardownConfig(c.cfg, keepDisk, c.logger)
}

// TeardownConfig stops a VM by config without a running Cluster
// instance. Order matters: kill before delete, so a stuck VM
// doesn't leave a stale port-forward bound while we cheerfully
// remove its disk and report success.
func TeardownConfig(cfg Config, keepDisk bool, logger *zap.Logger) error {
	if logger == nil {
		logger = zap.NewNop()
	}

	pidFile := filepath.Join(cfg.CacheDir, cfg.Name+".pid")
	if err := stopVM(pidFile, logger); err != nil {
		// The VM is still alive after SIGKILL. Don't continue with
		// disk delete -- the operator needs to know the previous
		// process is still bound to its port forwards.
		return err
	}

	// Clean kubeconfig — remove context and fix null→[] for kubie
	kubecfg, err := kubeconfig.New(cfg.Context, clusterName(cfg.Name), logger)
	if err == nil {
		kubecfg.CleanupTeardown()
	}

	// Handle per-VM artefacts. keepDisk preserves everything (for
	// export, where the operator wants the qcow2 plus the keypair
	// that authenticates against it). The default removes
	// everything: disk, state sidecar, ssh keypair, cloud-init seed,
	// rendered cloud-init.yaml, and console log. Wiping the keypair
	// is load-bearing -- the next provision generates a fresh one,
	// which is the contract for per-customer appliance delivery (no
	// key reuse across provision runs).
	// Operator-owned DataDisk is preserved unconditionally
	// across teardown -- it lives outside CacheDir and is the
	// whole point of the disk-reuse primitive. Log it so the
	// operator sees the same path they configured will be
	// re-attached on the next provision.
	if cfg.DataDisk != "" {
		if _, err := os.Stat(cfg.DataDisk); err == nil {
			logger.Info("data disk preserved",
				zap.String("path", cfg.DataDisk))
		}
	}

	diskPath := filepath.Join(cfg.CacheDir, cfg.Name+".qcow2")
	if keepDisk {
		logger.Info("teardown complete, disk preserved", zap.String("disk", diskPath))
		return nil
	}
	var removed []string
	for _, p := range perVMArtefacts(cfg.CacheDir, cfg.Name) {
		err := os.Remove(p)
		switch {
		case err == nil:
			removed = append(removed, filepath.Base(p))
		case os.IsNotExist(err):
			// Artefact already gone -- idempotent teardown, no
			// log spam.
		default:
			logger.Warn("teardown could not remove artefact",
				zap.String("path", p), zap.Error(err))
		}
	}
	stateFile := statePath(cfg.CacheDir, cfg.Name)
	hadState := false
	if _, err := os.Stat(stateFile); err == nil {
		hadState = true
	}
	if err := removeState(cfg.CacheDir, cfg.Name); err != nil {
		logger.Warn("teardown could not remove state file", zap.Error(err))
	} else if hadState {
		removed = append(removed, filepath.Base(stateFile))
	}
	if len(removed) == 0 {
		// Nothing to do means the cache dir didn't have anything for
		// this name -- a previous teardown ran, or provision never
		// ran. Either way, lying with a "deleted" log would mask
		// real bugs (e.g. wrong --cacheDir).
		logger.Info("teardown complete, no artefacts found to delete",
			zap.String("cacheDir", cfg.CacheDir),
			zap.String("name", cfg.Name))
	} else {
		logger.Info("teardown complete",
			zap.Strings("removed", removed))
	}
	return nil
}

// perVMArtefacts returns every path Provision or PrepareExport
// creates for a given cluster. Used by TeardownConfig to leave
// the cache dir clean for the next provision -- the keypair in
// particular must go so the per-customer "no key reuse"
// contract holds, and the prepare-export gateway-state JSON
// must go so a stale dump from a prior export doesn't ship in
// the next bundle.
func perVMArtefacts(cacheDir, name string) []string {
	prefix := filepath.Join(cacheDir, name)
	return []string{
		prefix + ".qcow2",
		prefix + "-ssh",
		prefix + "-ssh.pub",
		prefix + "-seed.img",
		prefix + "-cloud-init.yaml",
		prefix + "-console.log",
		// PrepareExport's live phase writes the reconciled
		// Gateway snapshot here; export copies it into
		// BUNDLE_DIR/gateway-state.json. Without this entry,
		// teardown leaves the JSON behind and a subsequent
		// build picks up a stale dump from the prior cluster.
		prefix + "-gateway-state.json",
	}
}

// stopVM ends the qemu process whose pid lives in pidFile,
// then removes the pidfile. The sequence:
//
//  1. read the pidfile (no file == nothing to do)
//  2. SIGTERM, wait up to termGrace for graceful exit
//  3. if still alive, SIGKILL, wait up to killGrace
//  4. error out if it survives both signals -- the operator
//     needs to know rather than continue to disk-delete with a
//     live VM still holding its host-port forwards
//
// Tunables are package-level so unit tests can shorten them.
var (
	termGrace = 10 * time.Second
	killGrace = 5 * time.Second
)

func stopVM(pidFile string, logger *zap.Logger) error {
	if logger == nil {
		logger = zap.NewNop()
	}
	data, err := os.ReadFile(pidFile)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return fmt.Errorf("read %s: %w", pidFile, err)
	}
	var pid int
	if _, err := fmt.Sscanf(strings.TrimSpace(string(data)), "%d", &pid); err != nil {
		// Corrupt pidfile; nothing actionable. Remove it so the
		// next provision starts clean.
		_ = os.Remove(pidFile)
		return nil
	}
	if !pidAlive(pid) {
		_ = os.Remove(pidFile)
		return nil
	}

	logger.Info("stopping VM", zap.Int("pid", pid))
	if err := pidTerminate(pid); err != nil {
		// SIGTERM failure on an alive pid is unusual but not
		// fatal -- we'll try SIGKILL next.
		logger.Warn("SIGTERM failed", zap.Int("pid", pid), zap.Error(err))
	}
	if waitForExit(pid, termGrace) {
		_ = os.Remove(pidFile)
		return nil
	}

	logger.Warn("VM did not stop on SIGTERM, escalating to SIGKILL", zap.Int("pid", pid))
	if err := pidKill(pid); err != nil {
		return fmt.Errorf("kill VM pid %d: %w", pid, err)
	}
	if waitForExit(pid, killGrace) {
		_ = os.Remove(pidFile)
		return nil
	}

	// Pidfile stays so a subsequent teardown can retry.
	return fmt.Errorf("VM pid %d did not exit after SIGKILL", pid)
}

// waitForExit polls pidAlive until it returns false or timeout.
// Returns true when the process exited.
func waitForExit(pid int, timeout time.Duration) bool {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if !pidAlive(pid) {
			return true
		}
		time.Sleep(200 * time.Millisecond)
	}
	return !pidAlive(pid)
}

// SSH runs a command on the VM via SSH.
func (c *Cluster) SSH(ctx context.Context, command string) ([]byte, error) {
	return sshExec(ctx, c.sshKey, c.cfg.SSHPort, command)
}

// SCP copies a local file to the VM.
func (c *Cluster) SCP(ctx context.Context, localPath, remotePath string) error {
	return scpTo(ctx, c.sshKey, c.cfg.SSHPort, localPath, remotePath)
}

// Context returns the kubectl context name the provisioner wrote
// into the host's kubeconfig. Implements provision.Cluster.
func (c *Cluster) Context() string { return c.cfg.Context }

// NodeExec runs a command on the VM, optionally piping stdin into
// the remote process (used by `y-cluster images load` to stream OCI
// tarballs into `ctr image import` on the node). Implements
// provision.Cluster.
func (c *Cluster) NodeExec(ctx context.Context, command string, stdin io.Reader) ([]byte, error) {
	return sshExecStdin(ctx, c.sshKey, c.cfg.SSHPort, command, stdin)
}

// writeRegistries renders the configured registries.yaml and
// stages it on the VM at registries.Path. No-op when the config
// has no mirrors and no auth (the empty case shouldn't write a
// file at all -- containerd then uses its default behaviour).
func (c *Cluster) writeRegistries(ctx context.Context) error {
	body, err := registries.Marshal(c.cfg.Registries)
	if err != nil {
		return err
	}
	if body == nil {
		return nil
	}
	c.logger.Info("writing registries.yaml",
		zap.String("path", registries.Path),
		zap.Int("mirrors", len(c.cfg.Registries.Mirrors)),
		zap.Int("configs", len(c.cfg.Registries.Configs)),
	)
	// install -d creates the dir with the right mode if missing;
	// tee writes the file as root with 0600 since it may carry
	// credentials.
	cmd := "sudo install -d -m 0755 /etc/rancher/k3s && sudo install -m 0600 /dev/stdin " + registries.Path
	out, err := c.NodeExec(ctx, cmd, bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("write %s: %s: %w", registries.Path, out, err)
	}
	return nil
}

// DiskPath returns the path to the VM's disk image.
func (c *Cluster) DiskPath() string {
	return filepath.Join(c.cfg.CacheDir, c.cfg.Name+".qcow2")
}

// Import writes a VM disk to diskPath by converting from a
// supported input format. Format is sniffed by extension on
// inputPath:
//
//   - .vmdk  -> qemu-img convert -f vmdk  -O qcow2 (the original
//              VMware-import path; vmdk subformat doesn't matter
//              because qemu-img -f vmdk auto-detects the variant).
//   - .qcow2 -> qemu-img convert -f qcow2 -O qcow2 (rewrites the
//              qcow2 into the cache layout; usually a quick
//              copy + compaction, no format change).
//
// A local-qemu e2e loop that does `y-cluster export
// --format=qcow2 ... | y-cluster import` doesn't need any
// out-of-band format conversion -- qcow2 is the native format on
// both ends. Other formats (raw, vdi, OVA, gcp-tar) aren't on
// the import path; add them when a real consumer asks.
func Import(inputPath, diskPath string) error {
	if _, err := os.Stat(inputPath); err != nil {
		return fmt.Errorf("input not found: %s", inputPath)
	}
	srcFormat, err := importFormatFromExt(inputPath)
	if err != nil {
		return err
	}
	cmd := exec.Command("qemu-img", "convert",
		"-f", srcFormat, "-O", "qcow2",
		inputPath, diskPath)
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("qemu-img convert: %s: %w", out, err)
	}
	return nil
}

// ImportVMDK is the deprecated alias for Import retained for any
// out-of-tree caller pinned to the old name. Prefer Import.
func ImportVMDK(vmdkPath, diskPath string) error {
	return Import(vmdkPath, diskPath)
}

// importFormatFromExt maps a file extension to the qemu-img `-f`
// argument. Centralised so the supported-set is in one place and a
// new format becomes a one-line table update.
func importFormatFromExt(path string) (string, error) {
	ext := strings.ToLower(filepath.Ext(path))
	switch ext {
	case ".vmdk":
		return "vmdk", nil
	case ".qcow2":
		return "qcow2", nil
	default:
		return "", fmt.Errorf("unsupported import format %q for %s (supported: .vmdk, .qcow2)", ext, path)
	}
}

// --- internal helpers ---

const ubuntuVersion = "noble"

// clusterName derives the kubeconfig cluster entry name from the VM name.
// e.g. "ystack-qemu" → "ystack-qemu"
func clusterName(vmName string) string {
	return vmName
}

func (c *Cluster) ensureCloudImage(ctx context.Context) (string, error) {
	imgPath := filepath.Join(c.cfg.CacheDir, fmt.Sprintf("ubuntu-%s-server-cloudimg-amd64.img", ubuntuVersion))
	if _, err := os.Stat(imgPath); err == nil {
		return imgPath, nil
	}
	c.logger.Info("downloading cloud image", zap.String("version", ubuntuVersion))
	url := fmt.Sprintf("https://cloud-images.ubuntu.com/%s/current/%s-server-cloudimg-amd64.img", ubuntuVersion, ubuntuVersion)
	if err := downloadFile(ctx, url, imgPath); err != nil {
		return "", fmt.Errorf("download cloud image: %w", err)
	}
	return imgPath, nil
}

// ensureDisk creates the per-VM qcow2. Errors when the disk
// already exists -- the per-customer appliance model treats every
// `y-cluster provision` as a fresh build (fresh disk, fresh
// keypair, fresh machine-id once the guest boots). Reusing a
// disk under a new keypair would also leave the old pubkey in
// the disk's authorized_keys, which we have no way to update
// from the host. The operator runs `y-cluster teardown` first
// (or `y-cluster start` to resume an existing one).
func (c *Cluster) ensureDisk(ctx context.Context, cloudImg, diskPath string) error {
	if _, err := os.Stat(diskPath); err == nil {
		return fmt.Errorf(
			"disk %s already exists; run `y-cluster teardown -c %s` to start fresh, or `y-cluster start --context=%s` to resume",
			diskPath, c.cfg.CacheDir, c.cfg.Context)
	}
	c.logger.Info("creating disk", zap.String("size", c.cfg.DiskSize))
	cmd := exec.CommandContext(ctx, "qemu-img", "create",
		"-f", "qcow2", "-b", cloudImg, "-F", "qcow2",
		diskPath, c.cfg.DiskSize)
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("qemu-img create: %s: %w", out, err)
	}
	return nil
}

// ensureSSHKey generates a fresh SSH keypair on every provision.
// y-cluster ships the keypair as part of the per-customer appliance
// handoff; reusing keys across provisions would compromise the
// contract that each shipped appliance authenticates with its own
// keypair. We delete any leftover key first (a clean teardown
// already removed it; this is belt-and-braces against a half-done
// teardown) and regenerate.
func (c *Cluster) ensureSSHKey() error {
	_ = os.Remove(c.sshKey)
	_ = os.Remove(c.sshKey + ".pub")
	return sshexec.GenerateKey(c.sshKey)
}

// renderCloudInitUserData returns the #cloud-config user-data the
// qemu provisioner writes into the seed image. Pulled out as a
// pure function so unit tests can pin the shape without spinning
// up cloud-localds.
//
// The write_files entry drops a cloud-init config snippet onto the
// disk during first boot. It pins datasource_list so that when this
// disk is later exported and re-imported (Hetzner snapshot, VMware
// OVA, dd to bare metal), cloud-init only probes NoCloud (the qemu
// seed shape) and then falls through to None instead of spending
// minutes hammering EC2 IMDS / GCE metadata / OpenStack ConfigDrive
// on hosts that don't provide them. The "no SSH banner" failure
// mode on Hetzner was cloud-init blocking sshd's network ordering;
// this pin prevents the recurrence.
func renderCloudInitUserData(hostname, sshPubKey string, mountDataLabel bool) string {
	// Mount block. When the operator has configured a DataDisk
	// for this VM, the labeled qcow2 is attached as an extra
	// virtio drive and the kernel needs a fstab entry to mount
	// it at the appliance contract path. `nofail` keeps boot
	// moving when the volume is missing (an operator manually
	// detached it for debugging, say) instead of dropping into
	// emergency mode.
	mounts := ""
	if mountDataLabel {
		mounts = `mounts:
  - [ "LABEL=` + DataDiskLabel + `", "/data/yolean", "ext4", "defaults,nofail", "0", "2" ]
`
	}
	return fmt.Sprintf(`#cloud-config
hostname: %s
users:
  - name: ystack
    sudo: ALL=(ALL) NOPASSWD:ALL
    shell: /bin/bash
    ssh_authorized_keys:
      - %s
package_update: false
%swrite_files:
  - path: /etc/cloud/cloud.cfg.d/99-y-cluster-pin.cfg
    permissions: '0644'
    content: |
      # y-cluster: bound cloud-init datasource discovery so a re-imported
      # disk does not stall on EC2 IMDS / GCE metadata probing on hosts
      # that don't provide them. NoCloud covers the qemu seed; None lets
      # cloud-init proceed when no NoCloud source is present.
      datasource_list: [NoCloud, None]
`, hostname, strings.TrimSpace(sshPubKey), mounts)
}

func (c *Cluster) createCloudInitSeed() (string, error) {
	pubKey, err := os.ReadFile(c.sshKey + ".pub")
	if err != nil {
		return "", fmt.Errorf("read SSH public key: %w", err)
	}

	cloudInit := renderCloudInitUserData(c.cfg.Name, string(pubKey), c.cfg.DataDisk != "")

	// Name-prefix the cloud-init source so two concurrent provisions
	// in the same cacheDir don't race on the file. Per-VM artifacts
	// elsewhere (qcow2, pidfile, ssh key, seed image, console log,
	// state sidecar) all follow the same <name>-prefixed convention.
	cloudInitPath := filepath.Join(c.cfg.CacheDir, c.cfg.Name+"-cloud-init.yaml")
	if err := os.WriteFile(cloudInitPath, []byte(cloudInit), 0o644); err != nil {
		return "", err
	}

	seedPath := filepath.Join(c.cfg.CacheDir, c.cfg.Name+"-seed.img")
	cmd := exec.Command("cloud-localds", seedPath, cloudInitPath)
	if out, err := cmd.CombinedOutput(); err != nil {
		return "", fmt.Errorf("cloud-localds: %s: %w", out, err)
	}
	return seedPath, nil
}

func (c *Cluster) startVM(ctx context.Context, diskPath, seedPath string) error {
	c.logger.Info("starting VM",
		zap.String("cpus", c.cfg.CPUs),
		zap.String("memory", c.cfg.Memory+"MB"),
		zap.String("ssh-port", c.cfg.SSHPort),
	)
	consolePath := filepath.Join(c.cfg.CacheDir, c.cfg.Name+"-console.log")
	args := []string{
		"-name", c.cfg.Name,
		"-machine", "accel=kvm",
		"-cpu", "host",
		"-smp", c.cfg.CPUs,
		"-m", c.cfg.Memory,
		"-drive", fmt.Sprintf("file=%s,format=qcow2,if=virtio", diskPath),
	}
	if seedPath != "" {
		args = append(args, "-drive", fmt.Sprintf("file=%s,format=raw,if=virtio", seedPath))
	}
	for _, d := range c.extraDisks {
		args = append(args, "-drive", fmt.Sprintf("file=%s,format=qcow2,if=virtio", d))
	}
	args = append(args,
		"-netdev", c.buildNetdev(),
		"-device", "virtio-net-pci,netdev=net0",
		"-serial", "file:"+consolePath,
		"-display", "none",
		"-daemonize",
		"-pidfile", c.pidFile,
	)
	cmd := exec.CommandContext(ctx, "qemu-system-x86_64", args...)
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("start VM: %s: %w", out, err)
	}
	return nil
}

func (c *Cluster) waitForSSH(ctx context.Context) error {
	c.logger.Info("waiting for SSH")
	deadline := time.Now().Add(120 * time.Second)
	for {
		if _, err := sshExec(ctx, c.sshKey, c.cfg.SSHPort, "true"); err == nil {
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("SSH not available after 120s")
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(2 * time.Second):
		}
	}
}

func (c *Cluster) buildNetdev() string {
	netdev := fmt.Sprintf("user,id=net0,hostfwd=tcp::%s-:22", c.cfg.SSHPort)
	for _, pf := range c.cfg.PortForwards {
		netdev += fmt.Sprintf(",hostfwd=tcp::%s-:%s", pf.Host, pf.Guest)
	}
	return netdev
}

func sshExec(ctx context.Context, keyPath, port, command string) ([]byte, error) {
	return sshexec.Exec(ctx, sshTarget(keyPath, port), command, nil)
}

// sshExecStdin is the same as sshExec but pipes stdin into the
// remote process. Callers that don't need stdin pass nil; callers
// that do (image load streaming a tar archive) supply an io.Reader.
func sshExecStdin(ctx context.Context, keyPath, port, command string, stdin io.Reader) ([]byte, error) {
	return sshexec.Exec(ctx, sshTarget(keyPath, port), command, stdin)
}

func scpTo(ctx context.Context, keyPath, port, localPath, remotePath string) error {
	return sshexec.SCP(ctx, sshTarget(keyPath, port), localPath, remotePath)
}

// sshTarget builds the sshexec.Target the qemu provisioner uses.
// User and host are fixed by the cloud-init template (`ystack`)
// and the host-side port forward (`127.0.0.1:<sshPort>`).
func sshTarget(keyPath, port string) sshexec.Target {
	return sshexec.Target{
		Host:    "127.0.0.1",
		Port:    port,
		User:    "ystack",
		KeyPath: keyPath,
	}
}
