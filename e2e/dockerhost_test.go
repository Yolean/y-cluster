//go:build e2e && kvm

package e2e

import (
	"bufio"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/Yolean/y-cluster/pkg/dockerhost"
	"github.com/Yolean/y-cluster/pkg/provision/qemu"
)

// The dockerhost e2e tests boot a real guest: cloud-init installs the
// pinned dockerd, containerd and buildkitd, and the host talks to them
// with the docker CLI and buildctl the way a session would. docker must
// be on PATH (or named by Y_CLUSTER_E2E_DOCKER); buildctl is the built
// binary's `y-cluster buildctl` unless Y_CLUSTER_E2E_BUILDCTL names
// another.
//
// TestDockerhost_TestForwards needs nothing from root: qemu user-mode
// networking with the daemon ports forwarded to 127.0.0.1. It is a
// test harness, not a way to use the dockerhost -- published container
// ports have no address the host reaches -- and covers everything else:
// the first boot, TLS on both ports and refusal without a certificate,
// a docker run and a buildctl build, idempotent provision, the idle
// reaper powering the guest off, and the restart from its disk.
//
// TestDockerhost_Tap is the product path and the exposure checks of the
// compliance review (D-e). It needs the tap that root prepared
// (DOCKERHOST.md, One-time root setup) and skips without:
//
//	Y_CLUSTER_E2E_DOCKERHOST_IFNAME=ycl1 Y_CLUSTER_E2E_DOCKERHOST_GUEST_ADDRESS=10.88.1.2/24 \
//	  go test -tags 'e2e kvm' -run TestDockerhost_Tap -timeout 30m ./e2e/

// dockerhostClients returns the docker CLI and the buildctl command
// line, bin being the y-cluster binary under test.
func dockerhostClients(t *testing.T, bin string) (docker string, buildctl []string) {
	t.Helper()
	docker = os.Getenv("Y_CLUSTER_E2E_DOCKER")
	if docker == "" {
		p, err := exec.LookPath("docker")
		if err != nil {
			t.Skip("docker not on PATH (or set Y_CLUSTER_E2E_DOCKER); the dockerhost e2e tests drive the guest with it")
		}
		docker = p
	}
	if p := os.Getenv("Y_CLUSTER_E2E_BUILDCTL"); p != "" {
		return docker, []string{p}
	}
	return docker, []string{bin, "buildctl"}
}

func mustBuildctl(t *testing.T, env []string, buildctl []string, args ...string) string {
	t.Helper()
	return mustClient(t, env, buildctl[0], append(buildctl[1:len(buildctl):len(buildctl)], args...)...)
}

func requireKVM(t *testing.T) {
	t.Helper()
	if _, err := os.Stat("/dev/kvm"); err != nil {
		t.Skip("dockerhost tests require /dev/kvm")
	}
	if err := qemu.CheckPrerequisites(); err != nil {
		t.Skip(err)
	}
}

// sharedDockerhostImages keeps the verified cloud images across test
// runs; provision never prunes a directory it is given.
func sharedDockerhostImages(t *testing.T) string {
	t.Helper()
	base, err := os.UserCacheDir()
	if err != nil {
		t.Fatal(err)
	}
	return filepath.Join(base, "y-cluster-e2e", "dockerhost-images")
}

// dockerhostEnv runs the built binary's `dockerhost env` against dir
// and parses the export lines.
func dockerhostEnv(t *testing.T, bin, dir string, extraEnv ...string) (map[string]string, string) {
	t.Helper()
	cmd := exec.Command(bin, "dockerhost", "env")
	cmd.Env = append(append(os.Environ(), dockerhost.DirEnv+"="+dir), extraEnv...)
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("dockerhost env: %v", err)
	}
	vars := map[string]string{}
	re := regexp.MustCompile(`^export ([A-Z_]+)='(.*)'$`)
	sc := bufio.NewScanner(strings.NewReader(string(out)))
	for sc.Scan() {
		if m := re.FindStringSubmatch(sc.Text()); m != nil {
			vars[m[1]] = m[2]
		}
	}
	return vars, string(out)
}

