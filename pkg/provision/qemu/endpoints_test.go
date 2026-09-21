package qemu

import (
	"reflect"
	"strings"
	"testing"
)

func TestEndpoints(t *testing.T) {
	for _, tc := range []struct {
		name string
		cfg  Config
		want endpoints
	}{
		{
			name: "defaults forward api and ingress",
			cfg: Config{SSHPort: "2222", PortForwards: []PortForward{
				{Host: "8443", Guest: "443"},
				{Host: "26443", Guest: "6443"},
				{Host: "8080", Guest: "80"},
			}},
			want: endpoints{SSHHost: "127.0.0.1", SSHPort: "2222", APIHost: "127.0.0.1", APIPort: "26443", IngressIP: "127.0.0.1"},
		},
		{
			name: "no forwards: ssh only, no api port, no ingress hint",
			cfg:  Config{SSHPort: "2222"},
			want: endpoints{SSHHost: "127.0.0.1", SSHPort: "2222", APIHost: "127.0.0.1"},
		},
		{
			name: "ingress hint needs guest 80, not 443",
			cfg: Config{SSHPort: "2222", PortForwards: []PortForward{
				{Host: "6443", Guest: "6443"},
				{Host: "443", Guest: "443"},
			}},
			want: endpoints{SSHHost: "127.0.0.1", SSHPort: "2222", APIHost: "127.0.0.1", APIPort: "6443"},
		},
		{
			name: "wildcard bind is dialed over loopback",
			cfg: Config{SSHPort: "2222", BindAddress: "0.0.0.0", PortForwards: []PortForward{
				{Host: "6443", Guest: "6443"}, {Host: "80", Guest: "80"},
			}},
			want: endpoints{SSHHost: "127.0.0.1", SSHPort: "2222", APIHost: "127.0.0.1", APIPort: "6443", IngressIP: "127.0.0.1"},
		},
		{
			name: "specific bind address is where everything is dialed",
			cfg: Config{SSHPort: "2222", BindAddress: "192.168.1.10", PortForwards: []PortForward{
				{Host: "6443", Guest: "6443"}, {Host: "80", Guest: "80"},
			}},
			want: endpoints{SSHHost: "192.168.1.10", SSHPort: "2222", APIHost: "192.168.1.10", APIPort: "6443", IngressIP: "192.168.1.10"},
		},
		{
			name: "first forward to 6443 wins",
			cfg: Config{SSHPort: "2222", PortForwards: []PortForward{
				{Host: "16443", Guest: "6443"},
				{Host: "26443", Guest: "6443"},
			}},
			want: endpoints{SSHHost: "127.0.0.1", SSHPort: "2222", APIHost: "127.0.0.1", APIPort: "16443"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.cfg.endpoints(); got != tc.want {
				t.Fatalf("got  %+v\nwant %+v", got, tc.want)
			}
		})
	}
}

func TestEndpoints_SSHTarget(t *testing.T) {
	got := Config{SSHPort: "2229"}.endpoints().sshTarget("/keys/x-ssh")
	if got.Host != "127.0.0.1" || got.Port != "2229" || got.User != "ystack" || got.KeyPath != "/keys/x-ssh" {
		t.Fatalf("unexpected target: %+v", got)
	}
}

func TestSSHCommand(t *testing.T) {
	got := Config{Name: "vm", CacheDir: "/c", SSHPort: "2229", BindAddress: "192.168.1.10"}.SSHCommand()
	if want := "ssh -p 2229 -i /c/vm-ssh ystack@192.168.1.10"; got != want {
		t.Fatalf("got  %s\nwant %s", got, want)
	}
}

// k3sKubeconfig is the relevant part of /etc/rancher/k3s/k3s.yaml.
const k3sKubeconfig = "apiVersion: v1\nclusters:\n- cluster:\n    server: https://127.0.0.1:6443\n  name: default\n"

func TestRewriteKubeconfigServer(t *testing.T) {
	e := Config{PortForwards: []PortForward{{Host: "26443", Guest: "6443"}}}.endpoints()
	got, err := rewriteKubeconfigServer([]byte(k3sKubeconfig), e)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(got), "server: https://127.0.0.1:26443\n") {
		t.Fatalf("server not rewritten:\n%s", got)
	}
}

// A forward that keeps 6443 on the host must survive the rewrite
// unchanged rather than being mangled by a partial match.
func TestRewriteKubeconfigServer_SamePort(t *testing.T) {
	e := Config{PortForwards: []PortForward{{Host: "6443", Guest: "6443"}}}.endpoints()
	got, err := rewriteKubeconfigServer([]byte(k3sKubeconfig), e)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != k3sKubeconfig {
		t.Fatalf("kubeconfig changed:\n%s", got)
	}
}

func TestRewriteKubeconfigServer_NoAPIForward(t *testing.T) {
	_, err := rewriteKubeconfigServer([]byte(k3sKubeconfig), Config{}.endpoints())
	if err == nil || !strings.Contains(err.Error(), "guest:6443") {
		t.Fatalf("want an error naming the missing forward, got %v", err)
	}
}

