//go:build e2e && kvm

package e2e

import (
	"archive/tar"
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"go.uber.org/zap"

	"github.com/Yolean/y-cluster/pkg/provision/qemu"
	"github.com/Yolean/y-cluster/pkg/sshexec"
)

// unpackOVA extracts every member of an OVA into dir and returns the
// path of its .vmdk.
func unpackOVA(t *testing.T, ova, dir string) string {
	t.Helper()
	f, err := os.Open(ova)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = f.Close() }()
	tr := tar.NewReader(f)
	vmdk := ""
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatalf("read %s: %v", ova, err)
		}
		dest := filepath.Join(dir, filepath.Base(hdr.Name))
		out, err := os.Create(dest)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := io.Copy(out, tr); err != nil {
			t.Fatal(err)
		}
		if err := out.Close(); err != nil {
			t.Fatal(err)
		}
		if strings.HasSuffix(dest, ".vmdk") {
			vmdk = dest
		}
	}
	if vmdk == "" {
		t.Fatalf("%s has no .vmdk member", ova)
	}
	return vmdk
}

// TestQemu_OVA_BootsOnForeignHardware takes the OVA deliverable the
// way a customer does: unpack it, give the disk to a hypervisor that
// is not y-cluster, attach a labeled data volume, power on.
//
// Every other e2e boots appliance disks through y-cluster's own qemu
// launch: virtio disk, virtio NIC, the MAC it was built with, a
// cloud-init seed next to it. The OVF describes a SATA controller,
// and VirtualBox / VMware bring their own NIC and MAC and no seed.
// What has to hold there, and is only asserted here:
//
//   - the kernel finds its root filesystem on an AHCI disk
//   - the network comes up on a NIC of another model and MAC
//     (prepare-export's generic netplan, cloud-init kept from
//     regenerating a MAC-pinned one)
//   - the bundle's ssh key still logs in (prepare-export's identity
//     reset left the account alone)
//   - the seed gate accepts the customer's labeled volume and k3s
//     comes up with the build-time data on it
func TestQemu_OVA_BootsOnForeignHardware(t *testing.T) {
	if _, err := os.Stat("/dev/kvm"); err != nil {
		t.Skip("QEMU tests require /dev/kvm")
	}
	if err := qemu.CheckPrerequisites(); err != nil {
		t.Skip(err)
	}
	for _, bin := range []string{"virt-customize", "virt-format"} {
		if _, err := exec.LookPath(bin); err != nil {
			t.Skipf("%s not on PATH; install libguestfs-tools", bin)
		}
	}

	logger, _ := zap.NewDevelopment()
	cfg := e2eQEMURuntime()
	cfg.Name = "y-cluster-e2e-ova"
	cfg.Context = "y-cluster-e2e-ova"
	cfg.CacheDir = e2eQEMUCacheDir(t)
	cfg.Memory = "2048"
	cfg.CPUs = "2"
	cfg.SSHPort = "2238"
	cfg.PortForwards = e2eUniqueForwards("26468", "28468")
	cfg.Kubeconfig = os.Getenv("KUBECONFIG")
	if cfg.Kubeconfig == "" {
		t.Skip("KUBECONFIG must be set")
	}
	// The gateway is not what is under test.
	cfg.Gateway.Skip = true

	ctx := context.Background()

	// Supplier side: build, prepare, export.
	cluster, err := qemu.Provision(ctx, cfg, logger)
	if err != nil {
		t.Fatalf("Provision: %v", err)
	}
	t.Cleanup(func() { _ = qemu.TeardownConfig(cfg, false, logger) })
	if out, err := cluster.SSH(ctx, "sudo mkdir -p /data/yolean && echo built-by-supplier | sudo tee /data/yolean/sentinel.txt >/dev/null"); err != nil {
		t.Fatalf("plant sentinel: %s: %v", out, err)
	}
	if err := qemu.PrepareExport(ctx, cfg.CacheDir, cfg.Name, logger); err != nil {
		t.Fatalf("PrepareExport: %v", err)
	}
	bundle := filepath.Join(t.TempDir(), "bundle")
	if err := qemu.Export(ctx, qemu.ExportOptions{
		CacheDir: cfg.CacheDir, Name: cfg.Name, BundleDir: bundle, Format: qemu.FormatOVA, Logger: logger,
	}); err != nil {
		t.Fatalf("Export: %v", err)
	}

	// Customer side. Nothing below uses the supplier's cache dir, so
	// it goes now: this test holds one multi-GB copy of the disk
	// after another, and each is removed once the next exists.
	if err := qemu.TeardownConfig(cfg, false, logger); err != nil {
		t.Fatalf("teardown of the supplier VM: %v", err)
	}
	customer := t.TempDir()
	ova := filepath.Join(bundle, cfg.Name+".ova")
	vmdk := unpackOVA(t, ova, customer)
	if err := os.Remove(ova); err != nil {
		t.Fatal(err)
	}
	boot := filepath.Join(customer, "boot.qcow2")
	if err := qemu.Import(vmdk, boot); err != nil {
		t.Fatalf("Import of the OVA's disk: %v", err)
	}
	if err := os.Remove(vmdk); err != nil {
		t.Fatal(err)
	}
	data := filepath.Join(customer, "data.qcow2")
	makeLabeledDataDisk(t, data, "y-cluster-data", "1G")

	const sshPort = "2239"
	pidFile := filepath.Join(customer, "vm.pid")
	launch := exec.Command("qemu-system-x86_64",
		"-name", "y-cluster-e2e-ova-customer",
		"-machine", "accel=kvm", "-cpu", "host", "-smp", "2", "-m", "2048",
		// The OVF's storage: one SATA controller, disks on its ports.
		"-device", "ahci,id=sata",
		"-drive", "if=none,id=boot,format=qcow2,file="+boot,
		"-device", "ide-hd,drive=boot,bus=sata.0",
		"-drive", "if=none,id=data,format=qcow2,file="+data,
		"-device", "ide-hd,drive=data,bus=sata.1",
		// VirtualBox's default NIC family, and not the build MAC.
		"-netdev", "user,id=net0,hostfwd=tcp:127.0.0.1:"+sshPort+"-:22",
		"-device", "e1000,netdev=net0,mac=08:00:27:12:34:56",
		"-display", "none",
		"-serial", "file:"+filepath.Join(customer, "console.log"),
		"-daemonize", "-pidfile", pidFile,
	)
	if out, err := launch.CombinedOutput(); err != nil {
		t.Fatalf("launch customer VM: %s: %v", out, err)
	}
	t.Cleanup(func() {
		pidBytes, err := os.ReadFile(pidFile)
		if err != nil {
			return
		}
		if pid, err := strconv.Atoi(strings.TrimSpace(string(pidBytes))); err == nil {
			_ = syscall.Kill(pid, syscall.SIGKILL)
		}
	})

	target := sshexec.Target{Host: "127.0.0.1", Port: sshPort, User: "ystack", KeyPath: filepath.Join(bundle, cfg.Name+"-ssh")}
	run := func(timeout time.Duration, command string) (string, error) {
		cctx, cancel := context.WithTimeout(ctx, timeout)
		defer cancel()
		out, err := sshexec.Exec(cctx, target, command, nil)
		return strings.TrimSpace(string(out)), err
	}
	eventually := func(what string, budget time.Duration, command, want string) {
		t.Helper()
		deadline := time.Now().Add(budget)
		for {
			out, err := run(15*time.Second, command)
			if err == nil && out == want {
				return
			}
			if time.Now().After(deadline) {
				console, _ := os.ReadFile(filepath.Join(customer, "console.log"))
				tail := string(console)
				if len(tail) > 3000 {
					tail = tail[len(tail)-3000:]
				}
				t.Fatalf("%s: not within %s. Last answer %q, err %v\nconsole tail:\n%s", what, budget, out, err, tail)
			}
			time.Sleep(3 * time.Second)
		}
	}

	// Root found on the SATA disk, network up on the e1000, the
	// bundle key accepted: all three, or no answer.
	eventually("ssh login with the bundle key", 5*time.Minute, "echo in", "in")

	if out, err := run(15*time.Second, `lsblk -ndo TRAN "/dev/$(lsblk -ndo PKNAME "$(findmnt -no SOURCE /)")"`); err != nil || out != "sata" {
		t.Errorf("root disk transport = %q (err %v), want sata: the test did not boot the hardware it means to", out, err)
	}
	if out, err := run(15*time.Second, `basename "$(readlink /sys/class/net/$(ip -o route get 10.0.2.2 | sed 's/.* dev \([^ ]*\).*/\1/')/device/driver)"`); err != nil || out != "e1000" {
		t.Errorf("NIC driver = %q (err %v), want e1000", out, err)
	}

	eventually("seed unit active on the customer volume", 2*time.Minute, "systemctl is-active y-cluster-data-seed.service", "active")
	if out, err := run(15*time.Second, "findmnt -no LABEL /data/yolean"); err != nil || out != "y-cluster-data" {
		t.Errorf("/data/yolean is on label %q (err %v), want the customer volume y-cluster-data", out, err)
	}
	if out, err := run(15*time.Second, "cat /data/yolean/sentinel.txt"); err != nil || out != "built-by-supplier" {
		t.Errorf("build-time data on the customer volume: %q (err %v)", out, err)
	}
	eventually("k3s apiserver ready", 4*time.Minute, "sudo k3s kubectl get --raw=/readyz", "ok")
	eventually("node Ready", 2*time.Minute,
		`sudo k3s kubectl get nodes -o jsonpath='{.items[0].status.conditions[?(@.type=="Ready")].status}'`, "True")

	// Power off the way the customer would, so the cleanup SIGKILL
	// has nothing left to kill.
	if out, err := run(15*time.Second, "sudo poweroff"); err != nil {
		t.Logf("poweroff: %s: %v (a dropped connection is expected)", out, err)
	}
	pidBytes, err := os.ReadFile(pidFile)
	if err != nil {
		t.Fatalf("read pidfile: %v", err)
	}
	deadline := time.Now().Add(90 * time.Second)
	for {
		if _, err := os.Stat(fmt.Sprintf("/proc/%s", strings.TrimSpace(string(pidBytes)))); err != nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("customer VM did not power off within 90s")
		}
		time.Sleep(time.Second)
	}
}