// clientEnv is os.Environ with the contract's variables replaced.
func clientEnv(vars map[string]string) []string {
	var env []string
	for _, kv := range os.Environ() {
		k, _, _ := strings.Cut(kv, "=")
		if _, ours := vars[k]; !ours && k != "DOCKER_CONTEXT" && k != "DOCKER_CONFIG" {
			env = append(env, kv)
		}
	}
	for k, v := range vars {
		env = append(env, k+"="+v)
	}
	return env
}

func runClient(t *testing.T, env []string, name string, args ...string) (string, error) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Env = env
	out, err := cmd.CombinedOutput()
	return string(out), err
}

func mustClient(t *testing.T, env []string, name string, args ...string) string {
	t.Helper()
	out, err := runClient(t, env, name, args...)
	if err != nil {
		t.Fatalf("%s %v: %v\n%s", filepath.Base(name), args, err, out)
	}
	return out
}

func guestExec(t *testing.T, dir, command string) string {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	out, err := qemu.GuestExec(ctx, filepath.Join(dir, "vm"), "dockerhost", command)
	if err != nil {
		t.Fatalf("guest %q: %v\n%s", command, err, out)
	}
	return string(out)
}

// assertRefusesWithoutCertificate checks D-b and D-e: both ports talk
// TLS only, and only to a client with a certificate from the guest's
// CA.
func assertRefusesWithoutCertificate(t *testing.T, address, dockerPort, buildkitPort, clientDir string) {
	t.Helper()
	ca, err := os.ReadFile(filepath.Join(clientDir, "ca.pem"))
	if err != nil {
		t.Fatal(err)
	}
	trust := func() *tls.Config {
		pool := x509.NewCertPool()
		if !pool.AppendCertsFromPEM(ca) {
			t.Fatal("ca.pem holds no certificate")
		}
		return &tls.Config{RootCAs: pool, ServerName: address, MinVersion: tls.VersionTLS12}
	}
	for _, port := range []string{dockerPort, buildkitPort} {
		addr := net.JoinHostPort(address, port)

		// TLS without a client certificate: the server's certificate
		// verifies, and the server ends the session.
		get := &http.Client{Timeout: 10 * time.Second, Transport: &http.Transport{TLSClientConfig: trust(), ForceAttemptHTTP2: true, DisableKeepAlives: true}}
		resp, err := get.Get("https://" + addr + "/_ping")
		if err == nil {
			body, _ := io.ReadAll(resp.Body)
			_ = resp.Body.Close()
			t.Errorf("%s answered a client without a certificate: %s %q", addr, resp.Status, body)
		} else {
			t.Logf("%s without a client certificate: %v", addr, err)
		}

		// Plain TCP: no API without TLS.
		conn, err := net.DialTimeout("tcp", addr, 5*time.Second)
		if err != nil {
			t.Fatalf("dial %s: %v", addr, err)
		}
		_ = conn.SetDeadline(time.Now().Add(5 * time.Second))
		_, _ = io.WriteString(conn, "GET /_ping HTTP/1.1\r\nHost: x\r\n\r\n")
		reply, _ := io.ReadAll(io.LimitReader(conn, 4096))
		_ = conn.Close()
		if strings.Contains(string(reply), "\r\n\r\nOK") || strings.HasPrefix(string(reply), "HTTP/1.1 200") {
			t.Errorf("%s answered plain HTTP: %q", addr, reply)
		}
		t.Logf("%s plain HTTP: %q", addr, firstLine(string(reply)))
	}
}

func firstLine(s string) string {
	l, _, _ := strings.Cut(s, "\n")
	return strings.TrimSpace(l)
}

const buildContextDockerfile = "FROM busybox:1.37\nRUN echo built-in-the-dockerhost > /built\n"

