package qemu

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"

	"go.uber.org/zap"
)

// Format is the on-disk format the customer's bundle ships in.
// The format is required (no default) until we know which one
// becomes the canonical handoff -- different customers will
// import on different hypervisors.
type Format string

const (
	// FormatQcow2 produces a self-contained qcow2 (no backing
	// file). Recommended for QEMU/KVM/libvirt/Proxmox/oVirt.
	FormatQcow2 Format = "qcow2"
	// FormatRaw produces a raw disk image. Universal: dd-able to
	// bare metal, importable by VirtualBox / VMware / most cloud
	// providers' "import disk" flows.
	FormatRaw Format = "raw"
	// FormatVMDK produces a VMDK. The default subformat is
	// streamOptimized, which ESXi accepts natively via the
	// datastore browser and VMware Workstation imports via
	// "Open" / attach-existing-disk. Operators targeting
	// VirtualBox should pass --vmdk-subformat=monolithicSparse
	// (VirtualBox's "Use Existing Virtual Hard Disk File" is
	// happier with monolithicSparse than streamOptimized).
	// All qemu-img VMDK subformats are valid:
	// monolithicSparse, monolithicFlat, twoGbMaxExtentSparse,
	// twoGbMaxExtentFlat, streamOptimized.
	FormatVMDK Format = "vmdk"
	// FormatOVA produces a single .ova file: an uncompressed
	// tar containing an OVF descriptor + a streamOptimized
	// VMDK. VirtualBox accepts it via File -> Import Appliance
	// (which only takes OVF/OVA, not raw VMDK). VMware
	// Workstation / Fusion / ESXi (via vSphere Client) accept
	// it through the same import path. The customer drops the
	// .ova in, picks CPU/RAM/network on the import wizard, and
	// the bundled OVF carries sensible defaults for them.
	FormatOVA Format = "ova"
	// FormatGCPTar produces a gzip-compressed tar containing
	// exactly one member named `disk.raw` -- the on-the-wire
	// shape Google Compute Engine accepts as a custom image
	// source. Upload to GCS, then `gcloud compute images
	// create --source-uri=gs://bucket/<name>.tar.gz` ingests
	// it directly with no managed conversion job. The single
	// member name is mandated by GCE; deviating breaks the
	// import.
	FormatGCPTar Format = "gcp-tar"
)

// VMDKSubformatDefault is the subformat used when --vmdk-subformat
// is not set. streamOptimized is what VMware ESXi expects out of
// the box; we keep it as the default so the historical "y-cluster
// export --format=vmdk" shape still produces an ESXi-importable
// disk.
const VMDKSubformatDefault = "streamOptimized"

// AllVMDKSubformats lists every VMDK subformat qemu-img accepts.
// Used to validate the --vmdk-subformat flag.
func AllVMDKSubformats() []string {
	return []string{
		"streamOptimized",
		"monolithicSparse",
		"monolithicFlat",
		"twoGbMaxExtentSparse",
		"twoGbMaxExtentFlat",
	}
}

// AllFormats lists every Format the export subcommand accepts.
// Used by cobra's flag validation and the "unknown format" error
// message so help output and error stay in sync.
func AllFormats() []string {
	return []string{string(FormatQcow2), string(FormatRaw), string(FormatVMDK), string(FormatOVA), string(FormatGCPTar)}
}

// extensionFor returns the on-disk filename extension for the
// given format. Convention: .qcow2 for qcow2, .img for raw (the
// most widely-recognised raw extension), .vmdk for vmdk, .ova
// for ova.
func extensionFor(f Format) (string, error) {
	switch f {
	case FormatQcow2:
		return ".qcow2", nil
	case FormatRaw:
		return ".img", nil
	case FormatVMDK:
		return ".vmdk", nil
	case FormatOVA:
		return ".ova", nil
	case FormatGCPTar:
		return ".tar.gz", nil
	}
	return "", fmt.Errorf("unsupported format %q (want one of: %v)", f, AllFormats())
}

