package hetzner

import (
	"strings"
	"testing"
)

// TestRejectUpstreamHostsToml pins the hosts.toml content. Three
// load-bearing lines:
//
//  1. `server = "..."`  -- when present, containerd does NOT fall
//     back to the registry's real hostname.
//  2. `[host."..."]`    -- declares the host; both quoted-URL
//     entries must agree on the same dead URL.
//  3. `capabilities = ["pull", "resolve"]` -- the host advertises
//     itself as usable for pulls; when DNS fails, the pull errors
//     out instead of silently being marked unusable.
func TestRejectUpstreamHostsToml(t *testing.T) {
	for _, want := range []string{
		`server = "http://reject-upstream-by-y-cluster.invalid:9999"`,
		`[host."http://reject-upstream-by-y-cluster.invalid:9999"]`,
		`capabilities = ["pull", "resolve"]`,
		"y-cluster",
		"rejectUpstream",
	} {
		if !strings.Contains(rejectUpstreamHostsToml, want) {
			t.Errorf("hosts.toml missing %q:\n%s", want, rejectUpstreamHostsToml)
		}
	}
}

// TestRejectUpstreamRegistries: the explicit list must include
// _default (the catch-all containerd consults when no registry-
// specific dir matches) plus the major upstream registries Yolean
// workloads typically reference.
func TestRejectUpstreamRegistries(t *testing.T) {
	got := map[string]bool{}
	for _, r := range rejectUpstreamRegistries {
		got[r] = true
	}
	for _, want := range []string{
		"_default",
		"docker.io",
		"registry.k8s.io",
		"quay.io",
		"ghcr.io",
	} {
		if !got[want] {
			t.Errorf("rejectUpstreamRegistries missing %q", want)
		}
	}
}

// TestRejectUpstreamScript: with a lifetime reaper installed the
// script must
//
//   - wait for the reaper Pod to reach Running before dropping
//     hosts.toml (otherwise the lockdown can land mid-pull of
//     hetznercloud/cli and leave the reaper Pod ImagePullBackOff,
//     killing the lifetime expiry safety net);
//   - drop both registries.yaml (durability across k3s restarts)
//     AND a hosts.toml per registry (the immediate-effect
//     lockdown);
//   - NOT restart k3s (containerd re-reads hosts.toml per pull,
//     so a restart would be wasted work plus a brief API outage).
func TestRejectUpstreamScript(t *testing.T) {
	got := rejectUpstreamScript(true)

	for _, want := range []string{
		"set -euo pipefail",
		// Reaper-readiness gate.
		"-l job-name=reaper",
		"phase=$(sudo k3s kubectl",
		`[ "$phase" = "Running" ]`,
		// File drops.
		"sudo tee /etc/rancher/k3s/registries.yaml",
		"sudo tee /var/lib/rancher/k3s/agent/etc/containerd/certs.d/_default/hosts.toml",
		"sudo tee /var/lib/rancher/k3s/agent/etc/containerd/certs.d/docker.io/hosts.toml",
		"sudo tee /var/lib/rancher/k3s/agent/etc/containerd/certs.d/registry.k8s.io/hosts.toml",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("script missing %q", want)
		}
	}

	// Reaper gate must precede the file drops.
	gate := strings.Index(got, "-l job-name=reaper")
	hosts := strings.Index(got, "certs.d/_default/hosts.toml")
	if gate < 0 || hosts < 0 || gate >= hosts {
		t.Errorf("reaper gate must precede hosts.toml drop; gate=%d hosts=%d", gate, hosts)
	}

	// Negative: no k3s restart in the script. A restart-as-part-
	// of-applyRejectUpstream is unnecessary (hosts.toml is per-
	// pull, not load-time) and would briefly take the API down.
	if strings.Contains(got, "systemctl restart k3s") {
		t.Errorf("script should not restart k3s; hosts.toml is read per-pull:\n%s", got)
	}
}

// TestRejectUpstreamScript_NoLifetime: a cluster without a
// lifetime has no reaper Job, so the readiness gate would spin
// its full 120s and then abort the lockdown. The gate must be
// absent while the file drops stay intact.
func TestRejectUpstreamScript_NoLifetime(t *testing.T) {
	got := rejectUpstreamScript(false)

	if strings.Contains(got, "job-name=reaper") {
		t.Errorf("script waits for a reaper that was never installed:\n%s", got)
	}
	for _, want := range []string{
		"sudo tee /etc/rancher/k3s/registries.yaml",
		"sudo tee /var/lib/rancher/k3s/agent/etc/containerd/certs.d/_default/hosts.toml",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("script missing %q", want)
		}
	}
}
