package qemu

import (
	"fmt"
	"os"
	"strings"
)

// kernelReadableHookName is the /etc/kernel/postinst.d/ filename the
// remediation suggests. Sorted late (zz-) so it runs after the
// distro hooks that lay the image down.
const kernelReadableHookName = "zz-vmlinuz-readable"

// runningKernelRelease returns the running kernel's release string
// (the `uname -r` value) from /proc, and whether it could be read.
// Used to locate the host kernel image libguestfs needs.
func runningKernelRelease() (string, bool) {
	b, err := os.ReadFile("/proc/sys/kernel/osrelease")
	if err != nil {
		return "", false
	}
	rel := strings.TrimSpace(string(b))
	if rel == "" {
		return "", false
	}
	return rel, true
}

// requireReadableHostKernel verifies the running kernel image is
// readable by the current process. libguestfs builds a supermin
// appliance from the host kernel, so virt-customize / virt-sysprep /
// virt-tar-out / virt-format all fail with the opaque "supermin
// exited with error status 1" when /boot/vmlinuz-<release> is not
// readable. Ubuntu ships those images mode 0600, and a fresh 0600
// image lands on every kernel upgrade -- which is why per-version
// chmod / dpkg-statoverride does not hold. The error surfaces a
// durable, copy-pasteable fix (a kernel postinst.d hook) so a
// downstream user fixes it once instead of rediscovering a
// workaround after every upgrade.
//
// Returns nil when the image is readable, when its path can't be
// determined, when it isn't found (we can't assert it's the
// blocker), or on a non-permission error -- in those cases we let
// libguestfs run and surface its own diagnostics rather than block
// on a false positive.
func requireReadableHostKernel() error {
	rel, ok := runningKernelRelease()
	if !ok {
		return nil
	}
	return checkKernelReadable("/boot/vmlinuz-" + rel)
}

// checkKernelReadable is the path-parameterized core of
// requireReadableHostKernel, split out so it can be tested against a
// temp file without depending on the host's real /boot.
func checkKernelReadable(path string) error {
	f, err := os.Open(path)
	if err == nil {
		_ = f.Close()
		return nil
	}
	if !os.IsPermission(err) {
		return nil
	}
	return fmt.Errorf(`host kernel %s is not readable by this user, so libguestfs
(virt-customize / virt-sysprep / virt-tar-out / virt-format) will fail
building its supermin appliance with "supermin exited with error status 1".

Ubuntu ships /boot/vmlinuz-* mode 0600, and a fresh 0600 image lands on
every kernel upgrade -- a one-off chmod or a per-version dpkg-statoverride
does not survive that. Install a kernel hook once so current and future
kernels stay readable (this makes vmlinuz world-readable):

  sudo tee /etc/kernel/postinst.d/%s >/dev/null <<'HOOK'
#!/bin/sh
# Keep installed kernels readable for libguestfs/supermin.
v="$1"; [ -n "$v" ] && [ -e "/boot/vmlinuz-$v" ] && chmod 0644 "/boot/vmlinuz-$v"
HOOK
  sudo chmod 0755 /etc/kernel/postinst.d/%s
  sudo chmod 0644 /boot/vmlinuz-*

The hook re-applies on every future kernel; the chmod fixes the ones
already installed.`, path, kernelReadableHookName, kernelReadableHookName)
}
