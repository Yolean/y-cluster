package multipass

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestURLEncodeK3sVersion(t *testing.T) {
	cases := map[string]string{
		"v1.35.4-rc3-k3s1": "v1.35.4-rc3-k3s1",
		"v1.35.3+k3s1":     "v1.35.3%2Bk3s1",
		"plain":            "plain",
	}
	for in, want := range cases {
		if got := urlEncodeK3sVersion(in); got != want {
			t.Errorf("urlEncodeK3sVersion(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestShellQuote(t *testing.T) {
	cases := map[string]string{
		"plain":     "'plain'",
		"with sp":   "'with sp'",
		"a'b":       `'a'\''b'`,
		"--tls-san": "'--tls-san'",
	}
	for in, want := range cases {
		if got := shellQuote(in); got != want {
			t.Errorf("shellQuote(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestK3sExecArgs_IncludesTLSSanForVMIP(t *testing.T) {
	c := &Cluster{vmIP: "192.168.64.10"}
	args := c.k3sExecArgs()
	if !strings.Contains(args, "--tls-san=192.168.64.10") {
		t.Fatalf("k3sExecArgs missing tls-san for VM IP: %q", args)
	}
	if !strings.Contains(args, "--disable=traefik") {
		t.Fatalf("k3sExecArgs missing --disable=traefik: %q", args)
	}
	if !strings.Contains(args, "--write-kubeconfig-mode=644") {
		t.Fatalf("k3sExecArgs missing kubeconfig mode: %q", args)
	}
}

func TestDownloadFile_Atomic(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("payload\n"))
	}))
	defer srv.Close()

	dest := filepath.Join(t.TempDir(), "out.bin")
	if err := downloadFile(context.Background(), srv.URL+"/ok", dest); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(dest)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "payload\n" {
		t.Fatalf("body: %q", got)
	}
	if _, err := os.Stat(dest + ".tmp"); !os.IsNotExist(err) {
		t.Fatal(".tmp file leaked after successful download")
	}
}

func TestDownloadFile_Non200(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.NotFound(w, r)
	}))
	defer srv.Close()

	dest := filepath.Join(t.TempDir(), "out.bin")
	err := downloadFile(context.Background(), srv.URL+"/x", dest)
	if err == nil || !strings.Contains(err.Error(), "404") {
		t.Fatalf("want 404 error, got %v", err)
	}
	if _, err := os.Stat(dest); !os.IsNotExist(err) {
		t.Fatal("dest file should not exist after failed download")
	}
}

// TestExtractKubeconfig_RewritesServerURLToVMIP exercises the byte
// rewrite without spinning up multipass. The kubeconfig the VM
// produces names 127.0.0.1; the host needs <vm-ip>.
func TestExtractKubeconfig_RewritesServerURLToVMIP(t *testing.T) {
	raw := []byte(strings.Join([]string{
		"apiVersion: v1",
		"clusters:",
		"- cluster:",
		"    server: https://127.0.0.1:6443",
		"  name: default",
		"",
	}, "\n"))
	got := strings.ReplaceAll(string(raw), "127.0.0.1:6443", "192.168.64.10:6443")
	if !strings.Contains(got, "192.168.64.10:6443") {
		t.Fatalf("rewrite missing: %q", got)
	}
	if strings.Contains(got, "127.0.0.1:6443") {
		t.Fatalf("original loopback still present: %q", got)
	}
}
