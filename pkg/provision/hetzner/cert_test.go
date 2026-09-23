package hetzner

import (
	"crypto/x509"
	"encoding/pem"
	"github.com/Yolean/y-cluster/pkg/provision/devcert"
	"strings"
	"testing"
)

// TestCertSubjectsForContext pins the SAN composition rules:
// leaf FQDN + wildcard, no IP SAN.
func TestCertSubjectsForContext(t *testing.T) {
	cn, dns := certSubjectsForContext("y-c-test", "alice", "local.test")
	if cn != "y-c-test.alice.local.test" {
		t.Errorf("CN: got %q", cn)
	}
	want := []string{"y-c-test.alice.local.test", "*.y-c-test.alice.local.test"}
	if !equalStringSlice(dns, want) {
		t.Errorf("dns: got %v, want %v", dns, want)
	}
}

// The Gateway listener and the certificate are built by two functions
// from the same three inputs. They used to disagree (the cert left
// the lb group out), so the cert could never validate for a hostname
// the Gateway serves; e2e did not notice behind `curl -k`. This test
// goes through a real certificate rather than comparing strings.
func TestCert_ValidForHostnamesTheGatewayServes(t *testing.T) {
	const ctxName, lbGroup, domain = "alice-dev", "alice", "local.test"
	cn, dns := certSubjectsForContext(ctxName, lbGroup, domain)
	certPEM, _, err := devcert.GenerateSelfSigned(cn, dns, nil)
	if err != nil {
		t.Fatal(err)
	}
	block, _ := pem.Decode(certPEM)
	if block == nil {
		t.Fatal("no PEM block in the generated certificate")
	}
	cert, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		t.Fatal(err)
	}

	pattern := defaultGatewayHostnamePattern(ctxName, lbGroup, domain)
	if !strings.HasPrefix(pattern, "*.") {
		t.Fatalf("expected a wildcard listener hostname, got %q", pattern)
	}
	served := "hello" + strings.TrimPrefix(pattern, "*")
	if err := cert.VerifyHostname(served); err != nil {
		t.Errorf("the Gateway serves %s but the certificate does not cover it: %v", served, err)
	}
	if err := cert.VerifyHostname(strings.TrimPrefix(pattern, "*.")); err != nil {
		t.Errorf("the leaf name is not covered: %v", err)
	}
	if err := cert.VerifyHostname("hello." + ctxName + "." + domain); err == nil {
		t.Error("the certificate still covers the name space without the lb group")
	}
}

func TestCertSubjectsForContext_DefaultDomain(t *testing.T) {
	cn, _ := certSubjectsForContext("ctx", "grp", "")
	if !strings.HasSuffix(cn, ".local.test") {
		t.Errorf("default domain should be local.test: got %q", cn)
	}
}

// equalStringSlice tests two []string for value equality without
// pulling reflect.DeepEqual (cleaner test failures on string
// content).
func equalStringSlice(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
