package dockerhost

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Yolean/y-cluster/pkg/inventory"
	"github.com/Yolean/y-cluster/pkg/shquote"
)

func readyState() State {
	created := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
	return State{
		Network:      Network{Ifname: "ycl1", GuestAddress: "10.88.1.2/24", Gateway: "10.88.1.1", DNS: []string{"1.1.1.1", "9.9.9.9"}},
		Address:      "10.88.1.2",
		DockerPort:   DockerPort,
		BuildkitPort: BuildkitPort,
		MemoryMB:     6144,
		CPUs:         4,
		DiskSize:     "60G",
		IdleTimeout:  "8h",
		MaxAge:       "336h",
		CreatedAt:    created,
		ReadyAt:      created.Add(3 * time.Minute),
		Pins:         currentPins(),
	}
}

func gle01() Config {
	c := Config{Network: Network{Ifname: "ycl1", GuestAddress: "10.88.1.2/24"}}
	c.ApplyDefaults()
	return c
}

func TestDecideExisting(t *testing.T) {
	st := readyState()
	day := 24 * time.Hour
	young := st.CreatedAt.Add(2 * day)
	old := st.CreatedAt.Add(14*day + time.Hour)
	ancient := st.CreatedAt.Add(15*day + time.Hour)
	otherPins := st
	otherPins.Pins.Docker = "29.0.0"
	notReady := st
	notReady.ReadyAt = time.Time{}
	bigger := gle01()
	bigger.MemoryMB = 8192
	moved := gle01()
	moved.Network.GuestAddress = "10.88.2.2/24"
	moved.ApplyDefaults()
	longer := gle01()
	longer.IdleTimeout = "12h"

	for _, tc := range []struct {
		name    string
		st      State
		cfg     Config
		running bool
		now     time.Time
		busy    bool
		want    action
		reason  string
	}{
		{"healthy candidate", st, gle01(), true, young, false, actReuse, ""},
		{"limits are not drift", st, longer, true, young, false, actReuse, ""},
		{"powered off by the reaper", st, gle01(), false, young, false, actRestart, "stopped"},
		{"first boot never finished", notReady, gle01(), true, young, false, actReplace, "first boot"},
		{"more memory", st, bigger, true, young, true, actReplace, "size"},
		{"another address", st, moved, true, young, true, actReplace, "network"},
		{"past max age, idle", st, gle01(), true, old, false, actReplace, "past the maximum age"},
		{"past max age, in use", st, gle01(), true, old, true, actReuse, "past the maximum age"},
		{"past max age, stopped", st, gle01(), false, old, false, actReplace, "past the maximum age"},
		{"a day past max age, in use", st, gle01(), true, ancient, true, actReplace, "a day past"},
		{"new pins, idle", otherPins, gle01(), true, young, false, actReplace, "pins"},
		{"new pins, in use", otherPins, gle01(), true, young, true, actReuse, "pins"},
		{"new pins, stopped", otherPins, gle01(), false, young, false, actReplace, "pins"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			asked := false
			got, reason := decideExisting(tc.st, tc.cfg, tc.running, tc.now, func() bool { asked = true; return tc.busy })
			if got != tc.want {
				t.Fatalf("got %s (%s), want %s", got, reason, tc.want)
			}
			if !strings.Contains(reason, tc.reason) {
				t.Errorf("reason %q lacks %q", reason, tc.reason)
			}
			if asked && !tc.running {
				t.Error("a stopped guest cannot be asked whether it is busy")
			}
		})
	}
}

