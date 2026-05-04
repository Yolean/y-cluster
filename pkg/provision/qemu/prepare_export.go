package qemu

import (
	"context"
	_ "embed"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"

	"go.uber.org/zap"
)

// prepareInguestScript is the shared identity-reset script. The
// SAME script runs in two contexts:
//
//   - qemu PrepareExport runs it against an offline qcow2 via
//     `virt-customize --run`, which mounts the disk with
//     libguestfs and chroots into it.
//   - The Hetzner Packer build runs it inline via Packer's shell
//     provisioner against the live build VM's filesystem just
//     before snapshot.
//
// One source of truth means the local and Hetzner appliance
// builds end up with the same on-disk state for cloud-init,
// netplan, machine-id, ssh host keys, and friends. See the
// script header for the full list of what's wiped vs kept and
// the reasoning behind each choice.
//
//go:embed prepare_inguest.sh
var prepareInguestScript string

// PrepareExport strips host-specific identity from the offline
// disk image so the same disk boots cleanly when imported on a
// different hypervisor (VMware, KVM, cloud providers). It uses
// libguestfs's virt-customize to mount the qcow2 (no boot, no
// SSH, no host kernel involvement) and run the embedded
// prepare-inguest.sh script inside the chrooted filesystem.
//
// The same script also runs on the Hetzner Packer build path
// (inline, in a live VM); see prepareInguestScript above.
//
// VM must be stopped first; virt-customize refuses to operate
// on a disk in use by a running qemu. Run order:
//
//	y-cluster provision
//	y-cluster stop
//	y-cluster prepare-export
//
// Idempotent. A prepared appliance is no longer a usable dev
// cluster locally; the next start runs cloud-init re-init and
// regenerates identity bits. Re-provision (teardown + provision)
// for a fresh dev cluster.
func PrepareExport(ctx context.Context, cacheDir, name string, logger *zap.Logger) error {
	if logger == nil {
		logger = zap.NewNop()
	}
	cfg, err := loadState(cacheDir, name)
	if err != nil {
		if os.IsNotExist(err) {
			return fmt.Errorf("no saved state for %q in %s; run `y-cluster provision` first", name, cacheDir)
		}
		return fmt.Errorf("load state: %w", err)
	}
	if running, _ := cfg.IsRunning(); running {
		return fmt.Errorf("VM %q is running; run `y-cluster stop` first (virt-customize needs an offline disk)", name)
	}
	if _, err := exec.LookPath("virt-customize"); err != nil {
		return fmt.Errorf("virt-customize not found in PATH; install with: sudo apt install libguestfs-tools")
	}
	diskPath := filepath.Join(cfg.CacheDir, cfg.Name+".qcow2")
	if _, err := os.Stat(diskPath); err != nil {
		return fmt.Errorf("disk image not found at %s: %w", diskPath, err)
	}

	scriptPath, err := WritePrepareInguestScript("")
	if err != nil {
		return fmt.Errorf("write prepare script: %w", err)
	}
	defer os.Remove(scriptPath)

	// Build the seed assets. virt-tar-out is part of the same
	// libguestfs-tools package as virt-customize, so its presence
	// is implied. If the guest has no /data/yolean dir at all
	// (e.g., a build cluster that never ran a workload using the
	// bundled local-path), we WARN and skip the seed step. The
	// systemd unit's ConditionPathExists fires at customer boot
	// and the unit no-ops -- no spurious failures.
	var seed *SeedAssets
	seed, err = BuildSeedAssets(ctx, diskPath, applianceNameFromConfig(cfg))
	if err != nil {
		logger.Warn("seed assets not built; appliance will ship without first-boot seed",
			zap.Error(err))
		seed = nil
	} else {
		logger.Info("data-seed staged",
			zap.String("seed", seed.SeedTarPath),
			zap.String("meta", seed.SeedMetaPath))
		defer os.RemoveAll(seed.TmpDir)
	}

	args := prepareExportArgs(diskPath, scriptPath, seed)
	logger.Info("running virt-customize", zap.String("disk", diskPath), zap.String("script", scriptPath))
	cmd := exec.CommandContext(ctx, "virt-customize", args...)
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("virt-customize: %s: %w", out, err)
	}
	return nil
}

// prepareExportArgs is the canonical virt-customize argv for
// PrepareExport. Pulled out so unit tests can pin its shape
// without spinning up libguestfs.
//
// Order:
//
//  1. --upload / --mkdir / --chmod for seed assets (idempotent
//     filesystem ops; no side effects on the running guest's state)
//  2. --run prepare-inguest.sh (identity reset + manifest staging
//     -> auto-apply move + systemctl enable on the seed unit and
//     timesyncd)
//
// The seed args go FIRST so prepare-inguest can `systemctl enable`
// against units we've already uploaded. virt-customize processes
// flags strictly in order.
func prepareExportArgs(diskPath, scriptPath string, seed *SeedAssets) []string {
	args := []string{"-a", diskPath}
	args = append(args, virtCustomizeArgsForSeed(seed)...)
	args = append(args, "--run", scriptPath)
	return args
}

// WritePrepareInguestScript writes the embedded prepare-inguest
// shell script to a temp file (or a caller-supplied directory if
// dir is non-empty), marks it executable, and returns the path.
// Caller is responsible for removing the file.
//
// Exposed publicly so the Hetzner Packer driver script can also
// emit the script to disk and upload it via Packer's file
// provisioner -- one source of truth across both build paths.
func WritePrepareInguestScript(dir string) (string, error) {
	pattern := "y-cluster-prepare-*.sh"
	var f *os.File
	var err error
	if dir == "" {
		f, err = os.CreateTemp("", pattern)
	} else {
		f, err = os.CreateTemp(dir, pattern)
	}
	if err != nil {
		return "", err
	}
	if _, err := f.WriteString(prepareInguestScript); err != nil {
		f.Close()
		os.Remove(f.Name())
		return "", err
	}
	if err := f.Chmod(0o755); err != nil {
		f.Close()
		os.Remove(f.Name())
		return "", err
	}
	if err := f.Close(); err != nil {
		os.Remove(f.Name())
		return "", err
	}
	return f.Name(), nil
}

// PrepareInguestScript returns the embedded script source. Used
// by tests and by the y-cluster `prepare-script` subcommand
// (consumed by the Hetzner Packer driver).
func PrepareInguestScript() string {
	return prepareInguestScript
}
