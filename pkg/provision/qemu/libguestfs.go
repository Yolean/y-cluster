package qemu

import (
	"fmt"
	"os"
	"path/filepath"
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

// requireReadableHostKernel verifies the kernel image supermin will
// select is readable by the current process. libguestfs builds a
// supermin appliance from a host kernel, so virt-customize /
// virt-sysprep / virt-tar-out / virt-format all fail with the opaque
// "supermin exited with error status 1" when that image is not
// readable. Ubuntu ships those images mode 0600, and a fresh 0600
// image lands on every kernel upgrade -- which is why per-version
// chmod / dpkg-statoverride does not hold. The error surfaces a
// durable, copy-pasteable fix (a kernel postinst.d hook) so a
// downstream user fixes it once instead of rediscovering a
// workaround after every upgrade.
//
// Supermin does NOT use the running kernel: it picks the NEWEST
// /boot/vmlinuz-* that has a matching /lib/modules/<release> dir.
// Checking only the running kernel therefore passes on a host where
// a newer 0600 kernel is installed but not yet booted, and supermin
// still fails (observed on a dev host after an unattended kernel
// upgrade). We mirror supermin's selection and fall back to the
// running kernel when no candidate is found.
//
// Returns nil when the image is readable, when its path can't be
// determined, when it isn't found (we can't assert it's the
// blocker), or on a non-permission error -- in those cases we let
// libguestfs run and surface its own diagnostics rather than block
// on a false positive.
//
// A var (not a plain func) so a test can force it to fail and assert
// that a call site checks it only after its cheap correctness
// preconditions -- otherwise an unreadable kernel on the build host
// masks the actionable error (it did, on the CI runner).
var requireReadableHostKernel = func() error {
	if path, ok := superminKernelCandidate("/boot", "/lib/modules"); ok {
		return checkKernelReadable(path)
	}
	rel, ok := runningKernelRelease()
	if !ok {
		return nil
	}
	return checkKernelReadable("/boot/vmlinuz-" + rel)
}

// superminKernelCandidate mirrors supermin's kernel selection: the
// newest (by release-version ordering) bootDir/vmlinuz-* that has a
// matching modulesDir/<release> directory. Parameterized on the two
// roots so tests run against temp dirs.
func superminKernelCandidate(bootDir, modulesDir string) (string, bool) {
	matches, err := filepath.Glob(filepath.Join(bootDir, "vmlinuz-*"))
	if err != nil || len(matches) == 0 {
		return "", false
	}
	best := ""
	bestRel := ""
	for _, m := range matches {
		rel := strings.TrimPrefix(filepath.Base(m), "vmlinuz-")
		if st, err := os.Stat(filepath.Join(modulesDir, rel)); err != nil || !st.IsDir() {
			continue
		}
		if best == "" || kernelReleaseLess(bestRel, rel) {
			best, bestRel = m, rel
		}
	}
	return best, best != ""
}

// kernelReleaseLess orders kernel release strings by their numeric
// segments (so 6.9.x sorts before 6.17.x, which a plain string
// compare gets wrong). Non-numeric runs separate the segments; a
// missing segment sorts first, matching dpkg's ordering closely
// enough for the vmlinuz-<ver>-<flavor> shapes found under /boot.
func kernelReleaseLess(a, b string) bool {
	as, bs := numericSegments(a), numericSegments(b)
	for i := 0; i < len(as) && i < len(bs); i++ {
		if as[i] != bs[i] {
			return as[i] < bs[i]
		}
	}
	return len(as) < len(bs)
}

func numericSegments(s string) []int {
	var segs []int
	cur := -1
	for _, r := range s {
		if r >= '0' && r <= '9' {
			if cur < 0 {
				cur = 0
			}
			cur = cur*10 + int(r-'0')
		} else if cur >= 0 {
			segs = append(segs, cur)
			cur = -1
		}
	}
	if cur >= 0 {
		segs = append(segs, cur)
	}
	return segs
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
already installed`, path, kernelReadableHookName, kernelReadableHookName)
}