func TestDockerhost_TestForwards(t *testing.T) {
	requireKVM(t)
	t.Setenv("Y_CLUSTER_INVENTORY_DIR", t.TempDir())
	bin := buildBinary(t)
	docker, buildctl := dockerhostClients(t, bin)
	log := logger(t)
	ctx := context.Background()

	dir := t.TempDir()
	ports := freePorts(t, 3)
	cfg := dockerhost.Config{
		TestForwards: &dockerhost.TestForwards{SSH: ports[0], Docker: ports[1], Buildkit: ports[2]},
		MemoryMB:     4096,
		CPUs:         2,
		DiskSize:     "20G",
	}
	cfg.ApplyDefaults()
	opts := dockerhost.Options{Dir: dir, Config: cfg, ImageDir: sharedDockerhostImages(t)}
	t.Cleanup(func() {
		pid, running := qemu.GuestPID(filepath.Join(dir, "vm"), "dockerhost")
		if err := dockerhost.Teardown(dir, log); err != nil {
			t.Errorf("teardown: %v", err)
		}
		if running && pidAlive(pid) {
			t.Errorf("qemu pid %d still alive after teardown", pid)
		}
	})

	start := time.Now()
	res, err := dockerhost.Provision(ctx, opts, log)
	if err != nil {
		t.Fatalf("provision: %v", err)
	}
	t.Logf("TIMING provision (created): %s", time.Since(start).Round(time.Second))
	if res.Outcome != dockerhost.Created {
		t.Fatalf("outcome %s", res.Outcome)
	}
	vmDir := filepath.Join(dir, "vm")
	pid, running := qemu.GuestPID(vmDir, "dockerhost")
	if !running {
		t.Fatal("no qemu after provision")
	}
	logGuestMemory(t, "after provision", pid)

	// The contract, from the binary a session runs.
	vars, raw := dockerhostEnv(t, bin, dir)
	t.Logf("env:\n%s", raw)
	client := filepath.Join(dir, "client")
	for k, want := range map[string]string{
		"DOCKER_HOST":       "tcp://127.0.0.1:" + ports[1],
		"DOCKER_TLS_VERIFY": "1",
		"DOCKER_CERT_PATH":  client,
		"BUILDKIT_HOST":     "tcp://127.0.0.1:" + ports[2],
		"BUILDKIT_TLS_DIR":  client,
	} {
		if vars[k] != want {
			t.Errorf("%s=%q, want %q", k, vars[k], want)
		}
	}
	statusCmd := exec.Command(bin, "dockerhost", "status")
	statusCmd.Env = append(os.Environ(), dockerhost.DirEnv+"="+dir)
	if out, err := statusCmd.CombinedOutput(); err != nil {
		t.Errorf("status: %v\n%s", err, out)
	} else {
		t.Logf("status:\n%s", out)
	}

	assertRefusesWithoutCertificate(t, "127.0.0.1", ports[1], ports[2], client)
	assertHostListenersLoopbackOnly(t, pid)

	env := clientEnv(vars)
	// dockerd: the pinned versions, a container run.
	if v := strings.TrimSpace(mustClient(t, env, docker, "version", "--format", "{{.Server.Version}}")); v != dockerhost.DockerVersion {
		t.Errorf("dockerd %s, want %s", v, dockerhost.DockerVersion)
	}
	components := mustClient(t, env, docker, "version", "--format", "{{range .Server.Components}}{{.Name}}={{.Version}} {{end}}")
	if !strings.Contains(components, "containerd=v"+dockerhost.ContainerdVersion) && !strings.Contains(components, "containerd="+dockerhost.ContainerdVersion) {
		t.Errorf("server components %s, want containerd %s", components, dockerhost.ContainerdVersion)
	}
	runStart := time.Now()
	if out := mustClient(t, env, docker, "run", "--rm", "busybox:1.37", "echo", "hello from the dockerhost"); !strings.Contains(out, "hello from the dockerhost") {
		t.Errorf("docker run: %q", out)
	}
	t.Logf("TIMING docker run busybox (cold pull): %s", time.Since(runStart).Round(time.Second))

	// buildkitd: a Dockerfile build, exported to the client.
	ctxDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(ctxDir, "Dockerfile"), []byte(buildContextDockerfile), 0o644); err != nil {
		t.Fatal(err)
	}
	mustBuildctl(t, env, buildctl, "--tlsdir", vars["BUILDKIT_TLS_DIR"], "debug", "workers")
	buildStart := time.Now()
	ociTar := filepath.Join(t.TempDir(), "image.tar")
	mustBuildctl(t, env, buildctl, "--tlsdir", vars["BUILDKIT_TLS_DIR"], "build",
		"--frontend", "dockerfile.v0", "--local", "context="+ctxDir, "--local", "dockerfile="+ctxDir,
		"--output", "type=oci,dest="+ociTar)
	t.Logf("TIMING buildctl build (cold): %s", time.Since(buildStart).Round(time.Second))
	if fi, err := os.Stat(ociTar); err != nil || fi.Size() < 1024 {
		t.Fatalf("buildctl output %s: %v", ociTar, err)
	}
	logGuestMemory(t, "after docker run and buildctl build", pid)

	// The guest: first boot done, pins held, Ubuntu patched, no host
	// directory, no CA key and no client key.
	assertGuestDefinition(t, dir, client)
	for _, gone := range []string{"dockerhost-cloud-init.yaml", "dockerhost-seed.img"} {
		if _, err := os.Stat(filepath.Join(vmDir, gone)); !os.IsNotExist(err) {
			t.Errorf("%s (the server key) must be gone from the host after the first boot", gone)
		}
	}

	// Idempotent: a second provision reuses the guest.
	again := time.Now()
	res2, err := dockerhost.Provision(ctx, opts, log)
	if err != nil {
		t.Fatalf("second provision: %v", err)
	}
	t.Logf("TIMING provision (reused): %s", time.Since(again).Round(time.Millisecond))
	if res2.Outcome != dockerhost.Reused || !res2.State.CreatedAt.Equal(res.State.CreatedAt) {
		t.Fatalf("second provision: %s, created %s (first %s)", res2.Outcome, res2.State.CreatedAt, res.State.CreatedAt)
	}

	// The idle reaper: with a one-minute timeout and nobody connected,
	// the guest powers itself off.
	guestExec(t, dir, "sudo /usr/local/sbin/y-cluster-dockerhost-idle lease 60 1209600")
	reapStart := time.Now()
	deadline := reapStart.Add(9 * time.Minute)
	for pidAlive(pid) && time.Now().Before(deadline) {
		time.Sleep(5 * time.Second)
	}
	if pidAlive(pid) {
		t.Fatal("the idle reaper did not power the guest off within 9 minutes")
	}
	t.Logf("TIMING idle reaper powered off %s after the lease", time.Since(reapStart).Round(time.Second))
	console, _ := os.ReadFile(filepath.Join(vmDir, "dockerhost-console.log"))
	if !strings.Contains(string(console), "y-cluster dockerhost: powering off, idle") {
		t.Errorf("console log lacks the reaper's reason")
	}
	// env: nothing for a fresh shell, unset for one that points here.
	if _, raw := dockerhostEnv(t, bin, dir); raw != "" {
		t.Errorf("env with a stopped guest: %q", raw)
	}
	if _, raw := dockerhostEnv(t, bin, dir, "DOCKER_CERT_PATH="+client); !strings.HasPrefix(raw, "unset DOCKER_HOST ") {
		t.Errorf("env for a shell still pointing at the stopped guest: %q", raw)
	}

	// Provision boots it again from its disk: the image pulled before
	// is still there.
	restart := time.Now()
	res3, err := dockerhost.Provision(ctx, opts, log)
	if err != nil {
		t.Fatalf("provision after the reaper: %v", err)
	}
	t.Logf("TIMING provision (restarted): %s", time.Since(restart).Round(time.Second))
	if res3.Outcome != dockerhost.Restarted {
		t.Fatalf("outcome %s, want restarted", res3.Outcome)
	}
	mustClient(t, env, docker, "image", "inspect", "busybox:1.37")
	pid, _ = qemu.GuestPID(vmDir, "dockerhost")
	logGuestMemory(t, "after restart", pid)

	// Teardown, the way a user runs it.
	td := exec.Command(bin, "dockerhost", "teardown")
	td.Env = append(os.Environ(), dockerhost.DirEnv+"="+dir)
	if out, err := td.CombinedOutput(); err != nil {
		t.Fatalf("teardown: %v\n%s", err, out)
	}
	if pidAlive(pid) {
		t.Fatalf("qemu pid %d alive after teardown", pid)
	}
	for _, gone := range []string{"state.json", "client", "vm"} {
		if _, err := os.Stat(filepath.Join(dir, gone)); !os.IsNotExist(err) {
			t.Errorf("%s left after teardown", gone)
		}
	}
}

