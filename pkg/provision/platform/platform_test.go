package platform

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/Yolean/y-cluster/pkg/provision/config"
	"github.com/Yolean/y-cluster/pkg/provision/envoygateway"
)

func defaulted(t *testing.T) Options {
	t.Helper()
	c := &config.QEMUConfig{CommonConfig: config.CommonConfig{Provider: config.ProviderQEMU}}
	c.ApplyDefaults()
	return Options{Context: "ctx", Storage: c.Storage, Gateway: c.Gateway, CertManager: c.CertManager, DNSHintIP: "127.0.0.1"}
}

// The Envoy Gateway options are those qemu provision passed before
// the platform became a function of its own.
func TestOptions_EnvoyGatewayAsProvisionInstalledIt(t *testing.T) {
	o := defaulted(t).envoyGateway()
	o.Logger = nil
	want := envoygateway.Options{
		ContextName:          "ctx",
		GatewayClassName:     "y-cluster",
		DNSHintIP:            "127.0.0.1",
		ControllerCPURequest: "10m",
		ControllerMemRequest: "64Mi",
		ProxyCPURequest:      "10m",
		ProxyMemRequest:      "128Mi",
		MergeGateways:        true,
	}
	if !reflect.DeepEqual(o, want) {
		t.Errorf("envoygateway options\n got %+v\nwant %+v", o, want)
	}
	ls := defaulted(t).localStorage()
	if ls.ContextName != "ctx" || ls.Path != "/data/yolean" || ls.Pattern != "{{ .PVC.Namespace }}_{{ .PVC.Name }}" || ls.ReclaimPolicy != "Retain" || ls.ReadyTimeout != 0 {
		t.Errorf("localstorage options %+v", ls)
	}
	if cm := defaulted(t).certManager(); cm.ContextName != "ctx" || cm.Version != "" || cm.ReadyTimeout != 0 {
		t.Errorf("certmanager options %+v", cm)
	}
}

// fakeKubectl puts a kubectl on PATH whose `diff` exits with code
// when stdin contains marker, 0 otherwise, and logs its arguments.
func fakeKubectl(t *testing.T, marker string, code int) string {
	t.Helper()
	dir := t.TempDir()
	log := filepath.Join(dir, "calls")
	script := `#!/bin/sh
echo "$@" >> ` + log + `
case "$*" in
*" diff "*)
  if grep -q '` + marker + `'; then echo "-old +new"; exit ` + string(rune('0'+code)) + `; fi
  ;;
esac
exit 0
`
	if err := os.WriteFile(filepath.Join(dir, "kubectl"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	return log
}

func TestCheck(t *testing.T) {
	opts := defaulted(t)
	opts.Gateway.Skip = true
	opts.CertManager.Skip = true

	log := fakeKubectl(t, "local-path-provisioner", 1)
	var out bytes.Buffer
	drift, err := Check(context.Background(), opts, &out)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(drift, []string{"local-path-provisioner"}) {
		t.Errorf("drift %v\n%s", drift, out.String())
	}
	if !strings.Contains(out.String(), "# local-path-provisioner: would change\n-old +new") || !strings.Contains(out.String(), ": no change") {
		t.Errorf("report:\n%s", out.String())
	}
	calls, _ := os.ReadFile(log)
	for _, line := range strings.Split(strings.TrimSpace(string(calls)), "\n") {
		if line != "--context=ctx diff --server-side --force-conflicts --field-manager=y-cluster -f -" {
			t.Errorf("Check ran something other than a server-side diff: %s", line)
		}
	}

	fakeKubectl(t, "local-path-provisioner", 2)
	if _, err := Check(context.Background(), opts, &out); err == nil {
		t.Error("a failing kubectl diff is an error, not drift")
	}
	if _, err := Check(context.Background(), Options{}, &out); err == nil {
		t.Error("no context")
	}
}

// liveGateway puts a kubectl on PATH that reports liveImage as the
// running Envoy Gateway controller and logs every call.
func liveGateway(t *testing.T, liveImage string) string {
	t.Helper()
	dir := t.TempDir()
	log := filepath.Join(dir, "calls")
	script := `#!/bin/sh
echo "$@" >> ` + log + `
case "$*" in
*" get deployment envoy-gateway "*) printf '%s' '` + liveImage + `' ;;
esac
exit 0
`
	if err := os.WriteFile(filepath.Join(dir, "kubectl"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	return log
}

// A move the version rule refuses changes nothing: Install stops
// before Cilium, and Check reports a failure, not drift.
func TestRefusedGatewayMoveChangesNothing(t *testing.T) {
	opts := defaulted(t)
	opts.CertManager.Skip = true
	for _, c := range []struct {
		live, rule string
		allowMinor bool
	}{
		{live: "envoyproxy/gateway:v1.9.99", rule: "a downgrade is refused"},
		{live: "envoyproxy/gateway:v1.8.5", rule: "a minor upgrade is refused"},
		{live: "envoyproxy/gateway:v0.9.1", rule: "a change of major version is refused", allowMinor: true},
	} {
		opts.GatewayAllowMinor = c.allowMinor
		log := liveGateway(t, c.live)
		err := Install(context.Background(), opts)
		if err == nil || !strings.Contains(err.Error(), c.rule) || !strings.Contains(err.Error(), envoygateway.Version) {
			t.Errorf("live %s: Install: %v", c.live, err)
		}
		calls, _ := os.ReadFile(log)
		if strings.TrimSpace(string(calls)) != "--context=ctx -n envoy-gateway-system get deployment envoy-gateway --ignore-not-found -o jsonpath={.spec.template.spec.containers[?(@.name==\"envoy-gateway\")].image}" {
			t.Errorf("live %s: Install ran more than the version check:\n%s", c.live, calls)
		}
		var out bytes.Buffer
		drift, err := Check(context.Background(), opts, &out)
		if err == nil || !strings.Contains(err.Error(), c.rule) || len(drift) > 0 {
			t.Errorf("live %s: Check: %v, drift %v", c.live, err, drift)
		}
		if calls, _ := os.ReadFile(log); strings.Contains(string(calls), " diff ") || strings.Contains(string(calls), " apply ") {
			t.Errorf("live %s: a refused move was diffed or applied:\n%s", c.live, calls)
		}
	}
}
