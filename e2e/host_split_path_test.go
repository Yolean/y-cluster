//go:build e2e && kvm

package e2e

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"go.uber.org/zap"

	"github.com/Yolean/y-cluster/pkg/kubeconfig"
	"github.com/Yolean/y-cluster/pkg/provision/k3s"
	"github.com/Yolean/y-cluster/pkg/provision/qemu"
	"github.com/Yolean/y-cluster/pkg/shquote"
	"github.com/Yolean/y-cluster/pkg/sshexec"
)

// TestHost_SplitPath is a k3s host that y-cluster did not create, in
// miniature: a plain Ubuntu cloud image with nothing of y-cluster's
// cloud-init, an operator account whose sudo asks for a password, and
// the apiserver reachable only through an ssh tunnel. The test does
// what an operator's own tooling does around y-cluster:
//
//  1. `y-cluster k3s bundle` for a provider host config;
//  2. applies the bundle from its manifest alone, over ssh, every
//     privileged step through `sudo -S` with the password on stdin;
//  3. reads k3s.yaml with `sudo -S cat` and pipes it to `y-cluster
//     kubeconfig import`, pointed at the local end of `ssh -L`;
//  4. `y-cluster platform install`.
//
// The cluster then has the objects qemu provision gives (the list is
// what a provision of the same y-cluster version showed), a second
// platform install changes nothing and --check finds nothing; --check
// does find a change made behind its back.
//
// Needs /dev/kvm and about 4 GB of memory; the guest downloads
// nothing (airgap bundle), the host fetches the release artifacts
// into its cache on first use.
func TestHost_SplitPath(t *testing.T) {
	if _, err := os.Stat("/dev/kvm"); err != nil {
		t.Skip("QEMU tests require /dev/kvm")
	}
	if err := qemu.CheckPrerequisites(); err != nil {
		t.Skip(err)
	}
	if os.Getenv("KUBECONFIG") == "" {
		t.Skip("KUBECONFIG must be set")
	}
	const (
		name        = "y-cluster-e2e-host"
		contextName = "y-cluster-e2e-host"
		// Not "operator": Ubuntu has a system group of that name, and
		// cloud-init then fails to create the user.
		user = "deployer"
	)
	bin := buildBinary(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Minute)
	defer cancel()

	cacheDir := e2eQEMUCacheDir(t)
	password := randomHex(t, 16)
	key := filepath.Join(cacheDir, "deployer-ssh")
	if err := sshexec.GenerateKey(key); err != nil {
		t.Fatal(err)
	}
	sshPort := freePort(t)
	bootPlainGuest(t, ctx, cacheDir, name, user, password, key, sshPort)
	node := sshexec.Target{Host: "127.0.0.1", Port: sshPort, User: user, KeyPath: key}
	sudo := func(command string) ([]byte, error) {
		return sshexec.Exec(ctx, node, "sudo -S -p '' "+command, strings.NewReader(password+"\n"))
	}

	// The account is what the test claims: sudo works with the
	// password and refuses without it.
	if out, err := sshexec.Exec(ctx, node, "sudo -n true", nil); err == nil {
		t.Fatalf("sudo without a password succeeded: %s", out)
	}
	if out, err := sudo("true"); err != nil {
		t.Fatalf("sudo -S: %s: %v", out, err)
	}

	// 1. The bundle, from a provider host config with additive
	// server settings.
	cfgDir := t.TempDir()
	writeFile(t, filepath.Join(cfgDir, "y-cluster-provision.yaml"), `provider: host
context: `+contextName+`
k3s:
  serverConfig:
    secrets-encryption: true
    write-kubeconfig-mode: "0600"
gateway:
  dnsHintIP: 127.0.0.1
`)
	bundleDir := filepath.Join(t.TempDir(), "bundle")
	runCLI(t, bin, nil, "k3s", "bundle", "-c", cfgDir, "--out", bundleDir)

	// 2. Apply it from the manifest, as a converge task would.
	m, err := k3s.ReadBundle(bundleDir)
	if err != nil {
		t.Fatal(err)
	}
	out, err := sshexec.Exec(ctx, node, "mktemp -d", nil)
	if err != nil {
		t.Fatalf("mktemp: %s: %v", out, err)
	}
	staging := strings.TrimSpace(string(out))
	for _, f := range append(bundleFilePaths(m), m.Install.Script) {
		if err := sshexec.SCP(ctx, node, filepath.Join(bundleDir, f), staging+"/"+f); err != nil {
			t.Fatalf("copy %s: %v", f, err)
		}
	}
	for _, f := range m.Files {
		if out, err := sudo("install -D -m " + f.Mode + " " + shquote.Quote(staging+"/"+f.Path) + " " + shquote.Quote(f.Dest)); err != nil {
			t.Fatalf("install %s: %s: %v", f.Dest, out, err)
		}
		out, err := sudo("sha256sum " + shquote.Quote(f.Dest))
		if err != nil || !strings.HasPrefix(string(out), f.SHA256+" ") {
			t.Fatalf("%s on the node: %s %v, manifest says %s", f.Dest, out, err, f.SHA256)
		}
	}
	if out, err := sudo("env " + strings.Join(m.Install.EnvAssignments(), " ") + " sh " + shquote.Quote(staging+"/"+m.Install.Script)); err != nil {
		t.Fatalf("installer: %s: %v", out, err)
	}
	for _, c := range m.PostInstall {
		if out, err := sudo("sh -c " + shquote.Quote(c)); err != nil {
			t.Fatalf("post-install %q: %s: %v", c, out, err)
		}
	}
	waitFor(t, ctx, 4*time.Minute, "k3s apiserver", func() (bool, string) {
		out, err := sudo("k3s kubectl get --raw=/readyz")
		return err == nil && strings.TrimSpace(string(out)) == "ok", string(out)
	})

	// The node runs plain `k3s server` with everything in
	// config.yaml, and the additions took effect.
	if out, err := sshexec.Exec(ctx, node, "systemctl show k3s.service -p ExecStart --value", nil); err != nil || !strings.Contains(string(out), "argv[]=/usr/local/bin/k3s server ;") {
		t.Errorf("k3s unit ExecStart: %s %v", out, err)
	}
	if out, err := sudo("stat -c %a " + k3s.KubeconfigPath); err != nil || strings.TrimSpace(string(out)) != "600" {
		t.Errorf("write-kubeconfig-mode 0600: k3s.yaml mode %s %v", out, err)
	}
	// secrets-encryption: k3s writes the apiserver's encryption config
	// and passes it on. (`k3s secrets-encrypt status` can fail for a
	// while after /readyz answers.)
	if out, err := sudo("test -s /var/lib/rancher/k3s/server/cred/encryption-config.json"); err != nil {
		t.Errorf("secrets-encryption: no encryption config: %s %v", out, err)
	}
	if out, err := sudo("journalctl -u k3s --no-pager -o cat"); err != nil || !strings.Contains(string(out), "--encryption-provider-config=") {
		t.Errorf("secrets-encryption: kube-apiserver runs without --encryption-provider-config: %v", err)
	}
	if out, err := sudo("test ! -e /etc/rancher/k3s/config.yaml.d"); err != nil {
		t.Errorf("the bundle wrote into config.yaml.d: %s %v", out, err)
	}

	// 3. The kubeconfig, read as root and imported through a tunnel.
	raw, err := sudo("cat " + k3s.KubeconfigPath)
	if err != nil {
		t.Fatalf("read k3s.yaml: %s: %v", raw, err)
	}
	tunnelPort := freePort(t)
	openTunnel(t, key, sshPort, user, tunnelPort)
	t.Cleanup(func() {
		if m, err := kubeconfig.FromEnv(contextName, contextName, zap.NewNop()); err == nil {
			m.CleanupStale()
		}
	})
	runCLI(t, bin, raw, "kubeconfig", "import", "--context", contextName, "--server", "https://127.0.0.1:"+tunnelPort)
	waitFor(t, ctx, time.Minute, "apiserver through the tunnel", func() (bool, string) {
		out, err := exec.Command("kubectl", "--context="+contextName, "get", "--raw=/readyz").CombinedOutput()
		return err == nil && strings.TrimSpace(string(out)) == "ok", string(out)
	})

	// 4. The platform.
	runCLI(t, bin, nil, "platform", "install", "-c", cfgDir)
	assertNodeReady(t, contextName, os.Getenv("KUBECONFIG"))
	if got, want := platformObjects(t, contextName), []string{
		"ClusterIssuer//y-cluster-ca",
		"ClusterIssuer//y-cluster-selfsigned",
		"DaemonSet/kube-system/cilium",
		"DaemonSet/kube-system/cilium-envoy",
		"Deployment/cert-manager/cert-manager",
		"Deployment/cert-manager/cert-manager-cainjector",
		"Deployment/cert-manager/cert-manager-webhook",
		"Deployment/envoy-gateway-system/envoy-gateway",
		"Deployment/kube-system/cilium-operator",
		"Deployment/kube-system/coredns",
		"Deployment/kube-system/metrics-server",
		"Deployment/local-path-storage/local-path-provisioner",
		"GatewayClass//y-cluster",
	}; strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Errorf("platform objects:\n%s\nwant (as qemu provision has them):\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}

	// A second install changes nothing, and --check agrees. The
	// converged state has no certgen Job: Envoy Gateway's chart hook
	// deletes itself 30s after it completed.
	waitFor(t, ctx, 3*time.Minute, "certgen Job to delete itself", func() (bool, string) {
		out, err := exec.Command("kubectl", "--context="+contextName, "-n", "envoy-gateway-system",
			"get", "job", "--ignore-not-found", "-o", "name").CombinedOutput()
		return err == nil && strings.TrimSpace(string(out)) == "", string(out)
	})
	before, rvBefore := objectVersions(t, contextName)
	runCLI(t, bin, nil, "platform", "install", "-c", cfgDir)
	after, rvAfter := objectVersions(t, contextName)
	if after != before {
		t.Errorf("a second platform install changed objects:\nbefore:\n%s\nafter:\n%s", before, after)
	}
	t.Logf("resourceVersions moved by the second install (metadata only, see objectVersions):\n%s", changedLines(rvBefore, rvAfter))
	if out, err := cliOutput(bin, nil, "platform", "install", "-c", cfgDir, "--check"); err != nil {
		t.Errorf("--check on a converged cluster: %v\n%s", err, out)
	}

	// --check finds a change it did not make, and install undoes it.
	if out, err := exec.Command("kubectl", "--context="+contextName, "annotate", "--overwrite", "gatewayclass", "y-cluster", "yolean.se/dns-hint-ip=198.51.100.7").CombinedOutput(); err != nil {
		t.Fatalf("annotate: %s: %v", out, err)
	}
	out, err = cliOutput(bin, nil, "platform", "install", "-c", cfgDir, "--check")
	if err == nil || !strings.Contains(string(out), "198.51.100.7") || !strings.Contains(string(out), "GatewayClass: would change") {
		t.Errorf("--check after a change: %v\n%s", err, out)
	}
	runCLI(t, bin, nil, "platform", "install", "-c", cfgDir)
	if out, err := cliOutput(bin, nil, "platform", "install", "-c", cfgDir, "--check"); err != nil {
		t.Errorf("--check after install restored the change: %v\n%s", err, out)
	}
}

func bundleFilePaths(m *k3s.BundleManifest) []string {
	var out []string
	for _, f := range m.Files {
		out = append(out, f.Path)
	}
	return out
}

func randomHex(t *testing.T, n int) string {
	t.Helper()
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		t.Fatal(err)
	}
	return hex.EncodeToString(b)
}

