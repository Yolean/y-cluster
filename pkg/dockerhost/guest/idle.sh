#!/bin/sh
# y-cluster dockerhost idle reaper, inside the guest. See DOCKERHOST.md.
#
#   check    run every minute by y-cluster-dockerhost-idle.timer: powers
#            the guest off when it has been idle for IDLE_TIMEOUT seconds,
#            or when it is older than MAX_AGE seconds and idle right now.
#   lease IDLE_TIMEOUT MAX_AGE
#            run by the host's `y-cluster dockerhost provision` over ssh:
#            someone wants the guest. Also sets the limits (0 = off).
#   status   prints busy or idle.
#
# Busy means an established TCP connection in the guest's own network
# namespace other than ssh and loopback: a docker or buildctl client, a
# client of a published port (docker-proxy), a pull or push under way.
# Running containers alone do not count, so a forgotten `docker run -d`
# does not keep the guest up forever. Powering off ends qemu on the host;
# the disk stays, and the next provision boots it again (or recreates
# it when it is older than the maximum age).
set -eu

# The overrides are for y-cluster's unit test of this script.
dir=${Y_DOCKERHOST_STATE_DIR:-/var/lib/y-cluster-dockerhost}
conf=${Y_DOCKERHOST_REAPER_CONF:-/etc/y-cluster-dockerhost/reaper.conf}
console=${Y_DOCKERHOST_CONSOLE:-/dev/console}
now=$(date +%s)

busy() {
  ss -Htn state established | awk '
    { l = $3; p = $4 }
    l ~ /:22$/ { next }
    p ~ /^(127\.|\[::1\]|\[::ffff:127\.)/ { next }
    { n++ }
    END { exit (n > 0 ? 0 : 1) }'
}

# A missing or garbled file reads as 0.
num() {
  v=$(cat "$1" 2>/dev/null || true)
  case "$v" in
    '' | *[!0-9]*) echo 0 ;;
    *) echo "$v" ;;
  esac
}

case "${1:-check}" in
  lease)
    install -d -m 0755 "$dir"
    echo "$now" > "$dir/lease"
    if [ $# -ge 3 ]; then
      printf 'IDLE_TIMEOUT=%d\nMAX_AGE=%d\n' "$2" "$3" > "$conf"
    fi
    ;;
  status)
    if busy; then echo busy; else echo idle; fi
    ;;
  check)
    IDLE_TIMEOUT=0
    MAX_AGE=0
    # shellcheck source=/dev/null
    [ ! -r "$conf" ] || . "$conf"
    if busy; then
      echo "$now" > "$dir/last-active"
      exit 0
    fi
    created=$(num "$dir/created")
    # Not set up (yet): nothing to measure against.
    [ "$created" -gt 0 ] || exit 0
    last=$created
    for f in lease last-active; do
      v=$(num "$dir/$f")
      [ "$v" -le "$last" ] || last=$v
    done
    reason=""
    if [ "$MAX_AGE" -gt 0 ] && [ $((now - created)) -ge "$MAX_AGE" ]; then
      reason="older than the maximum age of ${MAX_AGE}s"
    elif [ "$IDLE_TIMEOUT" -gt 0 ] && [ $((now - last)) -ge "$IDLE_TIMEOUT" ]; then
      reason="idle for $((now - last))s"
    fi
    [ -n "$reason" ] || exit 0
    msg="y-cluster dockerhost: powering off, $reason"
    echo "$msg"
    # The host keeps the serial console in its console log.
    echo "$msg" > "$console" 2>/dev/null || true
    systemctl poweroff
    ;;
  *)
    echo "usage: $0 check | status | lease IDLE_TIMEOUT MAX_AGE" >&2
    exit 2
    ;;
esac
