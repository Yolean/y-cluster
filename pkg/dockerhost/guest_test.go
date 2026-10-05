package dockerhost

import (
	"bytes"
	"encoding/hex"
	"encoding/json"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/ProtonMail/go-crypto/openpgp"
)

func testSpecFiles(t *testing.T) (map[string]string, map[string]string, tlsMaterial, []string) {
	t.Helper()
	m, err := issueTLS(net.ParseIP("10.88.1.2"), time.Now())
	if err != nil {
		t.Fatal(err)
	}
	spec, err := guestSpec("/images/ubuntu.img", m, reaperLimits{IdleTimeout: 28800, MaxAge: 1209600})
	if err != nil {
		t.Fatal(err)
	}
	if spec.CloudImage != "/images/ubuntu.img" {
		t.Errorf("cloud image %q", spec.CloudImage)
	}
	content, perms := map[string]string{}, map[string]string{}
	for _, f := range spec.Files {
		if _, dup := content[f.Path]; dup {
			t.Errorf("%s written twice", f.Path)
		}
		content[f.Path], perms[f.Path] = f.Content, f.Permissions
	}
	return content, perms, m, spec.RunCmd
}

func TestGuestSpec_TLSMaterial(t *testing.T) {
	content, perms, m, _ := testSpecFiles(t)
	if content[guestTLSDir+"/server-key.pem"] != string(m.ServerKey) || perms[guestTLSDir+"/server-key.pem"] != "0600" {
		t.Error("the server key goes to the guest, root-only")
	}
	if content[guestTLSDir+"/server-cert.pem"] != string(m.ServerCert) || content[guestTLSDir+"/ca.pem"] != string(m.CACert) {
		t.Error("the server certificate and the CA certificate go to the guest")
	}
	// The client's key stays on the host; the only key in the guest is
	// the server's.
	for path, c := range content {
		if strings.Contains(c, string(m.ClientKey)) || strings.Contains(c, string(m.ClientCert)) {
			t.Errorf("%s carries the client's certificate or key", path)
		}
		if strings.Contains(c, "PRIVATE KEY") && path != guestTLSDir+"/server-key.pem" {
			t.Errorf("%s carries a private key", path)
		}
	}
}

func TestGuestSpec_Daemons(t *testing.T) {
	content, perms, _, runcmd := testSpecFiles(t)

	var daemon map[string]any
	if err := json.Unmarshal([]byte(content["/etc/docker/daemon.json"]), &daemon); err != nil {
		t.Fatalf("daemon.json: %v", err)
	}
	if daemon["tls"] != true || daemon["tlsverify"] != true || daemon["tlscacert"] != guestTLSDir+"/ca.pem" {
		t.Errorf("dockerd must verify clients against the CA: %v", daemon)
	}
	if _, ok := daemon["hosts"]; ok {
		t.Error("hosts belong on the command line, next to -H fd://, not in daemon.json")
	}
	override := content["/etc/systemd/system/docker.service.d/y-cluster-dockerhost.conf"]
	if !strings.Contains(override, "ExecStart=\n") || !strings.Contains(override, "-H fd:// -H tcp://0.0.0.0:2376 ") {
		t.Errorf("docker override:\n%s", override)
	}
	if strings.Contains(override, ":2375") {
		t.Error("no plain TCP port")
	}

	toml := content["/etc/buildkit/buildkitd.toml"]
	for _, want := range []string{
		`"tcp://0.0.0.0:8547"`,
		`ca = "` + guestTLSDir + `/ca.pem"`,
		`cert = "` + guestTLSDir + `/server-cert.pem"`,
		`key = "` + guestTLSDir + `/server-key.pem"`,
	} {
		if !strings.Contains(toml, want) {
			t.Errorf("buildkitd.toml lacks %s:\n%s", want, toml)
		}
	}

	pins := content["/etc/y-cluster-dockerhost/pins.env"]
	for _, want := range []string{dockerDebVersion, containerdDebVersion, buildkitURL, buildkitSHA256} {
		if !strings.Contains(pins, want) {
			t.Errorf("pins.env lacks %s", want)
		}
	}
	if content["/etc/y-cluster-dockerhost/reaper.conf"] != "IDLE_TIMEOUT=28800\nMAX_AGE=1209600\n" {
		t.Errorf("reaper.conf %q", content["/etc/y-cluster-dockerhost/reaper.conf"])
	}

	for _, script := range []string{guestSetupCommand, guestIdleCommand} {
		if perms[script] != "0755" || !strings.HasPrefix(content[script], "#!/bin/sh\n") {
			t.Errorf("%s: mode %s", script, perms[script])
		}
	}
	if len(runcmd) != 1 || runcmd[0] != guestSetupCommand {
		t.Errorf("runcmd %v", runcmd)
	}
	for _, unit := range []string{"/etc/systemd/system/buildkit.service", "/etc/systemd/system/y-cluster-dockerhost-idle.timer"} {
		if !strings.Contains(content[unit], "[Install]") {
			t.Errorf("%s cannot be enabled", unit)
		}
	}
	if !strings.Contains(content[guestSetupCommand], "apt-mark hold docker-ce docker-ce-cli containerd.io") {
		t.Error("the setup holds the pinned packages")
	}
	if !strings.Contains(content[guestSetupCommand], "sha256sum -c") {
		t.Error("the setup checks BuildKit's digest")
	}
}

// Docker's apt key is the anchor for dockerd and containerd in the
// guest; changing it must be a deliberate edit here.
func TestDockerAptKey_Fingerprint(t *testing.T) {
	raw, err := guestFS.ReadFile("guest/docker-apt.asc")
	if err != nil {
		t.Fatal(err)
	}
	keyring, err := openpgp.ReadArmoredKeyRing(bytes.NewReader(raw))
	if err != nil {
		t.Fatal(err)
	}
	if len(keyring) != 1 {
		t.Fatalf("want one key, got %d", len(keyring))
	}
	if got := strings.ToUpper(hex.EncodeToString(keyring[0].PrimaryKey.Fingerprint)); got != dockerAptKeyFingerprint {
		t.Fatalf("fingerprint %s, want %s", got, dockerAptKeyFingerprint)
	}
}

func TestPins(t *testing.T) {
	if !strings.HasPrefix(dockerDebVersion, "5:"+DockerVersion+"-") || !strings.HasPrefix(containerdDebVersion, ContainerdVersion+"-") {
		t.Error("deb versions must carry the pinned versions")
	}
	if !strings.Contains(buildkitURL, "/"+BuildKitVersion+"/") || len(buildkitSHA256) != 64 {
		t.Error("BuildKit URL and digest must match the pin")
	}
	if !strings.Contains(dockerDebVersion, "noble") || !strings.Contains(containerdDebVersion, "noble") {
		t.Error("packages are for the guest's Ubuntu release, noble")
	}
}

func TestReaperLimits_LeaseCommand(t *testing.T) {
	if got := (reaperLimits{IdleTimeout: 600, MaxAge: 3600}).leaseCommand(); got != "sudo "+guestIdleCommand+" lease 600 3600" {
		t.Errorf("got %q", got)
	}
}
