package qemu

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestAllFormats pins the formats the export subcommand accepts.
// Adding qcow2 / raw to AllFormats but forgetting to wire them
// through extensionFor would break Export silently otherwise.
func TestAllFormats(t *testing.T) {
	formats := AllFormats()
	for _, f := range formats {
		if _, err := extensionFor(Format(f)); err != nil {
			t.Errorf("AllFormats lists %q but extensionFor rejects it: %v", f, err)
		}
	}
}

// TestExtensionFor pins the on-disk filename extensions per
// format.
func TestExtensionFor(t *testing.T) {
	cases := []struct {
		f    Format
		want string
	}{
		{FormatQcow2, ".qcow2"},
		{FormatRaw, ".img"},
		{FormatVMDK, ".vmdk"},
		{FormatOVA, ".ova"},
		{FormatGCPTar, ".tar.gz"},
	}
	for _, c := range cases {
		got, err := extensionFor(c.f)
		if err != nil {
			t.Errorf("extensionFor(%q): %v", c.f, err)
			continue
		}
		if got != c.want {
			t.Errorf("extensionFor(%q): got %q, want %q", c.f, got, c.want)
		}
	}
	if _, err := extensionFor(Format("vhdx")); err == nil {
		t.Errorf("extensionFor(vhdx) should error until VHDX is wired up")
	}
}

// TestQemuImgConvertArgs pins the per-format qemu-img convert
// shape. VMDK defaults to `subformat=streamOptimized` (ESXi);
// an explicit subformat overrides it. Raw / qcow2 take no
// per-format options. Drift here would silently produce a VMDK
// the target hypervisor can't import.
func TestQemuImgConvertArgs(t *testing.T) {
	cases := []struct {
		name      string
		f         Format
		subformat string
		want      []string
	}{
		{"qcow2", FormatQcow2, "", []string{"-O", "qcow2"}},
		{"raw", FormatRaw, "", []string{"-O", "raw"}},
		{"vmdk default", FormatVMDK, "", []string{"-O", "vmdk", "-o", "subformat=streamOptimized"}},
		{"vmdk monolithicSparse", FormatVMDK, "monolithicSparse", []string{"-O", "vmdk", "-o", "subformat=monolithicSparse"}},
		{"vmdk subformat ignored for non-vmdk", FormatRaw, "monolithicSparse", []string{"-O", "raw"}},
	}
	for _, c := range cases {
		got := qemuImgConvertArgs(c.f, c.subformat)
		if len(got) != len(c.want) {
			t.Errorf("%s: got %v, want %v", c.name, got, c.want)
			continue
		}
		for i := range c.want {
			if got[i] != c.want[i] {
				t.Errorf("%s [%d]: got %q, want %q", c.name, i, got[i], c.want[i])
			}
		}
	}
}

// TestExport_RejectsUnknownVMDKSubformat surfaces a friendly
// error for typos. Only fires for FormatVMDK; other formats
// ignore the field entirely.
func TestExport_RejectsUnknownVMDKSubformat(t *testing.T) {
	cacheDir := t.TempDir()
	cfg := defaultedRuntimeConfig(t)
	cfg.CacheDir = cacheDir
	if err := saveState(cfg); err != nil {
		t.Fatal(err)
	}
	err := Export(context.Background(), ExportOptions{
		CacheDir:      cacheDir,
		Name:          cfg.Name,
		BundleDir:     filepath.Join(t.TempDir(), "bundle"),
		Format:        FormatVMDK,
		VMDKSubformat: "monolithic-sparse",
	})
	if err == nil {
		t.Fatal("expected error for unsupported vmdk subformat")
	}
	if !strings.Contains(err.Error(), "unsupported vmdk subformat") {
		t.Errorf("error should mention unsupported subformat: %v", err)
	}
}

// TestExport_RejectsUnknownFormat surfaces the friendly error.
// Uses "vhdx" deliberately -- it's a format we plan to add but
// haven't, so it stays "unknown" until that work lands and is a
// natural canary for "drop the unknown-format error before
// extensionFor knows the new format".
func TestExport_RejectsUnknownFormat(t *testing.T) {
	cacheDir := t.TempDir()
	cfg := defaultedRuntimeConfig(t)
	cfg.CacheDir = cacheDir
	if err := saveState(cfg); err != nil {
		t.Fatal(err)
	}
	err := Export(context.Background(), ExportOptions{
		CacheDir:  cacheDir,
		Name:      cfg.Name,
		BundleDir: filepath.Join(t.TempDir(), "bundle"),
		Format:    Format("vhdx"),
	})
	if err == nil {
		t.Fatal("expected error for unsupported format")
	}
	if !strings.Contains(err.Error(), "unsupported format") {
		t.Errorf("error should mention unsupported format: %v", err)
	}
}

