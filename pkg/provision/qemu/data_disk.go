package qemu

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"

	"go.uber.org/zap"
)

// DataDiskLabel is the ext4 filesystem label y-cluster's data
// disk carries. The appliance image's pre-baked
// `LABEL=y-cluster-data /data/yolean ext4 defaults,nofail 0 2`
// fstab entry mounts it; the qemu-provisioned cloud-init also
// adds the same entry when Config.DataDisk is set, so the
// labeled volume mounts even on a non-prepared boot disk.
const DataDiskLabel = "y-cluster-data"

// DataDiskDefaultSize is the size applied to a freshly-created
// DataDisk when QEMUConfig.DataDiskSize is empty. Matches the
// appliance-flow default (GCP_DATADIR_SIZE) so local QA of disk
// reuse uses the same shape the appliance does in cloud.
const DataDiskDefaultSize = "10G"

// ensureDataDisk makes path exist as a qcow2 with an ext4
// filesystem labeled DataDiskLabel. Idempotent on existence:
// if the file is already there, it's left alone (operator-owned
// state, the whole point of this primitive is that it survives
// teardown + re-provision). Only the initial creation runs the
// format step.
//
// Errors after `qemu-img create` succeeds but `virt-format`
// fails roll back the partial file so a re-run starts clean.
//
// Tools required when this fires (caller's check):
//   - qemu-img (always required by qemu provisioner)
//   - virt-format (libguestfs-tools; only needed for fresh
//     DataDisk creation, NOT for reuse)
func ensureDataDisk(ctx context.Context, path, size string, logger *zap.Logger) error {
	if path == "" {
		return fmt.Errorf("ensureDataDisk: path is empty")
	}
	if logger == nil {
		logger = zap.NewNop()
	}
	if _, err := os.Stat(path); err == nil {
		logger.Info("data disk exists, preserving",
			zap.String("path", path))
		return nil
	} else if !os.IsNotExist(err) {
		return fmt.Errorf("stat data disk %s: %w", path, err)
	}
	if size == "" {
		size = DataDiskDefaultSize
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return fmt.Errorf("create data-disk parent dir %s: %w", filepath.Dir(path), err)
	}
	logger.Info("creating data disk",
		zap.String("path", path),
		zap.String("size", size),
		zap.String("label", DataDiskLabel),
	)
	if out, err := exec.CommandContext(ctx, "qemu-img", "create", "-f", "qcow2", path, size).CombinedOutput(); err != nil {
		return fmt.Errorf("qemu-img create %s: %s: %w", path, out, err)
	}
	if out, err := exec.CommandContext(ctx, "virt-format",
		"-a", path,
		"--filesystem=ext4",
		"--label="+DataDiskLabel,
	).CombinedOutput(); err != nil {
		_ = os.Remove(path)
		return fmt.Errorf("virt-format %s: %s: %w", path, out, err)
	}
	return nil
}

// checkDataDiskTools enforces the libguestfs prerequisite that
// only matters when a fresh DataDisk needs creating. Existing
// data disks don't need libguestfs to attach (qemu handles raw
// qcow2 attachment natively).
func checkDataDiskTools(path string) error {
	if _, err := os.Stat(path); err == nil {
		return nil // existing disk, no creation needed
	}
	if _, err := exec.LookPath("virt-format"); err != nil {
		return fmt.Errorf(
			"DataDisk %s does not exist and virt-format is not on PATH; "+
				"install libguestfs-tools to let y-cluster create labeled data disks", path)
	}
	return nil
}
