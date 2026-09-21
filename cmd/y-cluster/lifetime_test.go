package main

import (
	"testing"
	"time"

	"github.com/Yolean/y-cluster/pkg/provision/qemu"
)

// reap is what the host timer fires, and what the README promises is
// safe to run by hand, from cron, or from a timer that went stale
// when the deadline was extended: only the persisted deadline counts.
func TestDecideReap(t *testing.T) {
	past, future := time.Now().Add(-time.Minute), time.Now().Add(time.Hour)
	for _, tc := range []struct {
		name string
		ls   qemu.LifetimeState
		want reapAction
	}{
		{"no budget", qemu.LifetimeState{}, reapNothing},
		{"budget of zero is no budget", qemu.LifetimeState{MaxRun: "0", ExpiresAt: past}, reapNothing},
		{"budget set but never armed", qemu.LifetimeState{MaxRun: "8h"}, reapNothing},
		{"stale timer: the deadline was extended", qemu.LifetimeState{MaxRun: "8h", OnExpiry: "stop", ExpiresAt: future}, reapRearm},
		{"expired, onExpiry unset means stop", qemu.LifetimeState{MaxRun: "8h", ExpiresAt: past}, reapStop},
		{"expired, stop", qemu.LifetimeState{MaxRun: "8h", OnExpiry: "stop", ExpiresAt: past}, reapStop},
		{"expired, pause", qemu.LifetimeState{MaxRun: "8h", OnExpiry: "pause", ExpiresAt: past}, reapPause},
		{"expired, teardown", qemu.LifetimeState{MaxRun: "8h", OnExpiry: "teardown", ExpiresAt: past}, reapTeardown},
		{"a destructive action still waits for the deadline", qemu.LifetimeState{MaxRun: "8h", OnExpiry: "teardown", ExpiresAt: future}, reapRearm},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got, reason := decideReap(tc.ls); got != tc.want {
				t.Fatalf("got %s (%s), want %s", got, reason, tc.want)
			}
		})
	}
}
