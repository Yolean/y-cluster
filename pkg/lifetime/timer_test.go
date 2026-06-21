package lifetime

import (
	"strings"
	"testing"
	"time"
)

func TestUnitName_Sanitizes(t *testing.T) {
	tests := map[string]string{
		"local":        "y-cluster-lifetime-local",
		"alice-dev1":   "y-cluster-lifetime-alice-dev1",
		"weird/ctx @1": "y-cluster-lifetime-weird-ctx--1",
	}
	for in, want := range tests {
		if got := unitName(in); got != want {
			t.Errorf("unitName(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestRemainingSeconds_FloorIsOne(t *testing.T) {
	if got := remainingSeconds(-5 * time.Minute); got != 1 {
		t.Errorf("past-due remaining should floor to 1s, got %d", got)
	}
	if got := remainingSeconds(0); got != 1 {
		t.Errorf("zero remaining should floor to 1s, got %d", got)
	}
	if got := remainingSeconds(90 * time.Second); got != 90 {
		t.Errorf("remainingSeconds(90s) = %d, want 90", got)
	}
}

func TestSystemdRunArgs(t *testing.T) {
	args := systemdRunArgs("/usr/local/bin/y-cluster", "local", 8*time.Hour)
	joined := strings.Join(args, " ")
	for _, want := range []string{
		"--user",
		"--unit=y-cluster-lifetime-local",
		"--on-active=28800s",
		"--",
		"/usr/local/bin/y-cluster lifetime reap --context=local",
	} {
		if !strings.Contains(joined, want) {
			t.Errorf("systemd-run args missing %q in: %s", want, joined)
		}
	}
}

func TestAtTimeSpec_RoundsUpToMinute(t *testing.T) {
	tests := map[time.Duration]string{
		30 * time.Second: "now + 1 minutes",
		90 * time.Second: "now + 2 minutes",
		8 * time.Hour:    "now + 480 minutes",
		-1 * time.Minute: "now + 1 minutes",
	}
	for in, want := range tests {
		if got := atTimeSpec(in); got != want {
			t.Errorf("atTimeSpec(%v) = %q, want %q", in, got, want)
		}
	}
}

func TestAtScript_CarriesMarkerAndCommand(t *testing.T) {
	s := atScript("/usr/local/bin/y-cluster", "alice-dev1")
	if !strings.Contains(s, "/usr/local/bin/y-cluster lifetime reap --context=alice-dev1") {
		t.Errorf("at script missing reap command: %s", s)
	}
	if !strings.Contains(s, "# y-cluster-lifetime-alice-dev1") {
		t.Errorf("at script missing disarm marker: %s", s)
	}
}

func TestReapInvocation(t *testing.T) {
	got := reapInvocation("y-cluster", "local")
	want := []string{"y-cluster", "lifetime", "reap", "--context=local"}
	if strings.Join(got, " ") != strings.Join(want, " ") {
		t.Errorf("reapInvocation = %v, want %v", got, want)
	}
}
