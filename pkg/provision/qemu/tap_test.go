package qemu

import (
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"sigs.k8s.io/yaml"

	"github.com/Yolean/y-cluster/pkg/provision/config"
)

func tapConfig(t *testing.T) Config {
	t.Helper()
	c := &config.QEMUConfig{
		CommonConfig: config.CommonConfig{Name: "tapvm", Context: "tapvm"},
		Network:      config.QEMUNetwork{Mode: "tap", Ifname: "ycl0", GuestAddress: "10.88.0.2/24"},
	}
	c.ApplyDefaults()
	if err := c.Validate(); err != nil {
		t.Fatal(err)
	}
	return FromConfig(c)
}

func TestMacForName(t *testing.T) {
	a, b := macForName("cluster-a"), macForName("cluster-b")
	if a != macForName("cluster-a") {
		t.Error("MAC must be stable for a name")
	}
	if a == b {
		t.Errorf("distinct names share MAC %s", a)
	}
	hw, err := net.ParseMAC(a)
	if err != nil {
		t.Fatalf("%q is not a MAC: %v", a, err)
	}
	if !strings.HasPrefix(a, "52:54:00:") {
		t.Errorf("want the 52:54:00 prefix, got %s", a)
	}
	if hw[0]&0x01 != 0 {
		t.Errorf("%s is a multicast address", a)
	}
	// Pinned so a change to the derivation is a visible decision.
	if got := macForName("y-cluster"); got != "52:54:00:b4:e0:d5" {
		t.Errorf("macForName(y-cluster) = %s; the derivation changed", got)
	}
}

func TestTap_UserModeHasNoTap(t *testing.T) {
	c := &config.QEMUConfig{}
	c.ApplyDefaults()
	if FromConfig(c).Tap != nil {
		t.Fatal("user mode must not carry tap state")
	}
}

func TestTap_Endpoints(t *testing.T) {
	got := tapConfig(t).endpoints()
	want := endpoints{SSHHost: "10.88.0.2", SSHPort: "22", APIHost: "10.88.0.2", APIPort: "6443", IngressIP: "10.88.0.2"}
	if got != want {
		t.Fatalf("got  %+v\nwant %+v", got, want)
	}
}

func TestTap_KubeconfigAndTLSSAN(t *testing.T) {
	e := tapConfig(t).endpoints()
	got, err := e.apiAddress()
	if err != nil {
		t.Fatal(err)
	}
	if got != "10.88.0.2:6443" {
		t.Fatalf("kubeconfig would name %s", got)
	}
	if flags := k3sServerFlags(e); !strings.HasSuffix(flags, " --tls-san=10.88.0.2") {
		t.Fatalf("k3s flags: %s", flags)
	}
}

func TestTap_QemuArgs(t *testing.T) {
	cfg := tapConfig(t)
	if got, want := netdevArg(cfg), "tap,id=net0,ifname=ycl0,script=no,downscript=no"; got != want {
		t.Errorf("netdev:\ngot  %s\nwant %s", got, want)
	}
	if got, want := nicArg(cfg), "virtio-net-pci,netdev=net0,mac="+macForName("tapvm"); got != want {
		t.Errorf("nic:\ngot  %s\nwant %s", got, want)
	}
	joined := strings.Join(vmArgs(cfg, vmDisks{Boot: "/c/vm.qcow2"}, "/c/console.log", "/c/vm.pid"), " ")
	if strings.Contains(joined, "hostfwd") {
		t.Errorf("tap mode must not forward host ports: %s", joined)
	}
}

// User-mode launches are unchanged by the existence of tap mode.
func TestTap_UserModeNicHasNoMAC(t *testing.T) {
	if got := nicArg(Config{}); got != "virtio-net-pci,netdev=net0" {
		t.Fatalf("user-mode nic: %s", got)
	}
}

func TestRenderNetworkConfig(t *testing.T) {
	got := renderNetworkConfig(*tapConfig(t).Tap)
	want := `version: 2
ethernets:
  guest-nic:
    match:
      macaddress: "` + macForName("tapvm") + `"
    dhcp4: false
    addresses:
      - 10.88.0.2/24
    routes:
      - to: default
        via: 10.88.0.1
    nameservers:
      addresses:
        - 1.1.1.1
        - 9.9.9.9
`
	if got != want {
		t.Fatalf("network-config:\ngot:\n%s\nwant:\n%s", got, want)
	}
}

type fakeTapHost struct {
	addrs    []net.IP
	addrsErr error
	flags    uint64
	flagsErr error
	owner    int
	uid      int
}

func (f fakeTapHost) Addrs(string) ([]net.IP, error)  { return f.addrs, f.addrsErr }
func (f fakeTapHost) TunFlags(string) (uint64, error) { return f.flags, f.flagsErr }
func (f fakeTapHost) Owner(string) (int, error)       { return f.owner, nil }
func (f fakeTapHost) UID() int                        { return f.uid }
func (f fakeTapHost) Username() string                { return "alice" }

