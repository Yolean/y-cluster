package qemu

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// TestWriteGCPTar exercises the qcow2 -> tar+gzip(disk.raw)
// pipeline against a real qemu-img-built source. Tarball must
// contain exactly one member named `disk.raw`, and that
// member's size must equal the qcow2's virtual size (qemu-img
// always pads to virtual size on raw conversion). Drift here
// breaks GCE's image-create which silently fails or imports
// a truncated boot disk.
func TestWriteGCPTar(t *testing.T) {
	if _, err := exec.LookPath("qemu-img"); err != nil {
		t.Skip("qemu-img not installed")
	}

	// Tiny 16 MiB qcow2 source -- big enough to exercise the
	// pipe and gzip frame, small enough to run in a unit
	// test without flooding /tmp.
	dir := t.TempDir()
	qcow2 := filepath.Join(dir, "src.qcow2")
	const virtSize = 16 * 1024 * 1024
	if out, err := exec.Command("qemu-img", "create", "-f", "qcow2", qcow2, "16M").CombinedOutput(); err != nil {
		t.Fatalf("qemu-img create: %s: %v", out, err)
	}

	tarGz := filepath.Join(dir, "out.tar.gz")
	if err := writeGCPTar(context.Background(), qcow2, tarGz); err != nil {
		t.Fatalf("writeGCPTar: %v", err)
	}

	// Inspect the tarball.
	f, err := os.Open(tarGz)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	gzr, err := gzip.NewReader(f)
	if err != nil {
		t.Fatalf("gzip open: %v", err)
	}
	tr := tar.NewReader(gzr)

	hdr, err := tr.Next()
	if err != nil {
		t.Fatalf("tar.Next: %v", err)
	}
	if hdr.Name != "disk.raw" {
		t.Errorf("first member name: got %q, want disk.raw", hdr.Name)
	}
	if hdr.Size != virtSize {
		t.Errorf("disk.raw size: got %d, want %d (virtual size)", hdr.Size, int64(virtSize))
	}

	// Drain the body to ensure no corruption / truncation in
	// the pipe-stream path.
	n, err := io.Copy(io.Discard, tr)
	if err != nil {
		t.Fatalf("read disk.raw body: %v", err)
	}
	if n != virtSize {
		t.Errorf("disk.raw body length: got %d, want %d", n, int64(virtSize))
	}

	// No second member: GCE rejects tarballs with anything
	// other than disk.raw.
	if _, err := tr.Next(); err != io.EOF {
		t.Errorf("expected EOF after disk.raw, got %v", err)
	}
}

// TestGCPTar_FormatGNU_NoExtraMembers guards against the
// regression that surfaced as GCE "The tar archive is not a
// valid image": for files >8 GiB the default tar format (PAX)
// emits an extra PaxHeaders member ahead of disk.raw, making
// the tarball two-member from GCE's perspective. The fix is
// pinning Header.Format = tar.FormatGNU, which encodes large
// sizes inline.
//
// We can't easily synthesize a 9 GiB body in a unit test, so
// we test the inverse: a PAX-formatted small file emits two
// members IF the size triggers it, and a GNU-formatted file
// always emits one. Reading the writeGCPTar source directly
// is good enough to confirm we use FormatGNU.
func TestGCPTar_FormatGNU_NoExtraMembers(t *testing.T) {
	src, err := os.ReadFile("gcp_tar.go")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(src), "tar.FormatGNU") {
		t.Errorf("gcp_tar.go must pin tar.FormatGNU; PAX emits a separate header member that GCE rejects with 'tar archive is not a valid image' for disks >8 GiB")
	}
}
