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

func TestSuperminKernelCandidate_PicksNewestWithModules(t *testing.T) {
	boot := t.TempDir()
	modules := t.TempDir()
	for _, rel := range []string{"6.9.0-99-generic", "6.17.0-14-generic", "6.17.0-40-generic"} {
		if err := os.WriteFile(filepath.Join(boot, "vmlinuz-"+rel), []byte("k"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	// Only the two 6.17 kernels have module dirs; 6.9.0-99 (which a
	// naive string sort would rank highest) must be ignored even if
	// present, and the newest of the eligible ones must win.
	for _, rel := range []string{"6.17.0-14-generic", "6.17.0-40-generic"} {
		if err := os.Mkdir(filepath.Join(modules, rel), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	got, ok := superminKernelCandidate(boot, modules)
	if !ok {
		t.Fatal("expected a candidate")
	}
	if want := filepath.Join(boot, "vmlinuz-6.17.0-40-generic"); got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

func TestSuperminKernelCandidate_NoModulesDirsMeansNoCandidate(t *testing.T) {
	boot := t.TempDir()
	if err := os.WriteFile(filepath.Join(boot, "vmlinuz-6.17.0-14-generic"), []byte("k"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, ok := superminKernelCandidate(boot, t.TempDir()); ok {
		t.Error("kernel without /lib/modules/<release> must not be a candidate")
	}
}

func TestKernelReleaseLess_NumericSegments(t *testing.T) {
	cases := []struct {
		a, b string
		want bool
	}{
		{"6.9.0-99-generic", "6.17.0-14-generic", true},
		{"6.17.0-14-generic", "6.17.0-40-generic", true},
		{"6.17.0-40-generic", "6.17.0-14-generic", false},
		{"6.17.0-14-generic", "6.17.0-14-generic", false},
		{"6.17.0-14", "6.17.0-14-generic", false},
	}
	for _, c := range cases {
		if got := kernelReleaseLess(c.a, c.b); got != c.want {
			t.Errorf("kernelReleaseLess(%q, %q) = %v, want %v", c.a, c.b, got, c.want)
		}
	}
}