// assertGuestDefinition looks inside the guest.
func assertGuestDefinition(t *testing.T, dir, clientDir string) {
	t.Helper()
	if out := guestExec(t, dir, "cloud-init status"); !strings.Contains(out, "status: done") {
		t.Errorf("cloud-init: %s", out)
	}
	held := guestExec(t, dir, "apt-mark showhold")
	for _, p := range []string{"docker-ce", "docker-ce-cli", "containerd.io"} {
		if !strings.Contains(held, p) {
			t.Errorf("%s is not held: %s", p, held)
		}
	}
	if out := guestExec(t, dir, "buildkitd --version"); !strings.Contains(out, dockerhost.BuildKitVersion) {
		t.Errorf("buildkitd %s, want %s", out, dockerhost.BuildKitVersion)
	}
	if out := guestExec(t, dir, "systemctl is-enabled unattended-upgrades.service; apt-config dump APT::Periodic::Unattended-Upgrade"); !strings.Contains(out, "enabled") || !strings.Contains(out, `"1"`) {
		t.Errorf("unattended-upgrades must stay on in the guest: %s", out)
	}
	if out := guestExec(t, dir, "systemctl is-active y-cluster-dockerhost-idle.timer buildkit.service docker.service"); strings.Count(out, "active") != 3 {
		t.Errorf("units: %s", out)
	}
	if out := guestExec(t, dir, "findmnt -rn -t 9p,virtiofs,nfs,cifs || true"); strings.TrimSpace(out) != "" {
		t.Errorf("the guest mounts something from outside: %s", out)
	}
	// The only private key in the guest's y-cluster files is the
	// server's; the client's key is nowhere in the guest.
	if out := guestExec(t, dir, "sudo grep -rl 'PRIVATE KEY' /etc/y-cluster-dockerhost /etc/docker /etc/buildkit || true"); strings.TrimSpace(out) != "/etc/y-cluster-dockerhost/tls/server-key.pem" {
		t.Errorf("private keys in the guest's configuration: %q", out)
	}
	key, err := os.ReadFile(filepath.Join(clientDir, "key.pem"))
	if err != nil {
		t.Fatal(err)
	}
	// The second base64 line: the end of the private scalar and the
	// public key. The first line is the PKCS#8 and P-256 header that
	// every such key shares, the server's included.
	lines := strings.Split(strings.TrimSpace(string(key)), "\n")
	if len(lines) < 4 {
		t.Fatalf("unexpected client key shape: %d lines", len(lines))
	}
	// Encoded on the command line: sudo logs command lines to
	// auth.log and the journal, which are searched too.
	needle := base64.StdEncoding.EncodeToString([]byte(lines[2]))
	search := `sudo sh -c 'n=$(echo ` + needle + ` | base64 -d); grep -rlF "$n" /etc /var/lib/cloud /var/log /home /root 2>/dev/null || true'`
	if out := guestExec(t, dir, search); strings.TrimSpace(out) != "" {
		t.Errorf("the client key is in the guest: %s", out)
	}
	listeners := guestExec(t, dir, "sudo ss -Htlnu")
	t.Logf("guest listeners:\n%s", listeners)
	for _, line := range strings.Split(strings.TrimSpace(listeners), "\n") {
		f := strings.Fields(line)
		if len(f) < 5 {
			continue
		}
		local := f[4]
		switch {
		case strings.HasSuffix(local, ":22"), strings.HasSuffix(local, ":2376"), strings.HasSuffix(local, ":8547"):
		case strings.HasPrefix(local, "127.0.0.") || strings.HasPrefix(local, "[::1]"):
		case strings.HasPrefix(f[0], "udp") && strings.HasSuffix(local, "%enp0s2:68"), strings.HasSuffix(local, ":68"), strings.HasSuffix(local, ":546"):
			// DHCP clients.
		default:
			t.Errorf("unexpected listener in the guest: %s", line)
		}
	}
}