func freePort(t *testing.T) string {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = l.Close() }()
	return strconv.Itoa(l.Addr().(*net.TCPAddr).Port)
}

func writeFile(t *testing.T, path, body string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}

// bootPlainGuest starts the Ubuntu cloud image with a cloud-init of
// its own: one user with a password for sudo and the key for ssh,
// nothing else. Ubuntu's sudoers lets group sudo run anything with
// the user's password.
func bootPlainGuest(t *testing.T, ctx context.Context, cacheDir, name, user, password, key, sshPort string) {
	t.Helper()
	pub, err := os.ReadFile(key + ".pub")
	if err != nil {
		t.Fatal(err)
	}
	userData := filepath.Join(cacheDir, "user-data.yaml")
	writeFile(t, userData, fmt.Sprintf(`#cloud-config
hostname: %s
users:
  - name: %s
    shell: /bin/bash
    groups: [sudo]
    lock_passwd: false
    plain_text_passwd: %s
    ssh_authorized_keys:
      - %s
ssh_pwauth: false
package_update: false
`, name, user, password, strings.TrimSpace(string(pub))))
	metaData := filepath.Join(cacheDir, "meta-data.yaml")
	writeFile(t, metaData, fmt.Sprintf("instance-id: %s-%d\nlocal-hostname: %s\n", name, time.Now().UnixNano(), name))
	seed := filepath.Join(cacheDir, "seed.img")
	disk := filepath.Join(cacheDir, name+".qcow2")
	pidFile := filepath.Join(cacheDir, name+".pid")
	image, err := qemu.EnsureCloudImage(ctx, cacheDir, zap.NewNop())
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range [][]string{
		{"cloud-localds", seed, userData, metaData},
		{"qemu-img", "create", "-f", "qcow2", "-b", image, "-F", "qcow2", disk, "20G"},
		{"qemu-system-x86_64", "-name", name, "-machine", "accel=kvm", "-cpu", "host", "-smp", "2", "-m", "4096",
			"-drive", "file=" + disk + ",format=qcow2,if=virtio",
			"-drive", "file=" + seed + ",format=raw,if=virtio",
			"-netdev", "user,id=net0,hostfwd=tcp:127.0.0.1:" + sshPort + "-:22",
			"-device", "virtio-net-pci,netdev=net0",
			"-serial", "file:" + filepath.Join(cacheDir, name+"-console.log"),
			"-display", "none", "-daemonize", "-pidfile", pidFile},
	} {
		if out, err := exec.CommandContext(ctx, c[0], c[1:]...).CombinedOutput(); err != nil {
			t.Fatalf("%s: %s: %v", c[0], out, err)
		}
	}
	t.Cleanup(func() { stopPid(t, pidFile) })
	node := sshexec.Target{Host: "127.0.0.1", Port: sshPort, User: user, KeyPath: key}
	waitFor(t, ctx, 10*time.Minute, "ssh to the guest", func() (bool, string) {
		out, err := sshexec.Exec(ctx, node, "cloud-init status --wait >/dev/null; true", nil)
		return err == nil, string(out)
	})
}

