package dockerhost

import (
	"bytes"
	"crypto/tls"
	"crypto/x509"
	"encoding/pem"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func parseCert(t *testing.T, p []byte) *x509.Certificate {
	t.Helper()
	b, _ := pem.Decode(p)
	if b == nil || b.Type != "CERTIFICATE" {
		t.Fatalf("not a certificate: %q", p)
	}
	c, err := x509.ParseCertificate(b.Bytes)
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func TestIssueTLS_Chain(t *testing.T) {
	now := time.Date(2026, 10, 5, 12, 0, 0, 0, time.UTC)
	m, err := issueTLS(net.ParseIP("10.88.1.2"), now)
	if err != nil {
		t.Fatal(err)
	}
	ca := parseCert(t, m.CACert)
	if !ca.IsCA || !ca.MaxPathLenZero {
		t.Fatal("the CA must be a CA that signs leaves only")
	}
	roots := x509.NewCertPool()
	roots.AddCert(ca)
	at := now.Add(24 * time.Hour)

	server := parseCert(t, m.ServerCert)
	if _, err := server.Verify(x509.VerifyOptions{Roots: roots, DNSName: "10.88.1.2", CurrentTime: at, KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}}); err != nil {
		t.Fatalf("server certificate for the guest address: %v", err)
	}
	if _, err := server.Verify(x509.VerifyOptions{Roots: roots, DNSName: "10.88.1.3", CurrentTime: at}); err == nil {
		t.Fatal("the server certificate must name the guest's address only")
	}
	client := parseCert(t, m.ClientCert)
	if _, err := client.Verify(x509.VerifyOptions{Roots: roots, CurrentTime: at, KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth}}); err != nil {
		t.Fatalf("client certificate: %v", err)
	}
	if _, err := client.Verify(x509.VerifyOptions{Roots: roots, CurrentTime: at, KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}}); err == nil {
		t.Fatal("the client certificate must not serve")
	}
	if m.NotAfter.Sub(now) != certValidity || !server.NotAfter.Equal(m.NotAfter) {
		t.Errorf("validity: %s .. %s", server.NotBefore, server.NotAfter)
	}
	if bytes.Equal(m.ServerKey, m.ClientKey) {
		t.Fatal("server and client share a key")
	}
}

// The CA's key is never part of what a provision has: no field holds
// it, and the only private keys are the server's and the client's.
func TestIssueTLS_NoCAKey(t *testing.T) {
	m, err := issueTLS(net.ParseIP("127.0.0.1"), time.Now())
	if err != nil {
		t.Fatal(err)
	}
	keys := 0
	for _, p := range [][]byte{m.CACert, m.ServerCert, m.ServerKey, m.ClientCert, m.ClientKey} {
		for rest := p; ; {
			var b *pem.Block
			b, rest = pem.Decode(rest)
			if b == nil {
				break
			}
			if b.Type != "CERTIFICATE" {
				keys++
			}
		}
	}
	if keys != 2 {
		t.Fatalf("want exactly the server's and the client's key, found %d private keys", keys)
	}
}

func TestWriteClientDir_Permissions(t *testing.T) {
	m, err := issueTLS(net.ParseIP("127.0.0.1"), time.Now())
	if err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(t.TempDir(), "state", "client")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "stale.pem"), []byte("old"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := writeClientDir(dir, m); err != nil {
		t.Fatal(err)
	}
	fi, err := os.Stat(dir)
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode().Perm() != 0o700 {
		t.Errorf("client dir mode %o, want 700", fi.Mode().Perm())
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, e := range entries {
		names = append(names, e.Name())
		info, _ := e.Info()
		if info.Mode().Perm() != 0o600 {
			t.Errorf("%s mode %o, want 600", e.Name(), info.Mode().Perm())
		}
	}
	if len(names) != 3 || names[0] != "ca.pem" || names[1] != "cert.pem" || names[2] != "key.pem" {
		t.Fatalf("client dir holds %v, want exactly ca.pem cert.pem key.pem", names)
	}
	if leftovers, _ := filepath.Glob(filepath.Join(filepath.Dir(dir), "client.new-*")); len(leftovers) > 0 {
		t.Fatalf("temporary dirs left behind: %v", leftovers)
	}
}

// A server with the guest's material requires a client certificate
// from the same CA, which is what dockerd and buildkitd do with it.
func TestClientTLS_MutualAuth(t *testing.T) {
	m, err := issueTLS(net.ParseIP("127.0.0.1"), time.Now())
	if err != nil {
		t.Fatal(err)
	}
	srv := mtlsServer(t, m, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, "OK")
	}), false)
	addr := srv.Listener.Addr().String()

	dir := filepath.Join(t.TempDir(), "client")
	if err := writeClientDir(dir, m); err != nil {
		t.Fatal(err)
	}
	cfg, err := clientTLS(dir, "127.0.0.1")
	if err != nil {
		t.Fatal(err)
	}
	if server, err := pingDocker(t.Context(), cfg, addr); err != nil {
		t.Fatalf("with the client certificate: %v", err)
	} else if server != "" {
		t.Logf("server header %q", server)
	}

	noCert := cfg.Clone()
	noCert.Certificates = nil
	if _, err := pingDocker(t.Context(), noCert, addr); err == nil {
		t.Fatal("a client without a certificate must be refused")
	}

	other, err := issueTLS(net.ParseIP("127.0.0.1"), time.Now())
	if err != nil {
		t.Fatal(err)
	}
	otherDir := filepath.Join(t.TempDir(), "other")
	if err := writeClientDir(otherDir, other); err != nil {
		t.Fatal(err)
	}
	foreign, err := clientTLS(otherDir, "127.0.0.1")
	if err != nil {
		t.Fatal(err)
	}
	foreign.RootCAs = cfg.RootCAs // trusts the server, presents another CA's client cert
	if _, err := pingDocker(t.Context(), foreign, addr); err == nil {
		t.Fatal("a client certificate from another CA must be refused")
	}
}

// mtlsServer serves handler with the guest's server certificate and
// client verification against its CA.
func mtlsServer(t *testing.T, m tlsMaterial, handler http.Handler, h2 bool) *httptest.Server {
	t.Helper()
	cert, err := tls.X509KeyPair(m.ServerCert, m.ServerKey)
	if err != nil {
		t.Fatal(err)
	}
	pool := x509.NewCertPool()
	pool.AppendCertsFromPEM(m.CACert)
	srv := httptest.NewUnstartedServer(handler)
	srv.EnableHTTP2 = h2
	srv.TLS = &tls.Config{Certificates: []tls.Certificate{cert}, ClientAuth: tls.RequireAndVerifyClientCert, ClientCAs: pool}
	srv.StartTLS()
	t.Cleanup(srv.Close)
	return srv
}
