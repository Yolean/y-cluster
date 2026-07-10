# shellcheck shell=bash
# Sourced by the appliance build / e2e scripts before any libguestfs
# (virt-sysprep / virt-customize) work, to fail fast with a DURABLE
# fix when the kernel image supermin will use is not readable.
# libguestfs builds a supermin appliance from a host kernel, and
# Ubuntu ships /boot/vmlinuz-* mode 0600, so a fresh 0600 image lands
# on every kernel upgrade. Supermin picks the NEWEST /boot/vmlinuz-*
# that has a matching /lib/modules/<release> dir -- not the running
# kernel -- so the check mirrors that selection. This message is kept
# in sync with requireReadableHostKernel() in
# pkg/provision/qemu/libguestfs.go (the binary enforces the same
# check at its libguestfs call sites).
__kimg=""
__kbest=""
for __k in /boot/vmlinuz-*; do
    [ -e "$__k" ] || continue
    __krel="${__k#/boot/vmlinuz-}"
    [ -d "/lib/modules/$__krel" ] || continue
    if [ -z "$__kbest" ] || [ "$(printf '%s\n%s\n' "$__kbest" "$__krel" | sort -V | tail -1)" = "$__krel" ]; then
        __kbest="$__krel"
        __kimg="$__k"
    fi
done
[ -n "$__kimg" ] || __kimg="/boot/vmlinuz-$(uname -r)"
if ! [ -r "$__kimg" ]; then
    {
        echo "host kernel $__kimg is not readable by this user, so"
        echo "libguestfs (virt-customize / virt-sysprep) will fail building its"
        echo 'supermin appliance with "supermin exited with error status 1".'
        cat <<'EOM'

Ubuntu ships /boot/vmlinuz-* mode 0600, and a fresh 0600 image lands on
every kernel upgrade -- a one-off chmod or a per-version dpkg-statoverride
does not survive that. Install a kernel hook once so current and future
kernels stay readable (this makes vmlinuz world-readable):

  sudo tee /etc/kernel/postinst.d/zz-vmlinuz-readable >/dev/null <<'HOOK'
#!/bin/sh
# Keep installed kernels readable for libguestfs/supermin.
v="$1"; [ -n "$v" ] && [ -e "/boot/vmlinuz-$v" ] && chmod 0644 "/boot/vmlinuz-$v"
HOOK
  sudo chmod 0755 /etc/kernel/postinst.d/zz-vmlinuz-readable
  sudo chmod 0644 /boot/vmlinuz-*

The hook re-applies on every future kernel; the chmod fixes the ones
already installed.
EOM
    } >&2
    exit 1
fi
unset __kimg __kbest __krel __k
