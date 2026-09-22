package qemu

import (
	"archive/tar"
	"context"
	"encoding/xml"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

// ovfDescriptor is the part of the OVF an importer follows to find
// and size the disk.
type ovfDescriptor struct {
	Files []struct {
		Href string `xml:"href,attr"`
		ID   string `xml:"id,attr"`
		Size int64  `xml:"size,attr"`
	} `xml:"References>File"`
	Disks []struct {
		Capacity int64  `xml:"capacity,attr"`
		FileRef  string `xml:"fileRef,attr"`
	} `xml:"DiskSection>Disk"`
}

// TestWriteOVA_DescriptorMatchesArchive builds a real OVA from a small
// qcow2 and reads it the way an importer does: descriptor first, then
// the disk the descriptor points at. renderOVF's own tests pin the
// XML text; this one holds the text against the bytes it describes.
func TestWriteOVA_DescriptorMatchesArchive(t *testing.T) {
	if _, err := exec.LookPath("qemu-img"); err != nil {
		t.Skip("qemu-img not on PATH")
	}
	const virtualSize = 8 << 20
	dir := t.TempDir()
	src := filepath.Join(dir, "src.qcow2")
	if out, err := exec.Command("qemu-img", "create", "-f", "qcow2", src, "8M").CombinedOutput(); err != nil {
		t.Fatalf("qemu-img create: %s: %v", out, err)
	}
	bundle := filepath.Join(dir, "bundle")
	if err := os.Mkdir(bundle, 0o755); err != nil {
		t.Fatal(err)
	}
	ova := filepath.Join(bundle, "acme.ova")
	if err := writeOVA(context.Background(), src, ova, Config{Name: "acme", CPUs: "2", Memory: "4096"}); err != nil {
		t.Fatalf("writeOVA: %v", err)
	}

	// The intermediate VMDK is as large as the deliverable; it must
	// not stay behind in the bundle.
	left, err := os.ReadDir(bundle)
	if err != nil {
		t.Fatal(err)
	}
	if len(left) != 1 || left[0].Name() != "acme.ova" {
		t.Errorf("bundle dir holds more than the OVA: %v", left)
	}

	f, err := os.Open(ova)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = f.Close() }()
	tr := tar.NewReader(f)

	// Member 1: the descriptor. Streaming importers need it before
	// the disk.
	hdr, err := tr.Next()
	if err != nil {
		t.Fatal(err)
	}
	if hdr.Name != "acme.ovf" {
		t.Fatalf("first member is %q, want the descriptor acme.ovf", hdr.Name)
	}
	var ovf ovfDescriptor
	body, err := io.ReadAll(tr)
	if err != nil {
		t.Fatal(err)
	}
	if err := xml.Unmarshal(body, &ovf); err != nil {
		t.Fatalf("descriptor is not well-formed XML: %v\n%s", err, body)
	}
	if len(ovf.Files) != 1 || len(ovf.Disks) != 1 {
		t.Fatalf("want one file and one disk, got %+v", ovf)
	}
	if ovf.Disks[0].FileRef != ovf.Files[0].ID {
		t.Errorf("disk refers to file %q, the file is %q", ovf.Disks[0].FileRef, ovf.Files[0].ID)
	}
	if ovf.Disks[0].Capacity != virtualSize {
		t.Errorf("capacity %d, want the source's virtual size %d", ovf.Disks[0].Capacity, virtualSize)
	}

	// Member 2: the disk, under the name and size the descriptor
	// gave.
	hdr, err = tr.Next()
	if err != nil {
		t.Fatal(err)
	}
	if hdr.Name != ovf.Files[0].Href {
		t.Errorf("second member is %q, the descriptor points at %q", hdr.Name, ovf.Files[0].Href)
	}
	if hdr.Size != ovf.Files[0].Size {
		t.Errorf("disk member is %d bytes, the descriptor says %d", hdr.Size, ovf.Files[0].Size)
	}
	vmdk := filepath.Join(dir, "extracted.vmdk")
	extracted, err := os.Create(vmdk)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := io.Copy(extracted, tr); err != nil {
		t.Fatal(err)
	}
	if err := extracted.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := tr.Next(); !errors.Is(err, io.EOF) {
		t.Errorf("want exactly two members, third: %v", err)
	}

	// The disk converts back with the geometry the guest had: this
	// is `y-cluster import` of what a customer would unpack.
	back := filepath.Join(dir, "back.qcow2")
	if err := Import(vmdk, back); err != nil {
		t.Fatalf("Import of the OVA's disk: %v", err)
	}
	got, err := qemuImgVirtualSize(context.Background(), back)
	if err != nil {
		t.Fatal(err)
	}
	if got != virtualSize {
		t.Errorf("round-tripped disk has virtual size %d, want %d", got, virtualSize)
	}
}