// qemuImgConvertArgs returns the format-specific args to pass
// after `-f qcow2` and before the source/dest paths. VMDK gets a
// `-o subformat=...` so the resulting file matches the target
// hypervisor's import flow (streamOptimized for ESXi by default,
// monolithicSparse for VirtualBox). vmdkSubformat is ignored for
// non-VMDK formats; an empty value falls back to
// VMDKSubformatDefault.
func qemuImgConvertArgs(f Format, vmdkSubformat string) []string {
	switch f {
	case FormatVMDK:
		if vmdkSubformat == "" {
			vmdkSubformat = VMDKSubformatDefault
		}
		return []string{"-O", "vmdk", "-o", "subformat=" + vmdkSubformat}
	default:
		return []string{"-O", string(f)}
	}
}

// ExportOptions controls Export. Required: CacheDir, Name,
// BundleDir, Format. VMDKSubformat applies only when
// Format=FormatVMDK; empty falls back to VMDKSubformatDefault.
// Logger optional.
type ExportOptions struct {
	CacheDir      string
	Name          string
	BundleDir     string
	Format        Format
	VMDKSubformat string
	Logger        *zap.Logger
}

// Export writes a customer-handoff bundle to opts.BundleDir.
// Layout:
//
//	<BundleDir>/
//	  <name>.qcow2 (or .img)   -- self-contained disk, no backing file
//	  <name>-ssh               -- SSH private key, mode 0600
//	  <name>-ssh.pub           -- SSH public key, mode 0644
//	  README.md                -- boot + ssh instructions
//
// The disk is flattened via `qemu-img convert`, so the bundle has
// no reference to y-cluster's local cloud-image cache. The
// customer can scp the directory to their host and follow
// README.md from there.
//
// Refuses to overwrite a non-empty bundle directory: the customer
// handoff is precious, force the operator to remove the dir
// first if they really mean to re-export. The cluster must be
// stopped (export needs a quiesced disk; running qemu locks the
// qcow2 anyway).
func Export(ctx context.Context, opts ExportOptions) error {
	logger := opts.Logger
	if logger == nil {
		logger = zap.NewNop()
	}

	ext, err := extensionFor(opts.Format)
	if err != nil {
		return err
	}
	if opts.Format == FormatVMDK && opts.VMDKSubformat != "" {
		valid := false
		for _, s := range AllVMDKSubformats() {
			if s == opts.VMDKSubformat {
				valid = true
				break
			}
		}
		if !valid {
			return fmt.Errorf("unsupported vmdk subformat %q (want one of: %v)", opts.VMDKSubformat, AllVMDKSubformats())
		}
	}

	cfg, err := loadState(opts.CacheDir, opts.Name)
	if err != nil {
		if os.IsNotExist(err) {
			return fmt.Errorf("no saved state for %q in %s; run `y-cluster provision` first", opts.Name, opts.CacheDir)
		}
		return fmt.Errorf("load state: %w", err)
	}
	if running, _ := cfg.IsRunning(); running {
		return fmt.Errorf("VM %q is running; run `y-cluster stop` first (export needs a quiesced disk)", opts.Name)
	}

	if entries, err := os.ReadDir(opts.BundleDir); err == nil && len(entries) > 0 {
		return fmt.Errorf("bundle directory %s already has contents; remove it before re-exporting", opts.BundleDir)
	}
	if err := os.MkdirAll(opts.BundleDir, 0o755); err != nil {
		return fmt.Errorf("create bundle dir: %w", err)
	}

	diskSrc := filepath.Join(cfg.CacheDir, cfg.Name+".qcow2")
	if _, err := os.Stat(diskSrc); err != nil {
		return fmt.Errorf("source disk %s not found: %w", diskSrc, err)
	}
	diskDst := filepath.Join(opts.BundleDir, opts.Name+ext)

	logger.Info("converting disk",
		zap.String("format", string(opts.Format)),
		zap.String("src", diskSrc),
		zap.String("dst", diskDst),
	)
	switch opts.Format {
	case FormatOVA:
		// writeOVA owns its own qemu-img convert (always
		// streamOptimized -- the only VMDK subformat the OVF
		// disk-format URI references) plus the OVF descriptor
		// and the ordered tar. Skip the generic convert path
		// below; the .ova IS the disk artefact.
		if err := writeOVA(ctx, diskSrc, diskDst, cfg); err != nil {
			return fmt.Errorf("write ova: %w", err)
		}
	case FormatGCPTar:
		// writeGCPTar pipes qemu-img convert -O raw straight
		// into a gzip writer wrapping a tar writer that emits
		// a single member literally named `disk.raw`. The
		// pipe avoids materialising the full 20 GiB raw
		// expansion on disk.
		if err := writeGCPTar(ctx, diskSrc, diskDst); err != nil {
			return fmt.Errorf("write gcp-tar: %w", err)
		}
	default:
		convertArgs := append([]string{"convert", "-f", "qcow2"}, qemuImgConvertArgs(opts.Format, opts.VMDKSubformat)...)
		convertArgs = append(convertArgs, diskSrc, diskDst)
		cmd := exec.CommandContext(ctx, "qemu-img", convertArgs...)
		if out, err := cmd.CombinedOutput(); err != nil {
			return fmt.Errorf("qemu-img convert: %s: %w", out, err)
		}
	}

	keySrc := filepath.Join(cfg.CacheDir, cfg.Name+"-ssh")
	if err := copyKeyPair(keySrc, filepath.Join(opts.BundleDir, opts.Name+"-ssh")); err != nil {
		return fmt.Errorf("copy keypair: %w", err)
	}

	vmdkSub := opts.VMDKSubformat
	if vmdkSub == "" {
		vmdkSub = VMDKSubformatDefault
	}
	readme := renderBundleReadme(cfg, opts.Format, ext, vmdkSub)
	if err := os.WriteFile(filepath.Join(opts.BundleDir, "README.md"), []byte(readme), 0o644); err != nil {
		return fmt.Errorf("write README: %w", err)
	}

	logger.Info("export complete", zap.String("bundle", opts.BundleDir))
	return nil
}

