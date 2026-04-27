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
		Registries: c.Registries,
	}
}

// Cluster represents a running QEMU-based k3s cluster.
type Cluster struct {
	cfg        Config
	sshKey     string
	pidFile    string
	logger     *zap.Logger
	Kubeconfig *kubeconfig.Manager
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

	// Download cloud image
	cloudImg, err := c.ensureCloudImage(ctx)
	if err != nil {
		return nil, err
	}

	// Create disk from cloud image (or reuse existing)
	diskPath := filepath.Join(cfg.CacheDir, cfg.Name+".qcow2")
	diskReused := diskExisted(diskPath)
	if err := c.ensureDisk(ctx, cloudImg, diskPath); err != nil {
		return nil, err
	}

	// Generate SSH key
	if err := c.ensureSSHKey(); err != nil {
		return nil, err
	}

	// Create cloud-init seed (only needed for first boot)
	var seedPath string
	if !diskReused {
		var err error
		seedPath, err = c.createCloudInitSeed()
		if err != nil {
			return nil, err
		}
	}

	// Start VM
	if err := c.startVM(ctx, diskPath, seedPath); err != nil {
		return nil, err
	}

	// Wait for SSH
	if err := c.waitForSSH(ctx); err != nil {
		return nil, err
	}

	logger.Info("VM ready")

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

	// Install the bundled Envoy Gateway (CRDs + controller +
	// default GatewayClass). Replaces the Traefik k3s would
	// otherwise have run; --disable=traefik passed to k3s above
	// keeps that one out of the picture.
	if err := envoygateway.Install(ctx, envoygateway.Options{
		ContextName: cfg.Context,
		Logger:      logger,
	}); err != nil {
		return nil, fmt.Errorf("install envoy gateway: %w", err)
	}
	logger.Info("envoy gateway ready", zap.String("version", envoygateway.Version))

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

	// Handle disk
	diskPath := filepath.Join(cfg.CacheDir, cfg.Name+".qcow2")
	if keepDisk {
		logger.Info("teardown complete, disk preserved", zap.String("disk", diskPath))
	} else {
		os.Remove(diskPath)
		logger.Info("teardown complete, disk deleted")
	}
	return nil
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

// ExportVMDK converts the disk image to a streamOptimized VMDK.
func ExportVMDK(diskPath, outputPath string) error {
	if _, err := os.Stat(diskPath); err != nil {
		return fmt.Errorf("disk not found: %s", diskPath)
	}
	cmd := exec.Command("qemu-img", "convert",
		"-f", "qcow2", "-O", "vmdk",
		"-o", "subformat=streamOptimized",
		diskPath, outputPath)
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("qemu-img convert: %s: %w", out, err)
	}
	return nil
}

// ImportVMDK converts a VMDK to qcow2 for use as a VM disk.
func ImportVMDK(vmdkPath, diskPath string) error {
	if _, err := os.Stat(vmdkPath); err != nil {
		return fmt.Errorf("VMDK not found: %s", vmdkPath)
	}
	cmd := exec.Command("qemu-img", "convert",
		"-f", "vmdk", "-O", "qcow2",
		vmdkPath, diskPath)
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("qemu-img convert: %s: %w", out, err)
	}
	return nil
}

// --- internal helpers ---

const ubuntuVersion = "noble"

// clusterName derives the kubeconfig cluster entry name from the VM name.
// e.g. "ystack-qemu" → "ystack-qemu"
func clusterName(vmName string) string {
	return vmName
}

func diskExisted(path string) bool {
	_, err := os.Stat(path)
	return err == nil
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

func (c *Cluster) ensureDisk(ctx context.Context, cloudImg, diskPath string) error {
	if _, err := os.Stat(diskPath); err == nil {
		c.logger.Info("reusing existing disk", zap.String("path", diskPath))
		return nil
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

func (c *Cluster) ensureSSHKey() error {
	if _, err := os.Stat(c.sshKey); err == nil {
		return nil
	}
	return sshexec.GenerateKey(c.sshKey)
}

func (c *Cluster) createCloudInitSeed() (string, error) {
	pubKey, err := os.ReadFile(c.sshKey + ".pub")
	if err != nil {
		return "", fmt.Errorf("read SSH public key: %w", err)
	}

	cloudInit := fmt.Sprintf(`#cloud-config
hostname: %s
users:
  - name: ystack
    sudo: ALL=(ALL) NOPASSWD:ALL
    shell: /bin/bash
    ssh_authorized_keys:
      - %s
package_update: false
`, c.cfg.Name, strings.TrimSpace(string(pubKey)))

	cloudInitPath := filepath.Join(c.cfg.CacheDir, "cloud-init.yaml")
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
