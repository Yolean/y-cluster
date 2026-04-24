// Package qemu provides a QEMU/KVM-based Kubernetes cluster provisioner.
//
// It creates an Ubuntu VM using cloud images, installs k3s via SSH,
// configures registry mirrors, and extracts a kubeconfig for kubectl.
//
// Prerequisites: qemu-system-x86_64, qemu-img, cloud-localds, /dev/kvm
package qemu

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"go.uber.org/zap"

	"github.com/Yolean/y-cluster/pkg/kubeconfig"
)

// PortForward maps a host port to a guest port.
type PortForward struct {
	Host  string // host port (empty = auto)
	Guest string // guest port
}

// Config holds the VM and cluster configuration.
type Config struct {
	Name         string        // VM name (default: ystack-qemu)
	DiskSize     string        // qcow2 disk size (default: 40G)
	Memory       string        // RAM in MB (default: 8192)
	CPUs         string        // vCPU count (default: 4)
	SSHPort      string        // host port forwarded to VM SSH (default: 2222)
	PortForwards []PortForward // additional port forwards beyond SSH
	Context      string        // kubeconfig context name (default: local)
	CacheDir     string        // directory for VM disk and cloud image cache
	Kubeconfig   string        // path to kubeconfig file
}

// DefaultConfig returns a Config with sensible defaults.
func DefaultConfig() Config {
	home, _ := os.UserHomeDir()
	return Config{
		Name:     "ystack-qemu",
		DiskSize: "40G",
		Memory:   "8192",
		CPUs:     "4",
		SSHPort:  "2222",
		PortForwards: []PortForward{
			{Host: "6443", Guest: "6443"},
			{Host: "80", Guest: "80"},
			{Host: "443", Guest: "443"},
		},
		Context:    "local",
		CacheDir:   filepath.Join(home, ".cache", "ystack-qemu"),
		Kubeconfig: os.Getenv("KUBECONFIG"),
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
	if err := exec.Command("kill", "-0", fmt.Sprintf("%d", pid)).Run(); err != nil {
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
	return c, nil
}

// Teardown stops the VM and optionally removes the disk.
func (c *Cluster) Teardown(keepDisk bool) error {
	return TeardownConfig(c.cfg, keepDisk, c.logger)
}

// TeardownConfig stops a VM by config without a running Cluster instance.
func TeardownConfig(cfg Config, keepDisk bool, logger *zap.Logger) error {
	if logger == nil {
		logger = zap.NewNop()
	}

	// Stop the VM process
	pidFile := filepath.Join(cfg.CacheDir, cfg.Name+".pid")
	data, err := os.ReadFile(pidFile)
	if err == nil {
		var pid int
		if _, err := fmt.Sscanf(strings.TrimSpace(string(data)), "%d", &pid); err == nil {
			if exec.Command("kill", "-0", fmt.Sprintf("%d", pid)).Run() == nil {
				logger.Info("stopping VM", zap.Int("pid", pid))
				if err := exec.Command("kill", fmt.Sprintf("%d", pid)).Run(); err != nil {
					return fmt.Errorf("kill VM pid %d: %w", pid, err)
				}
				deadline := time.Now().Add(10 * time.Second)
				for time.Now().Before(deadline) {
					if exec.Command("kill", "-0", fmt.Sprintf("%d", pid)).Run() != nil {
						break
					}
					time.Sleep(500 * time.Millisecond)
				}
			}
		}
		os.Remove(pidFile)
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

// SSH runs a command on the VM via SSH.
func (c *Cluster) SSH(ctx context.Context, command string) ([]byte, error) {
	return sshExec(ctx, c.sshKey, c.cfg.SSHPort, command)
}

// SCP copies a local file to the VM.
func (c *Cluster) SCP(ctx context.Context, localPath, remotePath string) error {
	return scpTo(ctx, c.sshKey, c.cfg.SSHPort, localPath, remotePath)
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
	cmd := exec.CommandContext(ctx, "curl", "-fSL", "-o", imgPath, url)
	if out, err := cmd.CombinedOutput(); err != nil {
		return "", fmt.Errorf("download cloud image: %s: %w", out, err)
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
	cmd := exec.Command("ssh-keygen", "-t", "ed25519", "-f", c.sshKey, "-N", "", "-q")
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("ssh-keygen: %s: %w", out, err)
	}
	return nil
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
	cmd := exec.CommandContext(ctx, "ssh",
		"-i", keyPath,
		"-o", "StrictHostKeyChecking=no",
		"-o", "UserKnownHostsFile=/dev/null",
		"-o", "LogLevel=ERROR",
		"-p", port,
		"ystack@localhost",
		command,
	)
	return cmd.CombinedOutput()
}

func scpTo(ctx context.Context, keyPath, port, localPath, remotePath string) error {
	cmd := exec.CommandContext(ctx, "scp",
		"-i", keyPath,
		"-o", "StrictHostKeyChecking=no",
		"-o", "UserKnownHostsFile=/dev/null",
		"-o", "LogLevel=ERROR",
		"-P", port,
		localPath,
		"ystack@localhost:"+remotePath,
	)
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("scp: %s: %w", out, err)
	}
	return nil
}
