package dockerhost

import (
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"time"

	"sigs.k8s.io/yaml"

	"github.com/Yolean/y-cluster/pkg/provision/config"
	"github.com/Yolean/y-cluster/pkg/provision/qemu"
)

// Config is the machine's dockerhost configuration, read from
// DefaultConfigPath. The network names a tap device that root prepared
// once (DOCKERHOST.md, One-time root setup); y-cluster attaches to it
// and never creates, configures or filters a network device.
//
//	network:
//	  ifname: ycl1               # tap owned by this user; may be a bridge port
//	  guestAddress: 10.88.1.2/24 # static; clients reach the daemons here
//	  # gateway: 10.88.1.1       # default: first host address of the subnet
//	  # dns: [1.1.1.1, 9.9.9.9]  # default; must be reachable through the host's NAT
//	# memory: 6144               # MB
//	# cpus: 4
//	# diskSize: 60G              # sparse qcow2: images and build cache
//	# idleTimeout: 8h            # guest powers off after this long unused; 0 = never
//	# maxAge: 336h               # guest is recreated from the newest image after this
type Config struct {
	Network     Network `json:"network"`
	MemoryMB    int     `json:"memory,omitempty"`
	CPUs        int     `json:"cpus,omitempty"`
	DiskSize    string  `json:"diskSize,omitempty"`
	IdleTimeout string  `json:"idleTimeout,omitempty"`
	MaxAge      string  `json:"maxAge,omitempty"`

	// TestForwards replaces the tap with qemu user-mode networking and
	// forwards on 127.0.0.1. It exists for y-cluster's own e2e test on
	// hosts without a prepared tap, is not read from the config file,
	// and is not a way to use the dockerhost: published container
	// ports have no address the host can reach.
	TestForwards *TestForwards `json:"-"`
}

// Network is the tap device and the guest's place on its subnet.
type Network struct {
	Ifname       string   `json:"ifname"`
	GuestAddress string   `json:"guestAddress"`
	Gateway      string   `json:"gateway,omitempty"`
	DNS          []string `json:"dns,omitempty"`
}

// TestForwards are the host ports, on 127.0.0.1, of the test harness.
type TestForwards struct {
	SSH, Docker, Buildkit string
}

const (
	defaultMemoryMB    = 6144
	defaultCPUs        = 4
	defaultDiskSize    = "60G"
	defaultIdleTimeout = "8h"
	defaultMaxAge      = "336h"

	// maxMaxAge keeps a guest's certificates valid for its whole life
	// (certValidity), the day of grace past its maximum age included.
	maxMaxAge = 60 * 24 * time.Hour
	// minIdleTimeout keeps the reaper from powering off a guest
	// between two steps of one build.
	minIdleTimeout = 10 * time.Minute
)

// DefaultConfigPath is $XDG_CONFIG_HOME/y-cluster/dockerhost.yaml, or
// ~/.config/y-cluster/dockerhost.yaml.
func DefaultConfigPath() (string, error) {
	if xdg := os.Getenv("XDG_CONFIG_HOME"); xdg != "" {
		return filepath.Join(xdg, "y-cluster", "dockerhost.yaml"), nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".config", "y-cluster", "dockerhost.yaml"), nil
}

// ErrNoConfig is LoadConfig's error for a file that does not exist.
var ErrNoConfig = errors.New("no dockerhost configuration")

// LoadConfig reads, defaults and validates path. Unknown keys are an
// error, so a typo does not silently fall back to a default.
func LoadConfig(path string) (Config, error) {
	data, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return Config{}, fmt.Errorf("%w: %s does not exist", ErrNoConfig, path)
	}
	if err != nil {
		return Config{}, err
	}
	var c Config
	if err := yaml.UnmarshalStrict(data, &c); err != nil {
		return Config{}, fmt.Errorf("%s: %w", path, err)
	}
	c.ApplyDefaults()
	if err := c.Validate(); err != nil {
		return Config{}, fmt.Errorf("%s: %w", path, err)
	}
	return c, nil
}

// ApplyDefaults fills what the file left out. The network has no
// default: which device and subnet are the guest's is the host's
// decision (DOCKERHOST.md).
func (c *Config) ApplyDefaults() {
	if c.MemoryMB == 0 {
		c.MemoryMB = defaultMemoryMB
	}
	if c.CPUs == 0 {
		c.CPUs = defaultCPUs
	}
	if c.DiskSize == "" {
		c.DiskSize = defaultDiskSize
	}
	if c.IdleTimeout == "" {
		c.IdleTimeout = defaultIdleTimeout
	}
	if c.MaxAge == "" {
		c.MaxAge = defaultMaxAge
	}
	if c.TestForwards == nil {
		c.Network = tapNetwork(c.Network)
	}
}

// tapNetwork defaults the gateway and resolvers the way the qemu
// provider's tap mode does.
func tapNetwork(n Network) Network {
	q := config.QEMUConfig{Network: config.QEMUNetwork{
		Mode: config.QEMUNetworkModeTap, Ifname: n.Ifname, GuestAddress: n.GuestAddress, Gateway: n.Gateway, DNS: n.DNS,
	}}
	q.ApplyDefaults()
	n.Gateway, n.DNS = q.Network.Gateway, q.Network.DNS
	return n
}

