package dockerhost

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeConfig(t *testing.T, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "dockerhost.yaml")
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

// The values of gle01's guest network (OPS-BMH-001 development guest),
// as an operator would write them.
const gle01Config = `network:
  ifname: ycl1
  guestAddress: 10.88.1.2/24
`

func TestLoadConfig_DefaultsAndTap(t *testing.T) {
	c, err := LoadConfig(writeConfig(t, gle01Config))
	if err != nil {
		t.Fatal(err)
	}
	if c.Network.Gateway != "10.88.1.1" {
		t.Errorf("gateway default %q, want the subnet's first host address", c.Network.Gateway)
	}
	if strings.Join(c.Network.DNS, ",") != "1.1.1.1,9.9.9.9" {
		t.Errorf("dns default %v", c.Network.DNS)
	}
	if c.MemoryMB != 6144 || c.CPUs != 4 || c.DiskSize != "60G" || c.IdleTimeout != "8h" || c.MaxAge != "336h" {
		t.Errorf("defaults %+v", c)
	}
	ep := c.endpoint()
	if ep.Address.String() != "10.88.1.2" || ep.DockerPort != "2376" || ep.BuildkitPort != "8547" {
		t.Errorf("endpoint %+v", ep)
	}

	q, err := c.qemuConfig("/state/vm")
	if err != nil {
		t.Fatal(err)
	}
	if q.Tap == nil || q.Tap.Ifname != "ycl1" || q.Tap.GuestAddress != "10.88.1.2/24" || q.Tap.Gateway != "10.88.1.1" {
		t.Fatalf("tap %+v", q.Tap)
	}
	if !strings.HasPrefix(q.Tap.MAC, "52:54:00:") {
		t.Errorf("MAC %q", q.Tap.MAC)
	}
	if q.Name != "dockerhost" || q.CacheDir != "/state/vm" || q.Memory != "6144" || q.CPUs != "4" || q.DiskSize != "60G" {
		t.Errorf("launch shape %+v", q)
	}
	if q.Context != "" || q.Kubeconfig != "" || q.SSHPort != "" || len(q.PortForwards) != 0 || q.Lifetime != "" {
		t.Errorf("a tap guest has no context, kubeconfig, forwards or lifetime: %+v", q)
	}
}

func TestLoadConfig_Errors(t *testing.T) {
	for _, tc := range []struct{ name, yaml, want string }{
		{"no file", "", "does not exist"},
		{"no network", "memory: 4096\n", "network.ifname and network.guestAddress are required"},
		{"unknown key", gle01Config + "memroy: 4096\n", "memroy"},
		{"address without prefix", "network: {ifname: ycl1, guestAddress: 10.88.1.2}\n", "guestAddress"},
		{"gateway outside subnet", "network: {ifname: ycl1, guestAddress: 10.88.1.2/24, gateway: 10.88.2.1}\n", "outside"},
		{"bad ifname", "network: {ifname: 'a b', guestAddress: 10.88.1.2/24}\n", "not a valid interface name"},
		{"max age too long", gle01Config + "maxAge: 2000h\n", "at most"},
		{"max age unparseable", gle01Config + "maxAge: 14d\n", "maxAge"},
		{"idle too short", gle01Config + "idleTimeout: 1m\n", "at least"},
		{"too little memory", gle01Config + "memory: 512\n", "1024"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "missing.yaml")
			if tc.yaml != "" {
				path = writeConfig(t, tc.yaml)
			}
			_, err := LoadConfig(path)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("want an error with %q, got %v", tc.want, err)
			}
		})
	}
}

func TestLoadConfig_IdleReaperOff(t *testing.T) {
	c, err := LoadConfig(writeConfig(t, gle01Config+"idleTimeout: \"0\"\n"))
	if err != nil {
		t.Fatal(err)
	}
	l, err := c.limits()
	if err != nil {
		t.Fatal(err)
	}
	if l.IdleTimeout != 0 || l.MaxAge != 336*3600 {
		t.Fatalf("limits %+v", l)
	}
}

func TestDefaultConfigPath(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", "/xdg")
	if p, _ := DefaultConfigPath(); p != "/xdg/y-cluster/dockerhost.yaml" {
		t.Errorf("got %s", p)
	}
	t.Setenv("XDG_CONFIG_HOME", "")
	t.Setenv("HOME", "/home/u")
	if p, _ := DefaultConfigPath(); p != "/home/u/.config/y-cluster/dockerhost.yaml" {
		t.Errorf("got %s", p)
	}
}

func TestTestForwards_LaunchShape(t *testing.T) {
	c := Config{TestForwards: &TestForwards{SSH: "22022", Docker: "22376", Buildkit: "28547"}}
	c.ApplyDefaults()
	if err := c.Validate(); err != nil {
		t.Fatal(err)
	}
	q, err := c.qemuConfig("/vm")
	if err != nil {
		t.Fatal(err)
	}
	if q.Tap != nil || q.BindAddress != "127.0.0.1" || q.SSHPort != "22022" {
		t.Fatalf("test forwards must be user mode on loopback: %+v", q)
	}
	if len(q.PortForwards) != 2 || q.PortForwards[0].Guest != DockerPort || q.PortForwards[1].Guest != BuildkitPort {
		t.Fatalf("forwards %+v", q.PortForwards)
	}
	if ep := c.endpoint(); ep.Address.String() != "127.0.0.1" || ep.DockerPort != "22376" {
		t.Fatalf("endpoint %+v", ep)
	}
}