func stopPid(t *testing.T, pidFile string) {
	body, err := os.ReadFile(pidFile)
	if err != nil {
		return
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(body)))
	if err != nil {
		return
	}
	_ = syscall.Kill(pid, syscall.SIGTERM)
	for i := 0; i < 100; i++ {
		if errors.Is(syscall.Kill(pid, 0), syscall.ESRCH) {
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
	_ = syscall.Kill(pid, syscall.SIGKILL)
}

// openTunnel runs `ssh -N -L` from localPort to the node's 6443 for
// the rest of the test.
func openTunnel(t *testing.T, key, sshPort, user, localPort string) {
	t.Helper()
	cmd := exec.Command("ssh", "-N", "-i", key, "-p", sshPort,
		"-o", "StrictHostKeyChecking=no", "-o", "UserKnownHostsFile=/dev/null", "-o", "LogLevel=ERROR",
		"-o", "ExitOnForwardFailure=yes",
		"-L", "127.0.0.1:"+localPort+":127.0.0.1:6443", user+"@127.0.0.1")
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
	})
	deadline := time.Now().Add(30 * time.Second)
	for {
		if c, err := net.Dial("tcp", "127.0.0.1:"+localPort); err == nil {
			_ = c.Close()
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("ssh tunnel did not open")
		}
		time.Sleep(200 * time.Millisecond)
	}
}

