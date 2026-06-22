package qemu

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestCheckKernelReadable_Readable(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "vmlinuz-test")
	if err := os.WriteFile(p, []byte("kernel"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := checkKernelReadable(p); err != nil {
		t.Fatalf("readable kernel should pass, got: %v", err)
	}
}

func TestCheckKernelReadable_Missing(t *testing.T) {
	// A missing image is not treated as the blocker -- we let
	// libguestfs surface its own error rather than false-positive.
	if err := checkKernelReadable(filepath.Join(t.TempDir(), "nope")); err != nil {
		t.Fatalf("missing kernel should not block, got: %v", err)
	}
}

func TestCheckKernelReadable_PermissionDenied(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root bypasses DAC; permission-denied path is unobservable as root")
	}
	dir := t.TempDir()
	p := filepath.Join(dir, "vmlinuz-locked")
	if err := os.WriteFile(p, []byte("kernel"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(p, 0o000); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(p, 0o644) })

	err := checkKernelReadable(p)
	if err == nil {
		t.Fatal("unreadable kernel should return an actionable error")
	}
	msg := err.Error()
	// The remediation must point at the durable postinst.d hook.
	for _, want := range []string{p, "/etc/kernel/postinst.d/", "supermin", "chmod 0644 /boot/vmlinuz-*"} {
		if !strings.Contains(msg, want) {
			t.Errorf("error message missing %q; got:\n%s", want, msg)
		}
	}
}

func TestRunningKernelRelease(t *testing.T) {
	// On the Linux CI/dev host this should resolve; if /proc is
	// unavailable the function returns ok=false and callers no-op.
	rel, ok := runningKernelRelease()
	if ok && strings.TrimSpace(rel) == "" {
		t.Fatal("ok=true but release is empty")
	}
}