func TestEnv(t *testing.T) {
	dir := t.TempDir()
	p := paths(dir)
	running := false
	restore := guestRunning
	guestRunning = func(string) (int, bool) { return 4242, running }
	t.Cleanup(func() { guestRunning = restore })
	noEnv := func(string) string { return "" }

	if out, err := Env(dir, noEnv); err != nil || out != "" {
		t.Fatalf("no guest: got %q, %v", out, err)
	}

	st := readyState()
	if err := saveState(p, st); err != nil {
		t.Fatal(err)
	}
	if out, _ := Env(dir, noEnv); out != "" {
		t.Fatalf("a stopped guest prints nothing, got %q", out)
	}
	running = true
	out, err := Env(dir, noEnv)
	if err != nil {
		t.Fatal(err)
	}
	client := filepath.Join(dir, "client")
	want := "export DOCKER_HOST='tcp://10.88.1.2:2376'\n" +
		"export DOCKER_TLS_VERIFY='1'\n" +
		"export DOCKER_CERT_PATH='" + client + "'\n" +
		"export BUILDKIT_HOST='tcp://10.88.1.2:8547'\n" +
		"export BUILDKIT_TLS_DIR='" + client + "'\n"
	if out != want {
		t.Fatalf("env:\n%s\nwant:\n%s", out, want)
	}

	// A guest still in its first boot is not announced.
	booting := st
	booting.ReadyAt = time.Time{}
	if err := saveState(p, booting); err != nil {
		t.Fatal(err)
	}
	if out, _ := Env(dir, noEnv); out != "" {
		t.Fatalf("a guest in its first boot prints nothing, got %q", out)
	}

	// Gone: the shell that still points at it is reset, a shell with
	// someone else's daemon is left alone.
	running = false
	ours := func(k string) string {
		if k == "DOCKER_CERT_PATH" {
			return client
		}
		return ""
	}
	if out, _ := Env(dir, ours); out != "unset DOCKER_HOST DOCKER_TLS_VERIFY DOCKER_CERT_PATH BUILDKIT_HOST BUILDKIT_TLS_DIR\n" {
		t.Fatalf("stale variables of this dockerhost: got %q", out)
	}
	foreign := func(k string) string {
		return map[string]string{"DOCKER_HOST": "tcp://dockerd:2375", "DOCKER_CERT_PATH": "/home/u/.docker"}[k]
	}
	if out, _ := Env(dir, foreign); out != "" {
		t.Fatalf("someone else's DOCKER_HOST must be left alone, got %q", out)
	}
}