func waitFor(t *testing.T, ctx context.Context, timeout time.Duration, what string, ok func() (bool, string)) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for {
		done, last := ok()
		if done {
			return
		}
		if time.Now().After(deadline) || ctx.Err() != nil {
			t.Fatalf("%s: not ready after %s: %s", what, timeout, last)
		}
		time.Sleep(3 * time.Second)
	}
}

func cliOutput(bin string, stdin []byte, args ...string) ([]byte, error) {
	cmd := exec.Command(bin, args...)
	if stdin != nil {
		cmd.Stdin = bytes.NewReader(stdin)
	}
	return cmd.CombinedOutput()
}

func runCLI(t *testing.T, bin string, stdin []byte, args ...string) {
	t.Helper()
	if out, err := cliOutput(bin, stdin, args...); err != nil {
		t.Fatalf("y-cluster %s: %v\n%s", strings.Join(args, " "), err, out)
	}
}

// platformObjects lists the kinds the behaviour comparison of
// provision looks at, as Kind/namespace/name.
func platformObjects(t *testing.T, contextName string) []string {
	t.Helper()
	out, err := exec.Command("kubectl", "--context="+contextName, "get", "-A", "deploy,ds,sts,gatewayclass,clusterissuer",
		"-o", `jsonpath={range .items[*]}{.kind}/{.metadata.namespace}/{.metadata.name}{"\n"}{end}`).Output()
	if err != nil {
		t.Fatalf("list platform objects: %v", err)
	}
	lines := strings.Split(strings.TrimSpace(string(out)), "\n")
	sort.Strings(lines)
	return lines
}