// TestExport_RejectsMissingState exercises the "no saved state"
// branch. Operator should be told to run provision, not get a
// cryptic os.IsNotExist.
func TestExport_RejectsMissingState(t *testing.T) {
	err := Export(context.Background(), ExportOptions{
		CacheDir:  t.TempDir(),
		Name:      "missing",
		BundleDir: filepath.Join(t.TempDir(), "bundle"),
		Format:    FormatQcow2,
	})
	if err == nil {
		t.Fatal("expected error when no saved state exists")
	}
	if !strings.Contains(err.Error(), "y-cluster provision") {
		t.Errorf("error should hint at provision: %v", err)
	}
}

// TestExport_RejectsRunningCluster exercises the IsRunning check.
// Exporting while qemu is alive would race the qcow2 against
// in-flight writes.
func TestExport_RejectsRunningCluster(t *testing.T) {
	cacheDir := t.TempDir()
	cfg := defaultedRuntimeConfig(t)
	cfg.CacheDir = cacheDir
	if err := saveState(cfg); err != nil {
		t.Fatal(err)
	}
	pidFile := filepath.Join(cacheDir, cfg.Name+".pid")
	if err := os.WriteFile(pidFile, []byte("1\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	err := Export(context.Background(), ExportOptions{
		CacheDir:  cacheDir,
		Name:      cfg.Name,
		BundleDir: filepath.Join(t.TempDir(), "bundle"),
		Format:    FormatQcow2,
	})
	if err == nil {
		t.Fatal("expected error when VM still running")
	}
	if !strings.Contains(err.Error(), "y-cluster stop") {
		t.Errorf("error should hint at stop: %v", err)
	}
}

// TestExport_RejectsNonEmptyBundleDir is the precious-handoff
// guard. We never overwrite a customer's bundle silently; force
// the operator to remove the dir first.
func TestExport_RejectsNonEmptyBundleDir(t *testing.T) {
	cacheDir := t.TempDir()
	cfg := defaultedRuntimeConfig(t)
	cfg.CacheDir = cacheDir
	if err := saveState(cfg); err != nil {
		t.Fatal(err)
	}
	bundleDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(bundleDir, "leftover"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	err := Export(context.Background(), ExportOptions{
		CacheDir:  cacheDir,
		Name:      cfg.Name,
		BundleDir: bundleDir,
		Format:    FormatQcow2,
	})
	if err == nil {
		t.Fatal("expected error when bundle dir already has contents")
	}
	if !strings.Contains(err.Error(), "remove it") {
		t.Errorf("error should hint at removing the dir: %v", err)
	}
}

// TestRenderBundleReadme_Qcow2 pins the README's qcow2 boot
// shape: name, file, port forwards, ssh command. Drift here =
// drift in what we tell the customer.
func TestRenderBundleReadme_Qcow2(t *testing.T) {
	cfg := Config{Name: "acme", CPUs: "4", Memory: "8192"}
	body := renderBundleReadme(cfg, FormatQcow2, ".qcow2", "")
	for _, want := range []string{
		"y-cluster appliance bundle",
		"Source cluster: acme",
		"acme.qcow2",
		"acme-ssh",
		"format=qcow2",
		"-smp 4",
		"-m 8192",
		"hostfwd=tcp::8080-:80",
		"hostfwd=tcp::2222-:22",
		"ssh -i acme-ssh -p 2222 ystack@127.0.0.1",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("README missing %q:\n%s", want, body)
		}
	}
}

// TestRenderBundleReadme_Raw pins the raw-format bonus sections
// (dd to /dev/sdX, hypervisor-import notes).
func TestRenderBundleReadme_Raw(t *testing.T) {
	cfg := Config{Name: "acme", CPUs: "2", Memory: "4096"}
	body := renderBundleReadme(cfg, FormatRaw, ".img", "")
	for _, want := range []string{
		"acme.img",
		"format=raw",
		"dd if=acme.img",
		"of=/dev/sdX",
		"DESTRUCTIVE",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("raw README missing %q:\n%s", want, body)
		}
	}
}

