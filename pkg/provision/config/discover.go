package config

import (
	"context"
	"os"
	"os/exec"
	"runtime"
	"time"
)

// DiscoverProvider picks a default provider name from
// AllProviders by inspecting the host. Returns "" when nothing
// matches; the caller then errors out asking the user to set
// `provider:` explicitly. This is the runtime probe used when
// `y-cluster-provision.yaml` omits `provider:`.
//
// Heuristic, top to bottom:
//
//	1. Linux + /dev/kvm + qemu-system-x86_64    -> qemu
//	2. docker CLI present + `docker info` OK    -> docker
//
// qemu wins over docker on Linux because it has the full
// disk-and-appliance feature surface (cloud-init seed,
// persistent disk, snapshots) that the docker provisioner
// doesn't implement. If the host can run a VM, that's the
// default; if it can only run docker, that's the default.
//
// 100 % test coverage is impractical because the inputs are
// kernel-level (/dev/kvm) and external commands (`docker
// info`). The function is deliberately a flat sequence of
// probes you can read top to bottom; each probe lives in its
// own helper. Tests that need to control the outcome (e.g.
// "what does LoadProvision do when discovery returns empty?")
// override the package-level DiscoverProviderFn.
func DiscoverProvider() string { return DiscoverProviderFn() }

// DiscoverProviderFn is the implementation called by
// DiscoverProvider. Tests reassign it to control discovery
// without surgery on the kernel or the docker daemon. Restore
// to defaultDiscoverProvider when done.
var DiscoverProviderFn = defaultDiscoverProvider

func defaultDiscoverProvider() string {
	if runtime.GOOS == "linux" && hasKVM() && hasBinary("qemu-system-x86_64") {
		return ProviderQEMU
	}
	if dockerReachable() {
		return ProviderDocker
	}
	return ""
}

// hasKVM reports whether /dev/kvm exists. KVM is what the qemu
// provisioner relies on for accelerated VMs; the cluster takes
// many minutes to bring up under tcg emulation, which is below
// our usable threshold.
func hasKVM() bool {
	_, err := os.Stat("/dev/kvm")
	return err == nil
}

// hasBinary reports whether `name` resolves on $PATH.
func hasBinary(name string) bool {
	_, err := exec.LookPath(name)
	return err == nil
}

// dockerReachable reports whether `docker info` returns
// successfully within 2 s. The CLI presence check is cheaper
// than the daemon round-trip, so we short-circuit if the binary
// isn't even installed.
func dockerReachable() bool {
	if !hasBinary("docker") {
		return false
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	return exec.CommandContext(ctx, "docker", "info").Run() == nil
}