// objectVersions is, for every object a platform install applies or
// rolls out, its generation and a digest of its labels, annotations
// and spec, plus the names of the Jobs and ReplicaSets a change would
// leave behind. Status and managedFields are left out: the latter is
// rewritten once by the apiserver on the first re-apply of an object
// another manager has applied to since its creation (cert-manager's
// webhook configurations, which cainjector patches), without any
// change to the object. resourceVersions are returned separately for
// the log.
func objectVersions(t *testing.T, contextName string) (string, string) {
	t.Helper()
	out, err := exec.Command("kubectl", "--context="+contextName, "get", "-A",
		"deploy,ds,sts,rs,job,gatewayclass,clusterissuer,certificate,envoyproxy,storageclass,crd,mutatingwebhookconfiguration,validatingwebhookconfiguration",
		"-o", "json").Output()
	if err != nil {
		t.Fatalf("list objects: %v", err)
	}
	var list struct {
		Items []struct {
			Kind     string `json:"kind"`
			Metadata struct {
				Name            string            `json:"name"`
				Namespace       string            `json:"namespace"`
				Generation      int64             `json:"generation"`
				ResourceVersion string            `json:"resourceVersion"`
				Labels          map[string]string `json:"labels"`
				Annotations     map[string]string `json:"annotations"`
			} `json:"metadata"`
			Spec     json.RawMessage `json:"spec"`
			Webhooks json.RawMessage `json:"webhooks"`
		} `json:"items"`
	}
	if err := json.Unmarshal(out, &list); err != nil {
		t.Fatal(err)
	}
	var objects, versions []string
	for _, o := range list.Items {
		h := sha256.New()
		for _, part := range []any{o.Metadata.Labels, o.Metadata.Annotations, o.Spec, o.Webhooks} {
			b, _ := json.Marshal(part)
			h.Write(b)
		}
		id := o.Kind + "/" + o.Metadata.Namespace + "/" + o.Metadata.Name
		objects = append(objects, fmt.Sprintf("%s %d %x", id, o.Metadata.Generation, h.Sum(nil)[:8]))
		versions = append(versions, id+" "+o.Metadata.ResourceVersion)
	}
	sort.Strings(objects)
	sort.Strings(versions)
	return strings.Join(objects, "\n"), strings.Join(versions, "\n")
}

func changedLines(before, after string) string {
	was := map[string]bool{}
	for _, l := range strings.Split(before, "\n") {
		was[l] = true
	}
	var out []string
	for _, l := range strings.Split(after, "\n") {
		if !was[l] {
			out = append(out, l)
		}
	}
	if len(out) == 0 {
		return "(none)"
	}
	return strings.Join(out, "\n")
}
