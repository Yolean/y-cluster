package qemu

import (
	"testing"
	"time"
)

// pinClock pins nowFunc for the duration of a test and restores it.
func pinClock(t *testing.T, at time.Time) {
	t.Helper()
	prev := nowFunc
	nowFunc = func() time.Time { return at }
	t.Cleanup(func() { nowFunc = prev })
}

func TestArmLifetime_Disabled(t *testing.T) {
	dir := t.TempDir()
	cfg := Config{Name: "c", CacheDir: dir} // no Lifetime
	if err := saveState(cfg); err != nil {
		t.Fatal(err)
	}
	deadline, err := armLifetime(dir, "c")
	if err != nil {
		t.Fatal(err)
	}
	if !deadline.IsZero() {
		t.Fatalf("disabled lifetime should arm no deadline, got %v", deadline)
	}
	ls, err := loadLifetime(dir, "c")
	if err != nil {
		t.Fatal(err)
	}
	if ls.Enabled() || !ls.ExpiresAt.IsZero() {
		t.Fatalf("expected no lifetime state, got %+v", ls)
	}
}

func TestArmLifetime_AnchorsToNow(t *testing.T) {
	dir := t.TempDir()
	base := time.Date(2026, 6, 21, 12, 0, 0, 0, time.UTC)
	pinClock(t, base)

	cfg := Config{Name: "c", CacheDir: dir, Lifetime: "8h", OnExpiry: "stop"}
	if err := saveState(cfg); err != nil {
		t.Fatal(err)
	}
	deadline, err := armLifetime(dir, "c")
	if err != nil {
		t.Fatal(err)
	}
	want := base.Add(8 * time.Hour)
	if !deadline.Equal(want) {
		t.Fatalf("deadline = %v, want %v", deadline, want)
	}

	ls, err := loadLifetime(dir, "c")
	if err != nil {
		t.Fatal(err)
	}
	if ls.MaxRun != "8h" || ls.OnExpiry != "stop" {
		t.Fatalf("policy not persisted: %+v", ls)
	}
	if !ls.ExpiresAt.Equal(want) {
		t.Fatalf("persisted ExpiresAt = %v, want %v", ls.ExpiresAt, want)
	}
}

func TestArmLifetime_ReanchorsOnReArm(t *testing.T) {
	dir := t.TempDir()
	cfg := Config{Name: "c", CacheDir: dir, Lifetime: "2h", OnExpiry: "stop"}
	if err := saveState(cfg); err != nil {
		t.Fatal(err)
	}

	t0 := time.Date(2026, 6, 21, 10, 0, 0, 0, time.UTC)
	pinClock(t, t0)
	if _, err := armLifetime(dir, "c"); err != nil {
		t.Fatal(err)
	}

	// Simulate stop+start three hours later: re-arm gives a fresh
	// window from the new "now", not from the original provision.
	t1 := t0.Add(3 * time.Hour)
	pinClock(t, t1)
	deadline, err := armLifetime(dir, "c")
	if err != nil {
		t.Fatal(err)
	}
	if want := t1.Add(2 * time.Hour); !deadline.Equal(want) {
		t.Fatalf("re-armed deadline = %v, want %v", deadline, want)
	}
}

func TestLifetimeState_ExpiredAndRemaining(t *testing.T) {
	dir := t.TempDir()
	cfg := Config{Name: "c", CacheDir: dir, Lifetime: "1h", OnExpiry: "stop"}
	if err := saveState(cfg); err != nil {
		t.Fatal(err)
	}
	t0 := time.Date(2026, 6, 21, 9, 0, 0, 0, time.UTC)
	pinClock(t, t0)
	if _, err := armLifetime(dir, "c"); err != nil {
		t.Fatal(err)
	}

	// 30m in: not expired, ~30m remaining.
	pinClock(t, t0.Add(30*time.Minute))
	ls, err := loadLifetime(dir, "c")
	if err != nil {
		t.Fatal(err)
	}
	if ls.Expired() {
		t.Fatal("should not be expired at 30m of a 1h budget")
	}
	if r := ls.Remaining(); r != 30*time.Minute {
		t.Fatalf("Remaining = %v, want 30m", r)
	}

	// 90m in: expired, negative remaining.
	pinClock(t, t0.Add(90*time.Minute))
	ls, err = loadLifetime(dir, "c")
	if err != nil {
		t.Fatal(err)
	}
	if !ls.Expired() {
		t.Fatal("should be expired at 90m of a 1h budget")
	}
	if r := ls.Remaining(); r >= 0 {
		t.Fatalf("Remaining = %v, want negative", r)
	}
}

func TestSetExpiresAt_Extend(t *testing.T) {
	dir := t.TempDir()
	cfg := Config{Name: "c", CacheDir: dir, Lifetime: "1h", OnExpiry: "stop"}
	if err := saveState(cfg); err != nil {
		t.Fatal(err)
	}
	t0 := time.Date(2026, 6, 21, 9, 0, 0, 0, time.UTC)
	pinClock(t, t0)
	if _, err := armLifetime(dir, "c"); err != nil {
		t.Fatal(err)
	}
	ls, err := loadLifetime(dir, "c")
	if err != nil {
		t.Fatal(err)
	}
	extended := ls.ExpiresAt.Add(2 * time.Hour)
	if err := setExpiresAt(dir, "c", extended); err != nil {
		t.Fatal(err)
	}
	ls, err = loadLifetime(dir, "c")
	if err != nil {
		t.Fatal(err)
	}
	if !ls.ExpiresAt.Equal(extended) {
		t.Fatalf("extended ExpiresAt = %v, want %v", ls.ExpiresAt, extended)
	}
	// Extend must preserve the policy.
	if ls.MaxRun != "1h" || ls.OnExpiry != "stop" {
		t.Fatalf("extend clobbered policy: %+v", ls)
	}
}

// Old sidecars (no lifetime fields) decode to "no lifetime".
func TestLoadLifetime_LegacySidecar(t *testing.T) {
	dir := t.TempDir()
	cfg := Config{Name: "c", CacheDir: dir} // no lifetime at all
	if err := saveState(cfg); err != nil {
		t.Fatal(err)
	}
	ls, err := loadLifetime(dir, "c")
	if err != nil {
		t.Fatal(err)
	}
	if ls.Enabled() || !ls.ExpiresAt.IsZero() {
		t.Fatalf("legacy sidecar should yield empty lifetime, got %+v", ls)
	}
}
