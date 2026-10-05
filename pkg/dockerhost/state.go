package dockerhost

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"syscall"
	"time"
)

// guestName is the qemu VM's name. One dockerhost per machine: the
// name is not configurable, as host Docker has always been one daemon.
const guestName = "dockerhost"

// DirEnv overrides the state directory. y-cluster's tests use it; on a
// machine there is one dockerhost and one directory.
const DirEnv = "Y_CLUSTER_DOCKERHOST_DIR"

// DefaultDir is $Y_CLUSTER_DOCKERHOST_DIR, or
// ~/.cache/y-cluster-dockerhost next to the qemu provider's
// ~/.cache/y-cluster-qemu.
func DefaultDir() (string, error) {
	if d := os.Getenv(DirEnv); d != "" {
		return filepath.Abs(d)
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".cache", "y-cluster-dockerhost"), nil
}

// paths lays out the state directory, which is 0700: it holds the
// client's key, the guest's ssh key and, until the first boot is over,
// the guest's user-data with the server's key.
type paths string

func (p paths) dir() string    { return string(p) }
func (p paths) state() string  { return filepath.Join(string(p), "state.json") }
func (p paths) lock() string   { return filepath.Join(string(p), "lock") }
func (p paths) client() string { return filepath.Join(string(p), "client") }
func (p paths) vm() string     { return filepath.Join(string(p), "vm") }
func (p paths) images() string { return filepath.Join(string(p), "images") }

func (p paths) ensure() error {
	if err := os.MkdirAll(p.dir(), 0o700); err != nil {
		return err
	}
	return os.Chmod(p.dir(), 0o700)
}

// stateVersion guards the file's schema the way the qemu sidecar's
// does: a binary that does not know a newer version refuses it.
const stateVersion = 1

// State is the host's record of the guest, state.json in the state
// directory. Written 0600 and atomically.
type State struct {
	Version int `json:"version"`
	// Network is the tap configuration, or the test harness.
	Network      Network       `json:"network"`
	TestForwards *TestForwards `json:"testForwards,omitempty"`
	// Address, DockerPort and BuildkitPort are what clients dial; the
	// server certificate's SAN is Address.
	Address      string `json:"address"`
	DockerPort   string `json:"dockerPort"`
	BuildkitPort string `json:"buildkitPort"`

	MemoryMB    int    `json:"memory"`
	CPUs        int    `json:"cpus"`
	DiskSize    string `json:"diskSize"`
	IdleTimeout string `json:"idleTimeout"`
	MaxAge      string `json:"maxAge"`

	// CreatedAt is when the guest's disk was made; the maximum age
	// counts from here. ReadyAt is zero until the first boot finished
	// and both daemons answered; BootedAt is the latest boot.
	CreatedAt time.Time `json:"createdAt"`
	ReadyAt   time.Time `json:"readyAt,omitempty"`
	BootedAt  time.Time `json:"bootedAt,omitempty"`

	CertNotAfter time.Time  `json:"certNotAfter"`
	CloudImage   ImageState `json:"cloudImage"`
	Pins         Pins       `json:"pins"`
}

// ImageState records the verified cloud image the guest's disk is
// backed by.
type ImageState struct {
	SHA256   string    `json:"sha256"`
	URL      string    `json:"url"`
	SignedBy string    `json:"signedBy"`
	SignedAt time.Time `json:"signedAt"`
}

// Pins are the guest definition's versions at creation.
type Pins struct {
	Docker     string `json:"docker"`
	Containerd string `json:"containerd"`
	BuildKit   string `json:"buildkit"`
}

func currentPins() Pins {
	return Pins{Docker: DockerVersion, Containerd: ContainerdVersion, BuildKit: BuildKitVersion}
}

// errNoState is loadState's answer when no guest was provisioned.
var errNoState = errors.New("no dockerhost state")

func loadState(p paths) (State, error) {
	data, err := os.ReadFile(p.state())
	if os.IsNotExist(err) {
		return State{}, errNoState
	}
	if err != nil {
		return State{}, err
	}
	var s State
	if err := json.Unmarshal(data, &s); err != nil {
		return State{}, fmt.Errorf("parse %s: %w", p.state(), err)
	}
	if s.Version != stateVersion {
		return State{}, fmt.Errorf("%s: unsupported state version %d (want %d); `y-cluster dockerhost teardown` and provision again", p.state(), s.Version, stateVersion)
	}
	return s, nil
}

func saveState(p paths, s State) error {
	s.Version = stateVersion
	data, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(p.dir(), "state.json.*.tmp")
	if err != nil {
		return err
	}
	if _, err := tmp.Write(append(data, '\n')); err != nil {
		_ = tmp.Close()
		_ = os.Remove(tmp.Name())
		return err
	}
	if err := tmp.Close(); err != nil {
		_ = os.Remove(tmp.Name())
		return err
	}
	if err := os.Rename(tmp.Name(), p.state()); err != nil {
		_ = os.Remove(tmp.Name())
		return err
	}
	return nil
}

// DockerHostURL and BuildkitHostURL are the client URLs of the
// contract, DOCKER_HOST and BUILDKIT_HOST.
func (s State) DockerHostURL() string   { return "tcp://" + hostPort(s.Address, s.DockerPort) }
func (s State) BuildkitHostURL() string { return "tcp://" + hostPort(s.Address, s.BuildkitPort) }

func hostPort(host, port string) string { return host + ":" + port }

// lockTimeout bounds the wait for another provision or teardown,
// which may be in the middle of a first boot.
var lockTimeout = 20 * time.Minute

// withLock runs fn holding the state directory's lock. Provision and
// teardown take it; env and status only read files that are replaced
// atomically. flock(2): released when the process dies, so a killed
// provision leaves no stale lock.
func withLock(p paths, fn func() error) error {
	if err := p.ensure(); err != nil {
		return err
	}
	fh, err := os.OpenFile(p.lock(), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return err
	}
	defer func() { _ = fh.Close() }() // releases the lock
	deadline := time.Now().Add(lockTimeout)
	for {
		err := syscall.Flock(int(fh.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
		if err == nil {
			break
		}
		if !errors.Is(err, syscall.EWOULDBLOCK) {
			return fmt.Errorf("lock %s: %w", p.lock(), err)
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("another y-cluster dockerhost provision or teardown held %s for more than %s", p.lock(), lockTimeout)
		}
		time.Sleep(250 * time.Millisecond)
	}
	return fn()
}