func TestCheckTap(t *testing.T) {
	tap := *tapConfig(t).Tap
	ready := fakeTapHost{addrs: []net.IP{net.ParseIP("10.88.0.1")}, flags: iffTap | 0x1000, owner: 1000, uid: 1000}

	for _, tc := range []struct {
		name    string
		host    func(fakeTapHost) fakeTapHost
		wantErr string
	}{
		{"prepared host", func(h fakeTapHost) fakeTapHost { return h }, ""},
		{"root may attach to any device", func(h fakeTapHost) fakeTapHost { h.uid, h.owner = 0, 1000; return h }, ""},
		{"interface missing", func(h fakeTapHost) fakeTapHost { h.addrsErr = errors.New("no such interface"); return h }, "does not exist"},
		{"not a tun/tap device", func(h fakeTapHost) fakeTapHost { h.flagsErr = errors.New("no tun_flags"); return h }, "is not a tun/tap device"},
		{"tun instead of tap", func(h fakeTapHost) fakeTapHost { h.flags = 0x0001; return h }, "needs mode tap"},
		{"owned by someone else", func(h fakeTapHost) fakeTapHost { h.owner = 1001; return h }, "owned by uid 1001"},
		{"no owner", func(h fakeTapHost) fakeTapHost { h.owner = -1; return h }, "owned by uid -1"},
		{"gateway address missing", func(h fakeTapHost) fakeTapHost { h.addrs = nil; return h }, "does not have the gateway address 10.88.0.1"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := checkTap(tap, tc.host(ready))
			if tc.wantErr == "" {
				if err != nil {
					t.Fatalf("unexpected error: %v", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("want %q, got %v", tc.wantErr, err)
			}
			// Every failure carries the exact commands, with the
			// configured values substituted.
			for _, cmd := range []string{
				"sudo ip tuntap add dev ycl0 mode tap user alice",
				"sudo ip addr add 10.88.0.1/24 dev ycl0",
				"sudo ip link set ycl0 up",
				"sudo sysctl -w net.ipv4.ip_forward=1",
			} {
				if !strings.Contains(err.Error(), cmd) {
					t.Errorf("error lacks %q:\n%v", cmd, err)
				}
			}
		})
	}
}

func TestSSHTimeoutError(t *testing.T) {
	console := filepath.Join(t.TempDir(), "console.log")
	var lines []string
	for i := 1; i <= 60; i++ {
		lines = append(lines, fmt.Sprintf("boot line %d", i))
	}
	if err := os.WriteFile(console, []byte(strings.Join(lines, "\n")+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	user := sshTimeoutError(Config{SSHPort: "2222"}, console).Error()
	if strings.Contains(user, "boot line") {
		t.Errorf("user mode keeps the short message: %s", user)
	}

	tap := sshTimeoutError(tapConfig(t), console).Error()
	for _, want := range []string{"tried 10.88.0.2:22", "ycl0", "boot line 60", "boot line 21"} {
		if !strings.Contains(tap, want) {
			t.Errorf("tap timeout lacks %q:\n%s", want, tap)
		}
	}
	if strings.Contains(tap, "boot line 20\n") {
		t.Errorf("want only the console tail:\n%s", tap)
	}
}

// A tap cluster's static address must not ship in an exported
// appliance. cloud-init renders network-config to
// /etc/netplan/50-cloud-init.yaml; prepare-export has to REPLACE that
// file (not add a sibling, which netplan would merge with it) and
// stop cloud-init from regenerating it.
func TestPrepareInguestScript_ReplacesStaticNetplan(t *testing.T) {
	if !strings.Contains(prepareInguestScript, "cat > /etc/netplan/50-cloud-init.yaml <<") {
		t.Error("prepare-export no longer overwrites /etc/netplan/50-cloud-init.yaml; a tap cluster's static address would survive into the appliance")
	}
	if !strings.Contains(prepareInguestScript, "network: {config: disabled}") {
		t.Error("cloud-init network-config regeneration is no longer disabled")
	}
	if strings.Contains(renderNetworkConfig(*tapConfig(t).Tap), "set-name") {
		t.Error("the tap NIC must keep its kernel name: the exported netplan matches e* interfaces")
	}
}

// Found by the first tap e2e run: after stop/start the guest had lost
// its address. A restart boots without the seed, cloud-init falls
// back to DataSourceNone and re-renders the network as DHCP, which
// nothing answers on a tap device.
func TestCloudInitUserData_StaticNetworkSurvivesRestart(t *testing.T) {
	tap := renderCloudInitUserData("vm", "ssh-ed25519 KEY t@h\n", false, true)
	for _, want := range []string{
		"/etc/cloud/cloud.cfg.d/99-y-cluster-keep-network-config.cfg",
		"network: {config: disabled}",
	} {
		if !strings.Contains(tap, want) {
			t.Errorf("tap user-data lacks %q:\n%s", want, tap)
		}
	}
	var parsed map[string]any
	if err := yaml.Unmarshal([]byte(tap), &parsed); err != nil {
		t.Fatalf("tap user-data is not valid YAML: %v\n%s", err, tap)
	}
	if files, _ := parsed["write_files"].([]any); len(files) != 2 {
		t.Errorf("want 2 write_files entries, got %v", parsed["write_files"])
	}

	user := renderCloudInitUserData("vm", "ssh-ed25519 KEY t@h\n", false, false)
	if strings.Contains(user, "keep-network-config") {
		t.Errorf("user mode relies on the DHCP fallback and must stay as it was:\n%s", user)
	}
}