// Validate checks the configuration; the network checks are the qemu
// provider's for tap mode.
func (c Config) Validate() error {
	if c.MemoryMB < 1024 {
		return fmt.Errorf("memory %d: the guest needs at least 1024 MB", c.MemoryMB)
	}
	if c.CPUs < 1 {
		return fmt.Errorf("cpus %d: must be at least 1", c.CPUs)
	}
	idle, err := c.idleTimeout()
	if err != nil {
		return err
	}
	if idle != 0 && idle < minIdleTimeout {
		return fmt.Errorf("idleTimeout %s: use 0 (never) or at least %s", c.IdleTimeout, minIdleTimeout)
	}
	age, err := c.maxAge()
	if err != nil {
		return err
	}
	if age <= 0 || age > maxMaxAge {
		return fmt.Errorf("maxAge %s: must be positive and at most %s; the guest is recreated from the newest image on this cycle", c.MaxAge, maxMaxAge)
	}
	if c.TestForwards != nil {
		for _, p := range []string{c.TestForwards.SSH, c.TestForwards.Docker, c.TestForwards.Buildkit} {
			if n, err := strconv.Atoi(p); err != nil || n < 1 || n > 65535 {
				return fmt.Errorf("test forwards: %q is not a port", p)
			}
		}
		return nil
	}
	if c.Network.Ifname == "" || c.Network.GuestAddress == "" {
		return fmt.Errorf("network.ifname and network.guestAddress are required: the tap device root prepared for the guest, and the guest's address on it (see DOCKERHOST.md)")
	}
	_, err = c.qemuConfig("/nonexistent")
	return err
}

func (c Config) idleTimeout() (time.Duration, error) {
	if c.IdleTimeout == "0" {
		return 0, nil
	}
	d, err := time.ParseDuration(c.IdleTimeout)
	if err != nil {
		return 0, fmt.Errorf("idleTimeout %q: %w", c.IdleTimeout, err)
	}
	return d, nil
}

func (c Config) maxAge() (time.Duration, error) {
	d, err := time.ParseDuration(c.MaxAge)
	if err != nil {
		return 0, fmt.Errorf("maxAge %q: %w", c.MaxAge, err)
	}
	return d, nil
}

// limits is the reaper's view of the configuration.
func (c Config) limits() (reaperLimits, error) {
	idle, err := c.idleTimeout()
	if err != nil {
		return reaperLimits{}, err
	}
	age, err := c.maxAge()
	if err != nil {
		return reaperLimits{}, err
	}
	return reaperLimits{IdleTimeout: int64(idle / time.Second), MaxAge: int64(age / time.Second)}, nil
}

// endpoint is where clients reach the daemons: the guest's own address
// on the tap, or 127.0.0.1 and the forwarded ports in the test harness.
type endpoint struct {
	Address      net.IP
	DockerPort   string
	BuildkitPort string
}

func (c Config) endpoint() endpoint {
	if f := c.TestForwards; f != nil {
		return endpoint{Address: net.IPv4(127, 0, 0, 1), DockerPort: f.Docker, BuildkitPort: f.Buildkit}
	}
	ip, _, _ := net.ParseCIDR(c.Network.GuestAddress)
	return endpoint{Address: ip, DockerPort: DockerPort, BuildkitPort: BuildkitPort}
}

// qemuConfig is the guest's launch shape for the qemu provider. Tap
// mode goes through the provider's own config so its defaults,
// validation and MAC derivation apply unchanged.
func (c Config) qemuConfig(vmDir string) (qemu.Config, error) {
	if f := c.TestForwards; f != nil {
		return qemu.Config{
			Name:        guestName,
			CacheDir:    vmDir,
			DiskSize:    c.DiskSize,
			Memory:      strconv.Itoa(c.MemoryMB),
			CPUs:        strconv.Itoa(c.CPUs),
			SSHPort:     f.SSH,
			BindAddress: "127.0.0.1",
			PortForwards: []qemu.PortForward{
				{Host: f.Docker, Guest: DockerPort},
				{Host: f.Buildkit, Guest: BuildkitPort},
			},
		}, nil
	}
	q := &config.QEMUConfig{
		CommonConfig: config.CommonConfig{
			Provider: config.ProviderQEMU,
			Name:     guestName,
			Memory:   strconv.Itoa(c.MemoryMB),
			CPUs:     strconv.Itoa(c.CPUs),
		},
		DiskSize: c.DiskSize,
		Network: config.QEMUNetwork{
			Mode:         config.QEMUNetworkModeTap,
			Ifname:       c.Network.Ifname,
			GuestAddress: c.Network.GuestAddress,
			Gateway:      c.Network.Gateway,
			DNS:          c.Network.DNS,
		},
	}
	q.ApplyDefaults()
	if err := q.Validate(); err != nil {
		return qemu.Config{}, err
	}
	rt := qemu.FromConfig(q)
	rt.CacheDir = vmDir
	// A guest is no cluster node: no kubeconfig context, and nothing
	// of the k3s defaults the provider's config filled in applies.
	rt.Context, rt.Kubeconfig = "", ""
	rt.K3s = qemu.K3s{}
	rt.Lifetime, rt.OnExpiry = "", ""
	return rt, nil
}