// assertHostListenersLoopbackOnly: everything qemu binds on the host
// in the test harness is on 127.0.0.1.
func assertHostListenersLoopbackOnly(t *testing.T, pid int) {
	t.Helper()
	out, err := exec.Command("ss", "-Htlnp").Output()
	if err != nil {
		t.Logf("ss: %v (skipping the host listener check)", err)
		return
	}
	mark := "pid=" + strconv.Itoa(pid) + ","
	n := 0
	for _, line := range strings.Split(string(out), "\n") {
		if !strings.Contains(line, mark) {
			continue
		}
		n++
		f := strings.Fields(line)
		if len(f) >= 4 && !strings.HasPrefix(f[3], "127.0.0.1:") {
			t.Errorf("qemu listens beyond loopback: %s", line)
		}
	}
	if n != 3 {
		t.Errorf("want qemu's three forwards (ssh, dockerd, buildkitd), found %d listeners", n)
	}
}

func logGuestMemory(t *testing.T, when string, pid int) {
	t.Helper()
	status, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/status")
	if err != nil {
		return
	}
	for _, line := range strings.Split(string(status), "\n") {
		if strings.HasPrefix(line, "VmRSS:") || strings.HasPrefix(line, "VmHWM:") {
			t.Logf("MEMORY qemu %s %s", when, strings.Join(strings.Fields(line), " "))
		}
	}
}

