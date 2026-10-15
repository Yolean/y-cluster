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