// TestRenderBundleReadme_VMDK pins the VMDK-specific guidance
// (ESXi datastore upload, VirtualBox subformat conversion) for
// the default streamOptimized subformat.
func TestRenderBundleReadme_VMDK(t *testing.T) {
	cfg := Config{Name: "acme", CPUs: "2", Memory: "4096"}
	body := renderBundleReadme(cfg, FormatVMDK, ".vmdk", "streamOptimized")
	for _, want := range []string{
		"acme.vmdk",
		"VMware ESXi",
		"datastore",
		"VMware Workstation",
		"VirtualBox",
		"streamOptimized",
		"monolithicSparse",
		"Bundled VMDK subformat: streamOptimized",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("vmdk README missing %q:\n%s", want, body)
		}
	}
}

// TestRenderBundleReadme_VMDKMonolithicSparse pins that the
// README reflects the actual bundled subformat, not just the
// ESXi default.
func TestRenderBundleReadme_VMDKMonolithicSparse(t *testing.T) {
	cfg := Config{Name: "acme", CPUs: "2", Memory: "4096"}
	body := renderBundleReadme(cfg, FormatVMDK, ".vmdk", "monolithicSparse")
	if !strings.Contains(body, "Bundled VMDK subformat: monolithicSparse") {
		t.Errorf("README should report bundled subformat=monolithicSparse:\n%s", body)
	}
}

// TestRenderBundleReadme_OVA pins the OVA-format guidance:
// File -> Import Appliance is the only path that works
// (VirtualBox refuses raw VMDK via that wizard) and the
// per-hypervisor sections must mention CPU/RAM hints baked
// into the OVF descriptor.
func TestRenderBundleReadme_OVA(t *testing.T) {
	cfg := Config{Name: "acme", CPUs: "2", Memory: "4096"}
	body := renderBundleReadme(cfg, FormatOVA, ".ova", "")
	for _, want := range []string{
		"acme.ova",
		"VirtualBox",
		"File -> Import Appliance",
		"Port Forwarding",
		"VMware Workstation",
		"VMware ESXi",
		"OVF",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("ova README missing %q:\n%s", want, body)
		}
	}
}

// TestRenderOVF pins the OVF descriptor shape: descriptor
// references the .vmdk by name, capacity matches the qcow2
// virtual size, CPU/RAM come from the cluster config, and the
// VirtualSystemType is virtualbox-2.2 (the value VirtualBox
// honours; VMware ignores it). Drift here = customer can't
// import the OVA cleanly.
func TestRenderOVF(t *testing.T) {
	cfg := Config{Name: "acme", CPUs: "2", Memory: "4096"}
	body := renderOVF(cfg, 21474836480, 1500000000)
	for _, want := range []string{
		`ovf:href="acme.vmdk"`,
		`ovf:size="1500000000"`,
		`ovf:capacity="21474836480"`,
		`http://www.vmware.com/interfaces/specifications/vmdk.html#streamOptimized`,
		`<Name>acme</Name>`,
		`virtualbox-2.2`,
		`<rasd:VirtualQuantity>2</rasd:VirtualQuantity>`,    // CPU
		`<rasd:VirtualQuantity>4096</rasd:VirtualQuantity>`, // memory
	} {
		if !strings.Contains(body, want) {
			t.Errorf("OVF missing %q:\n%s", want, body)
		}
	}
}

// TestRenderOVF_EscapesName guards against an operator-supplied
// cluster name with XML-special chars breaking the descriptor
// (the cobra layer rejects most of these but defense in depth
// is cheap and matches what html.EscapeString gives us).
func TestRenderOVF_EscapesName(t *testing.T) {
	cfg := Config{Name: "a&b<c>", CPUs: "2", Memory: "4096"}
	body := renderOVF(cfg, 1, 1)
	if !strings.Contains(body, "a&amp;b&lt;c&gt;") {
		t.Errorf("OVF should XML-escape cluster name, got:\n%s", body)
	}
	if strings.Contains(body, "a&b<c>") {
		t.Errorf("OVF still contains raw special chars:\n%s", body)
	}
}
