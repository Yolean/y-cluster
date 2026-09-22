package gateway

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// fakeKubectl puts a kubectl on PATH that runs the given /bin/sh body.
func fakeKubectl(t *testing.T, body string) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("PATH stub helper is /bin/sh-only")
	}
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "kubectl"), []byte("#!/bin/sh\n"+body+"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
}

// kubectl prints warnings on stderr while succeeding. They must not
// end up in what callers unmarshal; prepare-export runs through this.
func TestRunKubectl_WarningsDoNotCorruptJSON(t *testing.T) {
	fakeKubectl(t, `echo "Warning: gateway.networking.k8s.io/v1beta1 is deprecated" >&2
echo '{"items":[]}'`)
	out, err := runKubectl(context.Background(), "ctx", "get", "gatewayclass", "-o", "json")
	if err != nil {
		t.Fatal(err)
	}
	var parsed struct{ Items []json.RawMessage }
	if err := json.Unmarshal(out, &parsed); err != nil {
		t.Fatalf("output is not clean JSON: %v\n%s", err, out)
	}
}

// The "no such resource type" classification reads the error text, so
// a failure has to keep carrying kubectl's stderr.
func TestRunKubectl_FailureCarriesStderr(t *testing.T) {
	fakeKubectl(t, `echo "error: the server doesn't have a resource type \"clienttrafficpolicies\"" >&2
exit 1`)
	_, err := runKubectl(context.Background(), "ctx", "get", "clienttrafficpolicies", "-A", "-o", "json")
	if err == nil {
		t.Fatal("expected an error")
	}
	if !isNoResourceType(err) {
		t.Fatalf("stderr lost from the error, classification broke: %v", err)
	}
	if !strings.Contains(err.Error(), "--context") && !strings.Contains(err.Error(), "get clienttrafficpolicies") {
		t.Errorf("error should name the command: %v", err)
	}
}