func pidAlive(pid int) bool {
	if pid <= 0 {
		return false
	}
	_, err := os.Stat("/proc/" + strconv.Itoa(pid))
	return err == nil
}

func freePorts(t *testing.T, n int) []string {
	t.Helper()
	var ports []string
	var ls []net.Listener
	for range n {
		l, err := net.Listen("tcp4", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		ls = append(ls, l)
		_, p, _ := net.SplitHostPort(l.Addr().String())
		ports = append(ports, p)
	}
	for _, l := range ls {
		_ = l.Close()
	}
	return ports
}

// TestDockerhost_Tap is the product path on a prepared tap, with the
// exposure checks of the compliance review (D-e): what the host
// listens on, what a container in the guest reaches, and both TLS
// ports refusing a client without a certificate. Its log is the
// record of the checks.
func TestDockerhost_Tap(t *testing.T) {
	ifname := os.Getenv("Y_CLUSTER_E2E_DOCKERHOST_IFNAME")
	guestAddress := os.Getenv("Y_CLUSTER_E2E_DOCKERHOST_GUEST_ADDRESS")
	if ifname == "" || guestAddress == "" {
		t.Skip("set Y_CLUSTER_E2E_DOCKERHOST_IFNAME and Y_CLUSTER_E2E_DOCKERHOST_GUEST_ADDRESS to a tap root prepared (DOCKERHOST.md, One-time root setup)")
	}
	requireKVM(t)
	t.Setenv("Y_CLUSTER_INVENTORY_DIR", t.TempDir())
	bin := buildBinary(t)
	docker, buildctl := dockerhostClients(t, bin)
	log := logger(t)
	ctx := context.Background()

	dir := t.TempDir()
	cfg := dockerhost.Config{
		Network:  dockerhost.Network{Ifname: ifname, GuestAddress: guestAddress},
		MemoryMB: 4096,
		CPUs:     2,
		DiskSize: "20G",
	}
	cfg.ApplyDefaults()
	if err := cfg.Validate(); err != nil {
		t.Fatal(err)
	}
	guestIP, _, _ := net.ParseCIDR(guestAddress)
	t.Logf("tap %s, guest %s, gateway %s, dns %v", ifname, guestAddress, cfg.Network.Gateway, cfg.Network.DNS)

	before := hostListeners(t)
	vmDir := filepath.Join(dir, "vm")
	t.Cleanup(func() {
		pid, running := qemu.GuestPID(vmDir, "dockerhost")
		if err := dockerhost.Teardown(dir, log); err != nil {
			t.Errorf("teardown: %v", err)
		}
		if running && pidAlive(pid) {
			t.Errorf("qemu pid %d still alive after teardown", pid)
		}
	})
	start := time.Now()
	res, err := dockerhost.Provision(ctx, dockerhost.Options{Dir: dir, Config: cfg, ImageDir: sharedDockerhostImages(t)}, log)
	if err != nil {
		t.Fatalf("provision: %v", err)
	}
	t.Logf("TIMING provision (created, tap): %s", time.Since(start).Round(time.Second))
	pid, _ := qemu.GuestPID(vmDir, "dockerhost")
	logGuestMemory(t, "after provision", pid)

	// D-e: nothing new listens on the host's wildcard; qemu on a tap
	// listens on nothing at all. Other processes on a shared host come and
	// go meanwhile (on gle01 a user-mode qemu guest opened a udp socket on
	// 0.0.0.0 during the first run), so a new wildcard listener fails the
	// test only when it is this test's or the guest's; others are logged.
	after := hostListeners(t)
	ours := []string{"pid=" + strconv.Itoa(pid) + ",", "pid=" + strconv.Itoa(os.Getpid()) + ","}
	for l := range after {
		if before[l] {
			continue
		}
		t.Logf("EXPOSURE new host listener during the test: %s", l)
		if !strings.Contains(l, " 0.0.0.0:") && !strings.Contains(l, " [::]:") && !strings.Contains(l, " *:") {
			continue
		}
		if strings.Contains(l, ours[0]) || strings.Contains(l, ours[1]) || !strings.Contains(l, "pid=") {
			t.Errorf("EXPOSURE new wildcard listener on the host: %s", l)
		} else {
			t.Logf("EXPOSURE new wildcard listener of another process, not the dockerhost's: %s", l)
		}
	}
	for l := range after {
		if strings.Contains(l, "pid="+strconv.Itoa(pid)+",") {
			t.Errorf("EXPOSURE qemu on a tap listens on the host: %s", l)
		}
	}
	t.Logf("EXPOSURE host listeners before and after provision: %d and %d, no new wildcard listener", len(before), len(after))

	vars, raw := dockerhostEnv(t, bin, dir)
	t.Logf("env:\n%s", raw)
	if want := "tcp://" + guestIP.String() + ":2376"; vars["DOCKER_HOST"] != want {
		t.Errorf("DOCKER_HOST=%q, want %q", vars["DOCKER_HOST"], want)
	}
	if want := "tcp://" + guestIP.String() + ":8547"; vars["BUILDKIT_HOST"] != want {
		t.Errorf("BUILDKIT_HOST=%q, want %q", vars["BUILDKIT_HOST"], want)
	}
	// D-e: both TLS ports refuse a client without a certificate.
	assertRefusesWithoutCertificate(t, guestIP.String(), dockerhost.DockerPort, dockerhost.BuildkitPort, vars["DOCKER_CERT_PATH"])

	env := clientEnv(vars)
	mustClient(t, env, docker, "version")
	runStart := time.Now()
	mustClient(t, env, docker, "pull", "-q", "busybox:1.37")
	t.Logf("TIMING docker pull busybox (cold, through the host's NAT): %s", time.Since(runStart).Round(time.Second))

	// A published port is reached at the guest's own address, the way
	// Testcontainers' getHost() and the mapped port reach it.
	mustClient(t, env, docker, "run", "-d", "--name", "e2e-published", "-p", "18080:8080", "busybox:1.37",
		"sh", "-c", "mkdir -p /w && echo published-at-the-guest-address > /w/index.html && exec httpd -f -p 8080 -h /w")
	defer func() { _, _ = runClient(t, env, docker, "rm", "-f", "e2e-published") }()
	url := "http://" + net.JoinHostPort(guestIP.String(), "18080") + "/"
	var body string
	for deadline := time.Now().Add(30 * time.Second); time.Now().Before(deadline); time.Sleep(time.Second) {
		if resp, err := http.Get(url); err == nil {
			b, _ := io.ReadAll(resp.Body)
			_ = resp.Body.Close()
			body = string(b)
			break
		}
	}
	if !strings.Contains(body, "published-at-the-guest-address") {
		t.Errorf("published port at %s: %q", url, body)
	}
	_, _ = runClient(t, env, docker, "rm", "-f", "e2e-published")

	// D-e: what a container in the guest reaches. The positive control
	// proves the probe works and egress through the host's NAT is open,
	// so the refusals below are the filter's.
	probe := func(host, port string) bool {
		out, err := runClient(t, env, docker, "run", "--rm", "busybox:1.37", "nc", "-z", "-w", "3", host, port)
		t.Logf("EXPOSURE container -> %s:%s: reached=%v %s", host, port, err == nil, strings.TrimSpace(out))
		return err == nil
	}
	if !probe("1.1.1.1", "443") {
		t.Fatal("a container cannot reach the internet; the probes below would prove nothing")
	}
	if !probe("9.9.9.9", "53") {
		t.Error("the guest's resolver is not reachable through the host's NAT")
	}
	hostPublic := hostPublicIPv4(t)
	for _, target := range []struct{ host, port, what string }{
		{cfg.Network.Gateway, "22", "the host, at its address on the tap"},
		{hostPublic, "22", "the host, at its public address"},
		{"10.0.0.1", "443", "a private range (10/8)"},
		{"172.16.0.1", "443", "a private range (172.16/12)"},
		{"192.168.0.1", "443", "a private range (192.168/16)"},
		{"100.64.0.1", "443", "shared address space (100.64/10)"},
		{"169.254.169.254", "80", "the metadata address"},
	} {
		if target.host == "" {
			continue
		}
		if probe(target.host, target.port) {
			t.Errorf("EXPOSURE a container in the guest reaches %s (%s:%s)", target.what, target.host, target.port)
		}
	}

	// buildkitd at the guest's address.
	ctxDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(ctxDir, "Dockerfile"), []byte(buildContextDockerfile), 0o644); err != nil {
		t.Fatal(err)
	}
	buildStart := time.Now()
	mustBuildctl(t, env, buildctl, "--tlsdir", vars["BUILDKIT_TLS_DIR"], "build",
		"--frontend", "dockerfile.v0", "--local", "context="+ctxDir, "--local", "dockerfile="+ctxDir,
		"--output", "type=oci,dest="+filepath.Join(t.TempDir(), "image.tar"))
	t.Logf("TIMING buildctl build at the guest address: %s", time.Since(buildStart).Round(time.Second))

	assertGuestDefinition(t, dir, vars["DOCKER_CERT_PATH"])
	logGuestMemory(t, "after the checks", pid)

	td := exec.Command(bin, "dockerhost", "teardown")
	td.Env = append(os.Environ(), dockerhost.DirEnv+"="+dir)
	if out, err := td.CombinedOutput(); err != nil {
		t.Fatalf("teardown: %v\n%s", err, out)
	}
	if pidAlive(pid) {
		t.Fatalf("qemu pid %d alive after teardown", pid)
	}
	_ = res
}

// hostListeners is `ss -Htulpn` as a set of "netid local-address
// process" lines.
func hostListeners(t *testing.T) map[string]bool {
	t.Helper()
	out, err := exec.Command("ss", "-Htulpn").Output()
	if err != nil {
		t.Fatalf("ss: %v", err)
	}
	set := map[string]bool{}
	for _, line := range strings.Split(string(out), "\n") {
		f := strings.Fields(line)
		if len(f) < 5 {
			continue
		}
		proc := ""
		if len(f) > 6 {
			proc = f[6]
		}
		set[f[0]+" "+f[4]+" "+proc] = true
	}
	return set
}

// hostPublicIPv4 is the address this host uses towards the internet.
func hostPublicIPv4(t *testing.T) string {
	t.Helper()
	conn, err := net.Dial("udp4", "1.1.1.1:53") // no packet is sent
	if err != nil {
		t.Logf("no route to the internet from the host: %v", err)
		return ""
	}
	defer func() { _ = conn.Close() }()
	return conn.LocalAddr().(*net.UDPAddr).IP.String()
}