// copyKeyPair copies <src> -> <dst> at 0600 and <src>.pub ->
// <dst>.pub at 0644.
func copyKeyPair(src, dst string) error {
	if err := copyFile(src, dst, 0o600); err != nil {
		return fmt.Errorf("private key: %w", err)
	}
	if err := copyFile(src+".pub", dst+".pub", 0o644); err != nil {
		return fmt.Errorf("public key: %w", err)
	}
	return nil
}

// copyFile copies src -> dst with the given mode. Truncates dst
// if it exists. The mode is reapplied via Chmod after the write
// because OpenFile's mode arg is masked by umask.
func copyFile(src, dst string, mode os.FileMode) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(dst, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, mode)
	if err != nil {
		return err
	}
	defer out.Close()
	if _, err := io.Copy(out, in); err != nil {
		return err
	}
	return out.Chmod(mode)
}

// renderBundleReadme renders the customer-facing README. Pure
// function so unit tests can pin the rendered shape (boot
// command per format, ssh user, port forwards) without writing
// to disk.
func renderBundleReadme(cfg Config, format Format, ext, vmdkSubformat string) string {
	diskFile := cfg.Name + ext
	keyFile := cfg.Name + "-ssh"

	var bootSection string
	switch format {
	case FormatQcow2:
		bootSection = fmt.Sprintf(`### Boot via QEMU/KVM

	qemu-system-x86_64 \
	    -name %s \
	    -machine accel=kvm -cpu host \
	    -smp %s -m %s \
	    -drive file=%s,format=qcow2,if=virtio \
	    -netdev user,id=n0,hostfwd=tcp::8080-:80,hostfwd=tcp::2222-:22 \
	    -device virtio-net-pci,netdev=n0 \
	    -display none -daemonize \
	    -pidfile %s.pid

Or import the qcow2 into your hypervisor (libvirt / Proxmox /
oVirt all accept qcow2 directly).`,
			cfg.Name, cfg.CPUs, cfg.Memory, diskFile, cfg.Name)
	case FormatVMDK:
		bootSection = fmt.Sprintf(`### Bundled VMDK subformat: %s

(Re-export with `+"`y-cluster export --format=vmdk --vmdk-subformat=...`"+`
to pick a different one. streamOptimized is the ESXi default;
monolithicSparse is the VirtualBox-friendly choice.)

### Import into VMware ESXi

Upload %s into a datastore via the vSphere Client / ESXi Host
Client (Browse Datastore -> Upload), then create a new VM and
attach it as the existing virtual disk.

### Import into VMware Workstation / Fusion

File -> Open -> select %s. Workstation will create a wrapper
VMX around the disk; adjust CPU / memory to match the
original (%s vCPU, %s MiB RAM) and add a NAT or bridged NIC.

### Import into VirtualBox

VirtualBox accepts streamOptimized VMDK via Tools -> Import
Appliance, but in many cases a plain monolithicSparse VMDK
imports more cleanly. Convert if needed:

    qemu-img convert -f vmdk -O vmdk -o subformat=monolithicSparse \
        %s %s

The bundled VMDK is %s.`,
			vmdkSubformat,
			diskFile, diskFile, cfg.CPUs, cfg.Memory,
			diskFile, cfg.Name+"-monolithic.vmdk",
			vmdkSubformat)
	case FormatOVA:
		bootSection = fmt.Sprintf(`### Import into VirtualBox

File -> Import Appliance -> select %s. VirtualBox reads the
embedded OVF descriptor (CPU=%s, RAM=%s MiB, NAT NIC, single
SATA disk) and creates a new VM around the bundled
streamOptimized VMDK.

After import, edit the VM's Network -> Adapter 1 -> Advanced
-> Port Forwarding to add:
  Name=ssh   Protocol=TCP  Host Port=2222  Guest Port=22
  Name=http  Protocol=TCP  Host Port=8080  Guest Port=80
  Name=https Protocol=TCP  Host Port=8443  Guest Port=443

### Import into VMware Workstation / Fusion

File -> Open -> select %s. Workstation parses the OVF and
materialises a wrapper VMX. Adjust port forwarding via the
NAT settings in Edit -> Virtual Network Editor.

### Import into VMware ESXi

vSphere Client -> Deploy OVF Template -> select %s. ESXi
honours the OVF descriptor's CPU / memory hints and the
streamOptimized VMDK is the canonical disk shape for this
import path.

### Inspect / convert

The .ova is a plain (uncompressed) tar of two files; if you
want to crack it open:

    tar tvf %s
    tar xvf %s   # extracts <name>.ovf and <name>.vmdk`,
			diskFile, cfg.CPUs, cfg.Memory,
			diskFile,
			diskFile,
			diskFile, diskFile)
	case FormatRaw:
		bootSection = fmt.Sprintf(`### Boot via QEMU/KVM (universal raw)

	qemu-system-x86_64 \
	    -name %s \
	    -machine accel=kvm -cpu host \
	    -smp %s -m %s \
	    -drive file=%s,format=raw,if=virtio \
	    -netdev user,id=n0,hostfwd=tcp::8080-:80,hostfwd=tcp::2222-:22 \
	    -device virtio-net-pci,netdev=n0 \
	    -display none -daemonize \
	    -pidfile %s.pid

### Or dd to a block device (bare-metal install)

	sudo dd if=%s of=/dev/sdX bs=4M status=progress conv=fsync

(Replace /dev/sdX with the target disk -- DESTRUCTIVE.)

### Or import as a virtual disk

VirtualBox / VMware Workstation / Proxmox accept .img files via
their disk-attach UI; some prefer the .raw extension -- rename
if your hypervisor doesn't recognise .img.`,
			cfg.Name, cfg.CPUs, cfg.Memory, diskFile, cfg.Name, diskFile)
	}

	return fmt.Sprintf(`# y-cluster appliance bundle

Source cluster: %s

## Contents

| File | Purpose |
| ---- | ------- |
| %s | Disk image (%s, self-contained, no external backing file) |
| %s | SSH private key (mode 0600) |
| %s.pub | SSH public key |
| README.md | This file |

## Boot

%s

After boot the VM hosts a single-node Kubernetes cluster (k3s)
with the application preinstalled. The application starts
automatically.

## SSH access

	ssh -i %s -p 2222 ystack@127.0.0.1

(Adjust -p to whatever port you forwarded to guest 22.) The
ystack user has passwordless sudo.

## kubectl access (optional, for inspection)

	ssh -i %s -p 2222 ystack@127.0.0.1 sudo cat /etc/rancher/k3s/k3s.yaml \
	    | sed 's|server: .*|server: https://127.0.0.1:6443|' > k3s.yaml
	KUBECONFIG=k3s.yaml kubectl get nodes

The default boot command above does not forward 6443 -- add
` + "`hostfwd=tcp::6443-:6443`" + ` to the netdev to expose the
apiserver to the host.

## Persistent storage

The appliance ships y-cluster's bundled local-path-provisioner
(replaces k3s's stock local-storage). Stateful workloads with
PersistentVolumeClaims against the default StorageClass
` + "`local-path`" + ` end up under ` + "`/data/yolean/`" + ` on
the appliance disk, named ` + "`<namespace>_<pvc-name>`" + `
(e.g. ` + "`/data/yolean/appliance-stateful_data-versitygw-0/`" + `).
The reclaim policy is ` + "`Retain`" + ` -- a stray
` + "`kubectl delete pvc`" + ` does NOT wipe the data; the
directory persists and the next PVC of the same
namespace+name picks it back up.

This is the appliance-upgrade story: ship a new appliance
disk, the customer's data stays at /data/yolean (whether on
the appliance disk or on a separately-mounted disk; see
below), and re-creating the same PVC names binds the same
data automatically.

### Optional: separate data disk

For workloads that need substantially more storage, or
separation between the OS and data devices, attach a second
virtual disk to the VM and mount it at /data/yolean:

1. Shut the VM down (your hypervisor's ACPI poweroff).
2. Attach a second virtual disk via your hypervisor.
3. Boot the VM. Format the new device (DESTRUCTIVE):

       sudo systemctl stop k3s
       sudo mkfs.ext4 /dev/vdb
       sudo rsync -aAX /data/yolean/ /mnt/new/  # if PVs already exist
       echo '/dev/vdb /data/yolean ext4 defaults,nofail 0 2' \
           | sudo tee -a /etc/fstab
       sudo mount /data/yolean
       sudo systemctl start k3s

4. Existing PVs (named ` + "`<ns>_<pvc-name>`" + `) are now on
   the new disk; PV bindings are unchanged. New PVCs land on
   the same path on the new disk.

### Custom storage path or pattern

The y-cluster-provision.yaml ` + "`storage`" + ` block overrides
all three knobs (path / pathPattern / reclaimPolicy):

    storage:
      path: /mnt/customer-data
      pathPattern: "{{ .PVC.Namespace }}/{{ .PVC.Name }}-{{ .PVName }}"
      reclaimPolicy: Delete

(.PVName expands to ` + "`pvc-<uuid>`" + ` -- the upstream
local-path-provisioner shape, useful when you want unique
per-PV directories that survive PVC delete+recreate without
inheriting the previous PV's data.)

Defaults are picked for the per-customer appliance: predictable
namespace_name path so an upgrade rebinds by name, and Retain
so accidental deletes don't lose data.
`,
		cfg.Name,
		diskFile, format,
		keyFile, keyFile,
		bootSection,
		keyFile, keyFile,
	)
}
