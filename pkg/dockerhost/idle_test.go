package dockerhost

import (
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// reaperSandbox runs guest/idle.sh with ss, date and systemctl stubbed
// and its paths in a temporary directory.
type reaperSandbox struct {
	t       *testing.T
	root    string
	now     int64
	sockets string // ss -Htn state established output
}

func newReaperSandbox(t *testing.T) *reaperSandbox {
	t.Helper()
	if _, err := exec.LookPath("sh"); err != nil {
		t.Skip("no sh")
	}
	root := t.TempDir()
	bin := filepath.Join(root, "bin")
	for _, d := range []string{bin, filepath.Join(root, "state")} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	stubs := map[string]string{
		"ss":        "#!/bin/sh\ncat \"$STUB_SS\"\n",
		"date":      "#!/bin/sh\necho \"$STUB_NOW\"\n",
		"systemctl": "#!/bin/sh\necho \"$*\" >> \"$STUB_SYSTEMCTL\"\n",
	}
	for name, body := range stubs {
		if err := os.WriteFile(filepath.Join(bin, name), []byte(body), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	script, err := guestFS.ReadFile("guest/idle.sh")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "idle.sh"), script, 0o755); err != nil {
		t.Fatal(err)
	}
	return &reaperSandbox{t: t, root: root, now: 1_790_000_000}
}

func (r *reaperSandbox) write(name, content string) {
	r.t.Helper()
	if err := os.WriteFile(filepath.Join(r.root, name), []byte(content), 0o644); err != nil {
		r.t.Fatal(err)
	}
}

func (r *reaperSandbox) read(name string) string {
	b, _ := os.ReadFile(filepath.Join(r.root, name))
	return string(b)
}

func (r *reaperSandbox) run(args ...string) string {
	r.t.Helper()
	r.write("ss.out", r.sockets)
	cmd := exec.Command("sh", append([]string{filepath.Join(r.root, "idle.sh")}, args...)...)
	cmd.Env = append(os.Environ(),
		"PATH="+filepath.Join(r.root, "bin")+":"+os.Getenv("PATH"),
		"STUB_SS="+filepath.Join(r.root, "ss.out"),
		"STUB_NOW="+strconv.FormatInt(r.now, 10),
		"STUB_SYSTEMCTL="+filepath.Join(r.root, "systemctl.log"),
		"Y_DOCKERHOST_STATE_DIR="+filepath.Join(r.root, "state"),
		"Y_DOCKERHOST_REAPER_CONF="+filepath.Join(r.root, "reaper.conf"),
		"Y_DOCKERHOST_CONSOLE="+filepath.Join(r.root, "console"),
	)
	out, err := cmd.CombinedOutput()
	if err != nil {
		r.t.Fatalf("idle.sh %v: %v\n%s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

func (r *reaperSandbox) poweredOff() bool {
	return strings.Contains(r.read("systemctl.log"), "poweroff")
}

const (
	sshSession   = "0 0 10.88.1.2:22 10.88.1.1:40000\n"
	loopback     = "0 0 127.0.0.1:41000 127.0.0.1:2376\n0 0 127.0.0.1:2376 127.0.0.1:41000\n"
	dockerClient = "0 0 10.88.1.2:2376 10.88.1.1:51234\n"
	publishedV6  = "0 0 [::ffff:10.88.1.2]:32768 [::ffff:10.88.1.1]:51000\n"
)

func TestIdleScript_Activity(t *testing.T) {
	r := newReaperSandbox(t)
	for _, tc := range []struct {
		sockets, want string
	}{
		{"", "idle"},
		{sshSession + loopback, "idle"},
		{sshSession + dockerClient, "busy"},
		{publishedV6, "busy"},
	} {
		r.sockets = tc.sockets
		if got := r.run("status"); got != tc.want {
			t.Errorf("sockets %q: %s, want %s", tc.sockets, got, tc.want)
		}
	}
}

func TestIdleScript_Lease(t *testing.T) {
	r := newReaperSandbox(t)
	r.run("lease", "600", "3600")
	if got := strings.TrimSpace(r.read("state/lease")); got != strconv.FormatInt(r.now, 10) {
		t.Errorf("lease %q", got)
	}
	if got := r.read("reaper.conf"); got != "IDLE_TIMEOUT=600\nMAX_AGE=3600\n" {
		t.Errorf("reaper.conf %q", got)
	}
}

func TestIdleScript_Check(t *testing.T) {
	const idle, maxAge = 600, 86400
	for _, tc := range []struct {
		name     string
		created  int64 // seconds before now; 0 = not set up
		lease    int64 // seconds before now
		sockets  string
		conf     string
		poweroff string // reason expected in the console, "" for none
	}{
		{"in use", 100, 7200, dockerClient, "", ""},
		{"leased recently", 7200, 60, sshSession, "", ""},
		{"idle past the timeout", 7200, 7200, sshSession, "", "idle for 7200s"},
		{"past the maximum age", maxAge + 1, 10, "", "", "older than the maximum age"},
		{"past the maximum age but in use", maxAge + 1, 10, dockerClient, "", ""},
		{"not set up", 0, 7200, "", "", ""},
		{"reaper off", 7200, 7200, "", "IDLE_TIMEOUT=0\nMAX_AGE=0\n", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := newReaperSandbox(t)
			conf := tc.conf
			if conf == "" {
				conf = "IDLE_TIMEOUT=" + strconv.Itoa(idle) + "\nMAX_AGE=" + strconv.Itoa(maxAge) + "\n"
			}
			r.write("reaper.conf", conf)
			if tc.created > 0 {
				r.write("state/created", strconv.FormatInt(r.now-tc.created, 10)+"\n")
			}
			r.write("state/lease", strconv.FormatInt(r.now-tc.lease, 10)+"\n")
			r.sockets = tc.sockets
			r.run("check")
			if tc.poweroff == "" {
				if r.poweredOff() {
					t.Fatalf("powered off: %s", r.read("console"))
				}
				return
			}
			if !r.poweredOff() {
				t.Fatal("did not power off")
			}
			if c := r.read("console"); !strings.Contains(c, tc.poweroff) {
				t.Errorf("console %q lacks %q", c, tc.poweroff)
			}
		})
	}
}

// Activity seen by one check keeps the guest up for the timeout after
// it, however old the lease.
func TestIdleScript_LastActiveCounts(t *testing.T) {
	r := newReaperSandbox(t)
	r.write("reaper.conf", "IDLE_TIMEOUT=600\nMAX_AGE=86400\n")
	r.write("state/created", strconv.FormatInt(r.now-7200, 10))
	r.write("state/lease", strconv.FormatInt(r.now-7200, 10))
	r.sockets = dockerClient
	r.run("check")
	r.sockets = ""
	r.now += 300
	r.run("check")
	if r.poweredOff() {
		t.Fatal("powered off 300s after the last activity")
	}
	r.now += 301
	r.run("check")
	if !r.poweredOff() {
		t.Fatal("still up 601s after the last activity")
	}
}
