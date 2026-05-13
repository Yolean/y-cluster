package qemu

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

// TestEnsureDataDisk_PreservesExisting pins the disk-reuse
// invariant: a DataDisk that already exists is NOT touched,
// even if its size differs from the configured size. The whole
// reason DataDisk is a separate concept from the boot disk is
// that operators put data on it; reformatting on every provision
// would defeat the purpose.
//
// We seed a sentinel file inside a placeholder qcow2 (using a
// regular file for byte equality; the production path checks
// file existence, not qcow2 validity). Post-call the bytes must
// match.
func TestEnsureDataDisk_PreservesExisting(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "data.qcow2")
	body := []byte("not a real qcow2 -- placeholder for the reuse-preserves-bytes check")
	if err := os.WriteFile(path, body, 0o644); err != nil {
		t.Fatal(err)
	}

	if err := ensureDataDisk(context.Background(), path, "1G", nil); err != nil {
		t.Fatalf("ensureDataDisk on existing file: %v", err)
	}

	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read after ensure: %v", err)
	}
	if string(got) != string(body) {
		t.Errorf("existing file content mutated:\ngot:  %q\nwant: %q", got, body)
	}
}

// TestEnsureDataDisk_EmptyPathRejected protects callers from
// silently creating a qcow2 at the empty path (which would land
// at CWD or worse depending on the running shell). The runtime
// Config translation in FromConfig should already prevent this,
// but a misuse-of-the-helper case is cheap to pin.
func TestEnsureDataDisk_EmptyPathRejected(t *testing.T) {
	if err := ensureDataDisk(context.Background(), "", "1G", nil); err == nil {
		t.Fatal("expected error for empty path")
	}
}

// TestCheckDataDiskTools_NoToolNeededWhenDiskExists pins the
// graceful-degradation contract: an operator on a host without
// libguestfs can still EXERCISE the disk-reuse flow (the
// expensive part) as long as the disk has been created once
// elsewhere -- attaching an existing qcow2 doesn't need
// libguestfs, only the initial format step does.
func TestCheckDataDiskTools_NoToolNeededWhenDiskExists(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "data.qcow2")
	if err := os.WriteFile(path, []byte("placeholder"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := checkDataDiskTools(path); err != nil {
		t.Errorf("expected no tool requirement for an existing disk: %v", err)
	}
}