func TestNetdevArg(t *testing.T) {
	for _, tc := range []struct {
		name string
		cfg  Config
		want string
	}{
		{
			name: "state from before bindAddress: empty host, which slirp binds to the wildcard",
			cfg:  Config{SSHPort: "2222"},
			want: "user,id=net0,hostfwd=tcp::2222-:22",
		},
		{
			name: "default loopback bind applies to ssh and every forward",
			cfg: Config{SSHPort: "2222", BindAddress: "127.0.0.1", PortForwards: []PortForward{
				{Host: "6443", Guest: "6443"},
			}},
			want: "user,id=net0,hostfwd=tcp:127.0.0.1:2222-:22,hostfwd=tcp:127.0.0.1:6443-:6443",
		},
		{
			name: "explicit wildcard",
			cfg:  Config{SSHPort: "2222", BindAddress: "0.0.0.0"},
			want: "user,id=net0,hostfwd=tcp:0.0.0.0:2222-:22",
		},
		{
			name: "forwards keep config order after ssh",
			cfg: Config{SSHPort: "2229", PortForwards: []PortForward{
				{Host: "39643", Guest: "6443"},
				{Host: "39080", Guest: "80"},
			}},
			want: "user,id=net0,hostfwd=tcp::2229-:22,hostfwd=tcp::39643-:6443,hostfwd=tcp::39080-:80",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := netdevArg(tc.cfg); got != tc.want {
				t.Fatalf("got  %s\nwant %s", got, tc.want)
			}
		})
	}
}

func TestVMArgs(t *testing.T) {
	cfg := Config{Name: "vm", CPUs: "2", Memory: "4096", SSHPort: "2222"}
	drives := func(args []string) []string {
		var out []string
		for i, a := range args {
			if a == "-drive" {
				out = append(out, args[i+1])
			}
		}
		return out
	}

	t.Run("first boot: boot, seed, then extra disks", func(t *testing.T) {
		args := vmArgs(cfg, vmDisks{Boot: "/c/vm.qcow2", Seed: "/c/vm-seed.img", Extra: []string{"/data/d.qcow2"}}, "/c/vm-console.log", "/c/vm.pid")
		want := []string{
			"file=/c/vm.qcow2,format=qcow2,if=virtio",
			"file=/c/vm-seed.img,format=raw,if=virtio",
			"file=/data/d.qcow2,format=qcow2,if=virtio",
		}
		if got := drives(args); !reflect.DeepEqual(got, want) {
			t.Fatalf("drives:\ngot  %v\nwant %v", got, want)
		}
	})

	t.Run("restart: no seed drive", func(t *testing.T) {
		args := vmArgs(cfg, vmDisks{Boot: "/c/vm.qcow2"}, "/c/vm-console.log", "/c/vm.pid")
		if got := drives(args); !reflect.DeepEqual(got, []string{"file=/c/vm.qcow2,format=qcow2,if=virtio"}) {
			t.Fatalf("drives: %v", got)
		}
	})

	t.Run("netdev id matches the nic, vm is daemonized with a pidfile", func(t *testing.T) {
		joined := strings.Join(vmArgs(cfg, vmDisks{Boot: "/c/vm.qcow2"}, "/c/vm-console.log", "/c/vm.pid"), " ")
		for _, want := range []string{
			"-name vm", "-smp 2", "-m 4096", "-machine accel=kvm",
			"-netdev user,id=net0,hostfwd=tcp::2222-:22",
			"-device virtio-net-pci,netdev=net0",
			"-serial file:/c/vm-console.log",
			"-daemonize -pidfile /c/vm.pid",
		} {
			if !strings.Contains(joined, want) {
				t.Errorf("missing %q in: %s", want, joined)
			}
		}
	})
}

// The host kubeconfig names the apiserver by APIHost, so an APIHost
// k3s would not put in its serving cert by itself needs a SAN.
func TestK3sServerFlags(t *testing.T) {
	base := "--write-kubeconfig-mode=644 --disable=traefik --disable=local-storage"
	if got := k3sServerFlags(Config{BindAddress: "127.0.0.1"}.endpoints()); got != base {
		t.Errorf("loopback: %q", got)
	}
	if got := k3sServerFlags(Config{BindAddress: "0.0.0.0"}.endpoints()); got != base {
		t.Errorf("wildcard is dialed over loopback: %q", got)
	}
	if got := k3sServerFlags(Config{BindAddress: "192.168.1.10"}.endpoints()); got != base+" --tls-san=192.168.1.10" {
		t.Errorf("specific address: %q", got)
	}
}

func TestRewriteKubeconfigServer_SpecificBindAddress(t *testing.T) {
	e := Config{BindAddress: "192.168.1.10", PortForwards: []PortForward{{Host: "6443", Guest: "6443"}}}.endpoints()
	got, err := rewriteKubeconfigServer([]byte(k3sKubeconfig), e)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(got), "server: https://192.168.1.10:6443\n") {
		t.Fatalf("server not rewritten:\n%s", got)
	}
}
