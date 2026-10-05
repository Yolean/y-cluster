package qemu

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"go.uber.org/zap"
	"sigs.k8s.io/yaml"

	"github.com/Yolean/y-cluster/pkg/provision"
	"github.com/Yolean/y-cluster/pkg/sshexec"
)

// A guest is a VM made the way Provision makes a cluster node -- the
// same disk, seed, network attachment, process handling and teardown
// -- for something that is not a k3s node: no k3s, no kubeconfig
// context, no lifetime timer. The caller adds what the guest is for as
// files and first-boot commands (GuestSpec). `y-cluster dockerhost`
// is the first such caller.

// GuestFile is a file cloud-init writes at first boot, after the
// provider's own write_files entries.
type GuestFile struct {
	Path string `json:"path"`
	// Permissions is an octal mode string such as "0600".
	Permissions string `json:"permissions"`
	Content     string `json:"content"`
}

// GuestSpec is what a caller of ProvisionGuest adds to a plain guest.
type GuestSpec struct {
	// CloudImage is the backing file of the guest's disk, for
	// instance EnsureVerifiedCloudImage's.
	CloudImage string
	Files      []GuestFile
	// RunCmd becomes cloud-init's runcmd: run once, as root, in
	// order, at the end of the first boot.
	RunCmd []string
}

// renderGuestUserData renders spec as an addition to the document
// renderCloudInitUserData returns. That document ends inside its
// write_files list, so spec's files continue the list at the same
// indentation, and runcmd follows as a key of its own. The parts are
// marshalled rather than formatted so file content needs no quoting
// rules of its own; qemu's tests parse the joined document.
func renderGuestUserData(spec GuestSpec) (string, error) {
	var b strings.Builder
	if len(spec.Files) > 0 {
		files, err := yaml.Marshal(spec.Files)
		if err != nil {
			return "", fmt.Errorf("render guest files: %w", err)
		}
		for _, line := range strings.SplitAfter(string(files), "\n") {
			if line != "" {
				b.WriteString("  " + line)
			}
		}
	}
	if len(spec.RunCmd) > 0 {
		runcmd, err := yaml.Marshal(map[string][]string{"runcmd": spec.RunCmd})
		if err != nil {
			return "", fmt.Errorf("render guest runcmd: %w", err)
		}
		b.Write(runcmd)
	}
	return b.String(), nil
}

// ProvisionGuest boots a fresh guest from spec.CloudImage and returns
// once it answers ssh. cloud-init gets the provider's user-data (the
// ystack user with a fresh ssh key, the datasource pin, the static
// network of tap mode) plus spec's files and commands; the caller
// decides when those have finished.
//
// cfg needs Name, CacheDir, DiskSize, Memory, CPUs and a network:
// Tap, or SSHPort and PortForwards on BindAddress. Context and
// Kubeconfig must be empty: a guest has neither. The guest is saved
// like a cluster's VM, so StartGuest boots it again after a stop and
// TeardownConfig removes it. A failure after qemu started leaves the
// VM running for the caller to inspect or tear down.
func ProvisionGuest(ctx context.Context, cfg Config, spec GuestSpec, logger *zap.Logger) (*Cluster, error) {
	if logger == nil {
		logger = zap.NewNop()
	}
	if cfg.Context != "" || cfg.Kubeconfig != "" {
		return nil, fmt.Errorf("guest %q: a guest has no kubeconfig context", cfg.Name)
	}
	if spec.CloudImage == "" {
		return nil, fmt.Errorf("guest %q: no cloud image", cfg.Name)
	}
	pf := provision.Preflight{
		HostPorts:       preflightHostPorts(cfg),
		HostBindAddress: cfg.BindAddress,
		PortBinder:      provision.PortBinderSelf,
	}
	if err := pf.Run(); err != nil {
		return nil, err
	}
	if cfg.Tap != nil {
		if err := checkTap(*cfg.Tap, sysTapHost{}); err != nil {
			return nil, err
		}
	}
	if running, pid := cfg.IsRunning(); running {
		return nil, fmt.Errorf("guest %q already running (pid %d)", cfg.Name, pid)
	}
	if err := os.MkdirAll(cfg.CacheDir, 0o700); err != nil {
		return nil, fmt.Errorf("create cache dir: %w", err)
	}

	c := &Cluster{
		cfg:     cfg,
		sshKey:  filepath.Join(cfg.CacheDir, cfg.Name+"-ssh"),
		pidFile: pidFilePath(cfg.CacheDir, cfg.Name),
		logger:  logger,
		guest:   &spec,
	}
	diskPath := filepath.Join(cfg.CacheDir, cfg.Name+".qcow2")
	if err := c.ensureDisk(ctx, spec.CloudImage, diskPath); err != nil {
		return nil, err
	}
	if err := c.ensureSSHKey(); err != nil {
		return nil, err
	}
	seedPath, err := c.createCloudInitSeed()
	if err != nil {
		return nil, err
	}
	if err := c.startVM(ctx, diskPath, seedPath); err != nil {
		return nil, err
	}
	// StartGuest and the graceful poweroff in TeardownConfig read the
	// launch shape from here.
	if err := saveState(cfg); err != nil {
		return c, fmt.Errorf("save guest state: %w", err)
	}
	if err := c.waitForSSH(ctx); err != nil {
		return c, err
	}
	logger.Info("guest ready", zap.String("name", cfg.Name))
	return c, nil
}

// StartGuest boots a stopped guest from its disk and the state
// ProvisionGuest saved, and returns once it answers ssh. The seed is
// not attached, so nothing of the first boot's cloud-init runs again.
func StartGuest(ctx context.Context, cacheDir, name string, logger *zap.Logger) (*Cluster, error) {
	return bootSaved(ctx, cacheDir, name, nil, false, logger)
}

// GuestPID returns the pid of the guest's qemu when it runs.
func GuestPID(cacheDir, name string) (int, bool) {
	pid, err := readPidFile(pidFilePath(cacheDir, name))
	return pid, err == nil
}

// GuestConfig returns the launch shape ProvisionGuest saved.
func GuestConfig(cacheDir, name string) (Config, error) {
	cfg, err := loadState(cacheDir, name)
	if err != nil {
		return Config{}, err
	}
	// loadState fills Kubeconfig from the environment for cluster
	// nodes; a guest has none.
	cfg.Kubeconfig = ""
	return cfg, nil
}

// GuestExec runs command in a running guest over ssh, as the ystack
// user (which has passwordless sudo), with the key ProvisionGuest made.
func GuestExec(ctx context.Context, cacheDir, name, command string) ([]byte, error) {
	cfg, err := GuestConfig(cacheDir, name)
	if err != nil {
		return nil, err
	}
	key := filepath.Join(cfg.CacheDir, cfg.Name+"-ssh")
	return sshexec.Exec(ctx, cfg.endpoints().sshTarget(key), command, nil)
}

// RemoveGuestUserData deletes the rendered user-data and the seed
// image of a guest whose first boot is over. They hold whatever the
// caller's files held, and nothing reads them again: StartGuest boots
// without the seed, and qemu keeps its own handle on the image for as
// long as the process lives.
func RemoveGuestUserData(cacheDir, name string) error {
	for _, suffix := range []string{"-cloud-init.yaml", "-seed.img"} {
		if err := os.Remove(filepath.Join(cacheDir, name+suffix)); err != nil && !os.IsNotExist(err) {
			return err
		}
	}
	return nil
}
