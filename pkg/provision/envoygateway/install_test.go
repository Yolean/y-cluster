package envoygateway

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Yolean/y-cluster/pkg/provision/upgrade"
)

// fakeKubectl puts a kubectl on PATH that answers the controller
// image query with liveImage and logs its arguments, one call per
// line.
func fakeKubectl(t *testing.T, liveImage string) (log string) {
	t.Helper()
	dir := t.TempDir()
	log = filepath.Join(dir, "calls")
	script := `#!/bin/sh
echo "$@" >> ` + log + `
case "$*" in
*" get deployment "*) printf '%s' '` + liveImage + `' ;;
*" apply "*) cat > /dev/null ;;
esac
exit 0
`
	if err := os.WriteFile(filepath.Join(dir, "kubectl"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	return log
}

// cachedRelease writes a release's install.yaml into a cache dir, as
// Ensure would have, and returns options that select it.
func cachedRelease(t *testing.T, version string) Options {
	t.Helper()
	cacheDir := t.TempDir()
	dir := filepath.Join(cacheDir, "envoygateway", version)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	body := strings.ReplaceAll(miniInstall, "v9.9.9", version)
	if err := os.WriteFile(filepath.Join(dir, "install.yaml"), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	return Options{
		ContextName:   "ctx",
		Version:       version,
		InstallSHA256: sha256Hex(body),
		CacheOverride: cacheDir,
		ReadyTimeout:  -1,
	}
}

func TestRender_Moves(t *testing.T) {
	for _, c := range []struct {
		live       string
		allowMinor bool
		want       upgrade.Step
		wantJob    bool
	}{
		{live: "", want: upgrade.Install, wantJob: true},
		{live: "envoyproxy/gateway:v1.9.2", want: upgrade.Same},
		{live: "envoyproxy/gateway:v1.9.1", want: upgrade.Patch, wantJob: true},
		{live: "docker.io/envoyproxy/gateway:v1.9.0", want: upgrade.Patch, wantJob: true},
		{live: "envoyproxy/gateway:v1.8.5", allowMinor: true, want: upgrade.Minor, wantJob: true},
	} {
		fakeKubectl(t, c.live)
		opts := cachedRelease(t, "v1.9.2")
		opts.AllowMinor = c.allowMinor
		r, err := Render(context.Background(), opts)
		if err != nil {
			t.Errorf("live %q: %v", c.live, err)
			continue
		}
		if r.Step != c.want || r.Version != "v1.9.2" {
			t.Errorf("live %q: step %v version %s, want %v", c.live, r.Step, r.Version, c.want)
		}
		if got := strings.Contains(strings.Join(renderedKinds(t, r.Install), ","), "Job"); got != c.wantJob {
			t.Errorf("live %q: the hook Job rendered %v, want %v", c.live, got, c.wantJob)
		}
	}
}

// A refused move names both releases, renders nothing and, through
// Install, applies nothing.
func TestInstall_RefusedMoveAppliesNothing(t *testing.T) {
	for _, c := range []struct {
		live, rule string
		minor      bool
	}{
		{live: "envoyproxy/gateway:v1.9.3", rule: "a downgrade is refused"},
		{live: "envoyproxy/gateway:v1.10.0", rule: "a downgrade is refused"},
		{live: "envoyproxy/gateway:v1.8.5", rule: "a minor upgrade is refused", minor: true},
		{live: "envoyproxy/gateway:v2.0.0", rule: "a change of major version is refused"},
		{live: "envoyproxy/gateway:v1.9.2-rc.1", rule: "is not a release tag"},
	} {
		log := fakeKubectl(t, c.live)
		opts := cachedRelease(t, "v1.9.2")
		err := Install(context.Background(), opts)
		var refused *upgrade.RefusedError
		if !errors.As(err, &refused) || refused.MinorNotAllowed != c.minor {
			t.Errorf("live %q: %v, want a refusal (minor %v)", c.live, err, c.minor)
			continue
		}
		installed, _ := ReleaseOf(c.live)
		for _, s := range []string{c.rule, installed, "v1.9.2", "nothing was changed"} {
			if !strings.Contains(err.Error(), s) {
				t.Errorf("live %q: %q does not say %q", c.live, err, s)
			}
		}
		calls, _ := os.ReadFile(log)
		if strings.Contains(string(calls), " apply ") {
			t.Errorf("live %q: a refused move applied:\n%s", c.live, calls)
		}
	}
}

// The configured version is what is applied, the proxy image goes on
// the EnvoyProxy, and a release whose cached file does not match its
// digest is refused before anything is applied.
func TestInstall_ConfiguredRelease(t *testing.T) {
	log := fakeKubectl(t, "envoyproxy/gateway:v1.9.1")
	opts := cachedRelease(t, "v1.9.2")
	opts.ProxyImage = "docker.io/envoyproxy/envoy:distroless-v1.39.5@sha256:0000000000000000000000000000000000000000000000000000000000000000"
	opts.GatewayClassName = "y-cluster"
	r, err := Render(context.Background(), opts)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(r.Install), "envoyproxy/gateway:v1.9.2") {
		t.Errorf("install manifest is not v1.9.2's:\n%s", r.Install)
	}
	if !strings.Contains(string(r.EnvoyProxy), "image: "+opts.ProxyImage) {
		t.Errorf("EnvoyProxy without the proxy image:\n%s", r.EnvoyProxy)
	}
	if !strings.Contains(string(r.GatewayClass), "name: "+EnvoyProxyName) {
		t.Errorf("GatewayClass without parametersRef to the EnvoyProxy:\n%s", r.GatewayClass)
	}
	if err := Install(context.Background(), opts); err != nil {
		t.Fatal(err)
	}
	calls, _ := os.ReadFile(log)
	if n := strings.Count(string(calls), " apply "); n != 3 {
		t.Errorf("%d applies, want install.yaml, EnvoyProxy and GatewayClass:\n%s", n, calls)
	}

	log = fakeKubectl(t, "envoyproxy/gateway:v1.9.1")
	opts.InstallSHA256 = sha256Hex("another release")
	if err := Install(context.Background(), opts); err == nil || !strings.Contains(err.Error(), "remove the file") {
		t.Errorf("a cached file that fails its digest: %v", err)
	}
	if calls, _ := os.ReadFile(log); strings.Contains(string(calls), " apply ") {
		t.Errorf("a cached file that fails its digest was applied:\n%s", calls)
	}
}

func TestReleaseOf(t *testing.T) {
	for image, want := range map[string]string{
		"envoyproxy/gateway:v1.9.1":                          "v1.9.1",
		"docker.io/envoyproxy/gateway:v1.9.1":                "v1.9.1",
		"registry.example:5000/envoyproxy/gateway:v1.9.2":    "v1.9.2",
		"docker.io/envoyproxy/gateway:v1.9.1@sha256:abc0123": "v1.9.1",
		"envoyproxy/gateway:v1.9.0-rc.1":                     "v1.9.0-rc.1",
	} {
		if got, err := ReleaseOf(image); err != nil || got != want {
			t.Errorf("ReleaseOf(%q) = %q, %v; want %q", image, got, err, want)
		}
	}
	for _, image := range []string{
		"envoyproxy/gateway",
		"registry.example:5000/envoyproxy/gateway",
		"envoyproxy/gateway@sha256:abc0123",
		"example/gateway:v1.9.1",
		"notenvoyproxy/gateway:v1.9.1",
	} {
		if got, err := ReleaseOf(image); err == nil {
			t.Errorf("ReleaseOf(%q) = %q, want an error", image, got)
		}
	}
}
