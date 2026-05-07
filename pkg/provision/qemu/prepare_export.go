package qemu

import (
	"context"
	_ "embed"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"

	"go.uber.org/zap"

	"github.com/Yolean/y-cluster/pkg/gateway"
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

// PrepareExport prepares the cluster's qcow2 for shipping as an
// appliance image. Runs in two phases:
//
//   - LIVE phase (cluster running): clears the per-deploy
//     yolean.se/dns-hint-ip GatewayClass annotation so the
//     customer's snapshot doesn't carry our LB IP, then dumps
//     the reconciled Gateway state to <cacheDir>/<name>-
//     gateway-state.json so the bundle ships a record of what
//     the appliance looked like at export time. Then stops
//     the cluster.
//   - OFFLINE phase (cluster stopped): builds the data-seed
//     tarball + runs virt-customize to identity-reset the
//     filesystem, same as the prior behavior.
//
// The same shared inguest script also runs on the Hetzner
// Packer build path (inline, in a live VM); see
// prepareInguestScript above.
//
// VM MUST BE RUNNING when invoked. Earlier versions of
// PrepareExport required the VM to be stopped first (operator
// ran `y-cluster stop && y-cluster prepare-export`). The new
// live-phase steps need the apiserver, so callers should drop
// the explicit `y-cluster stop` -- prepare-export stops the VM
// itself between the live and offline phases. Reordered run:
//
//	y-cluster provision
//	y-cluster prepare-export   # prepare-export now stops internally
//
// Idempotent. A prepared appliance is no longer a usable dev
// cluster locally; the next start runs cloud-init re-init and
// regenerates identity bits. Re-provision (teardown + provision)
// for a fresh dev cluster.
func PrepareExport(ctx context.Context, cacheDir, name string, logger *zap.Logger) error {
	if logger == nil {
		logger = zap.NewNop()
	}
	// Preflight tool checks first: surface "missing tool" errors
	// before doing any cluster-side work that we'd then have to
	// undo. virt-customize is needed in the offline phase;
	// kubectl is needed in the live phase. Both should be
	// addressable by a single `apt install` so it's reasonable
	// to surface either error up front.
	if _, err := exec.LookPath("virt-customize"); err != nil {
		return fmt.Errorf("virt-customize not found in PATH; install with: sudo apt install libguestfs-tools")
	}
	if _, err := exec.LookPath("kubectl"); err != nil {
		return fmt.Errorf("kubectl not found in PATH; install kubectl (prepare-export now snapshots reconciled Gateway state, which needs kubectl)")
	}

	cfg, err := loadState(cacheDir, name)
	if err != nil {
		if os.IsNotExist(err) {
			return fmt.Errorf("no saved state for %q in %s; run `y-cluster provision` first", name, cacheDir)
		}
		return fmt.Errorf("load state: %w", err)
	}
	if running, _ := cfg.IsRunning(); !running {
		return fmt.Errorf("VM %q is not running; start the cluster first (prepare-export now needs the apiserver up to snapshot reconciled Gateway state and clear the per-deploy dns-hint-ip annotation -- it stops the VM internally before the offline phase)", name)
	}
	diskPath := filepath.Join(cfg.CacheDir, cfg.Name+".qcow2")
	if _, err := os.Stat(diskPath); err != nil {
		return fmt.Errorf("disk image not found at %s: %w", diskPath, err)
	}

	// --- LIVE phase ---
	// Clear the per-deploy dns-hint-ip annotation so the snapshot
	// doesn't ship our LB IP. Then dump reconciled gateway state
	// for the bundle. Both steps need the apiserver up.
	logger.Info("clearing yolean.se/dns-hint-ip annotation on GatewayClass",
		zap.String("context", cfg.Context))
	if err := gateway.ClearDNSHintIPAnnotation(ctx, cfg.Context, "y-cluster"); err != nil {
		return fmt.Errorf("clear dns-hint-ip: %w", err)
	}
	gatewayStatePath := filepath.Join(cacheDir, name+"-gateway-state.json")
	logger.Info("snapshotting reconciled gateway state", zap.String("path", gatewayStatePath))
	state, err := gateway.Fetch(ctx, cfg.Context)
	if err != nil {
		return fmt.Errorf("fetch gateway state: %w", err)
	}
	stateJSON, err := json.MarshalIndent(state, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal gateway state: %w", err)
	}
	if err := os.WriteFile(gatewayStatePath, append(stateJSON, '\n'), 0o644); err != nil {
		return fmt.Errorf("write gateway state: %w", err)
	}

	// Stop the VM. virt-customize (offline phase) needs the disk
	// not in use by qemu; libguestfs does its own loopback mount.
	logger.Info("stopping VM before offline phase", zap.String("name", name))
	if err := Stop(cacheDir, name, logger); err != nil {
		return fmt.Errorf("stop VM: %w", err)
	}

	// --- OFFLINE phase ---

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