func TestState_RoundTripAndMode(t *testing.T) {
	p := paths(t.TempDir())
	if _, err := loadState(p); err != errNoState {
		t.Fatalf("want errNoState, got %v", err)
	}
	st := readyState()
	st.TestForwards = &TestForwards{SSH: "1", Docker: "2", Buildkit: "3"}
	if err := saveState(p, st); err != nil {
		t.Fatal(err)
	}
	fi, err := os.Stat(p.state())
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode().Perm() != 0o600 {
		t.Errorf("state mode %o", fi.Mode().Perm())
	}
	got, err := loadState(p)
	if err != nil {
		t.Fatal(err)
	}
	if got.Address != st.Address || !got.CreatedAt.Equal(st.CreatedAt) || got.TestForwards == nil || *got.TestForwards != *st.TestForwards {
		t.Fatalf("round trip: %+v", got)
	}
	if err := os.WriteFile(p.state(), []byte(`{"version": 99}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := loadState(p); err == nil || !strings.Contains(err.Error(), "unsupported state version") {
		t.Fatalf("a newer schema must be refused, got %v", err)
	}
}

func TestTeardown_NoGuest(t *testing.T) {
	t.Setenv("Y_CLUSTER_INVENTORY_DIR", t.TempDir())
	if err := Teardown(filepath.Join(t.TempDir(), "never-provisioned"), nil); err != nil {
		t.Fatal(err)
	}
	// State and certificates of a guest whose VM is already gone.
	dir := t.TempDir()
	p := paths(dir)
	if err := saveState(p, readyState()); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(p.client(), 0o700); err != nil {
		t.Fatal(err)
	}
	recordInventory(p, readyState(), nil)
	if err := Teardown(dir, nil); err != nil {
		t.Fatal(err)
	}
	for _, gone := range []string{p.state(), p.client()} {
		if _, err := os.Stat(gone); !os.IsNotExist(err) {
			t.Errorf("%s must be removed", gone)
		}
	}
	if recs, _ := inventory.List(); len(recs) != 0 {
		t.Errorf("inventory still lists %v", recs)
	}
}

func TestRecordInventory_TeardownCommand(t *testing.T) {
	t.Setenv("Y_CLUSTER_INVENTORY_DIR", t.TempDir())
	home := t.TempDir()
	t.Setenv("HOME", home)
	recordInventory(paths(filepath.Join(home, ".cache", "y-cluster-dockerhost")), readyState(), nil)
	recs, err := inventory.List()
	if err != nil || len(recs) != 1 {
		t.Fatalf("records %v, %v", recs, err)
	}
	if got := recs[0].TeardownCommand(); got != "y-cluster dockerhost teardown" {
		t.Errorf("default dir: %q", got)
	}
	st := readyState()
	st.TestForwards = &TestForwards{SSH: "22022", Docker: "22376", Buildkit: "28547"}
	recordInventory(paths("/tmp/e2e state"), st, nil)
	recs, _ = inventory.List()
	if got := recs[0].TeardownCommand(); got != "Y_CLUSTER_DOCKERHOST_DIR='/tmp/e2e state' y-cluster dockerhost teardown" {
		t.Errorf("other dir: %q", got)
	}
	if strings.Join(recs[0].HostPorts, ",") != "22022,22376,28547" {
		t.Errorf("host ports %v", recs[0].HostPorts)
	}
}

func TestWithLock_Exclusive(t *testing.T) {
	p := paths(t.TempDir())
	restore := lockTimeout
	lockTimeout = 300 * time.Millisecond
	t.Cleanup(func() { lockTimeout = restore })
	held := make(chan struct{})
	release := make(chan struct{})
	done := make(chan error)
	go func() {
		done <- withLock(p, func() error { close(held); <-release; return nil })
	}()
	<-held
	if err := withLock(p, func() error { return nil }); err == nil || !strings.Contains(err.Error(), "held") {
		t.Fatalf("a second holder must wait and give up, got %v", err)
	}
	close(release)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if err := withLock(p, func() error { return nil }); err != nil {
		t.Fatalf("the lock is free again: %v", err)
	}
	fi, err := os.Stat(p.dir())
	if err != nil || fi.Mode().Perm() != 0o700 {
		t.Fatalf("state dir mode: %v %v", fi.Mode(), err)
	}
}

func TestGetStatus_NoGuest(t *testing.T) {
	s, err := GetStatus(context.Background(), t.TempDir())
	if err != nil || s.State != nil || s.Healthy() {
		t.Fatalf("got %+v, %v", s, err)
	}
}

func TestBuildkitClient(t *testing.T) {
	dir := t.TempDir()
	p := paths(dir)
	running := false
	restore := guestRunning
	guestRunning = func(string) (int, bool) { return 4242, running }
	t.Cleanup(func() { guestRunning = restore })

	if _, _, found, err := BuildkitClient(dir); found || err != nil {
		t.Fatalf("no guest: found %v, %v", found, err)
	}

	if err := saveState(p, readyState()); err != nil {
		t.Fatal(err)
	}
	_, _, found, err := BuildkitClient(dir)
	if !found || !errors.Is(err, ErrGuestDown) {
		t.Fatalf("a stopped guest: found %v, %v", found, err)
	}
	if want := DirEnv + "=" + shquote.Quote(dir) + " y-cluster dockerhost provision"; !strings.Contains(err.Error(), want) {
		t.Errorf("%q does not say %q", err, want)
	}

	running = true
	addr, tlsDir, found, err := BuildkitClient(dir)
	if err != nil || !found {
		t.Fatalf("a running guest: found %v, %v", found, err)
	}
	if addr != "tcp://10.88.1.2:8547" || tlsDir != filepath.Join(dir, "client") {
		t.Errorf("got %s %s", addr, tlsDir)
	}
}
