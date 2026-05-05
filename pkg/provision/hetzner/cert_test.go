package hetzner

import (
	"crypto/x509"
	"encoding/pem"
	"net"
	"strings"
	"testing"
	"time"
)

// TestGenerateSelfSignedCert_Shape pins what the produced cert
// must contain so downstream Hetzner Certificate.Create accepts
// it and TLS clients verify it correctly:
//
//   - Two PEM blocks with the right types
//   - Cert signed by its own private key (self-signature verifies)
//   - SANs match the input (CN alone is ignored by modern clients)
//   - Validity window covers "now" plus our ~1 year
//   - ExtKeyUsage includes ServerAuth (required by browsers + go's
//     TLS verifier)
func TestGenerateSelfSignedCert_Shape(t *testing.T) {
	dnsNames := []string{"y-c-test.local.test", "*.y-c-test.local.test"}
	ipSANs := []net.IP{net.ParseIP("203.0.113.1")}
	certPEM, keyPEM, err := generateSelfSignedCert("y-c-test.local.test", dnsNames, ipSANs)
	if err != nil {
		t.Fatal(err)
	}

	certBlock, rest := pem.Decode(certPEM)
	if certBlock == nil || certBlock.Type != "CERTIFICATE" {
		t.Fatalf("cert PEM block: got %+v", certBlock)
	}
	if len(rest) != 0 {
		t.Errorf("trailing data after cert PEM: %d bytes", len(rest))
	}
	cert, err := x509.ParseCertificate(certBlock.Bytes)
	if err != nil {
		t.Fatalf("parse cert: %v", err)
	}

	keyBlock, rest := pem.Decode(keyPEM)
	if keyBlock == nil || keyBlock.Type != "RSA PRIVATE KEY" {
		t.Fatalf("key PEM block: got %+v", keyBlock)
	}
	if len(rest) != 0 {
		t.Errorf("trailing data after key PEM: %d bytes", len(rest))
	}

	// Self-signature: the cert's signature verifies against its
	// own public key. CheckSignatureFrom would also enforce IsCA
	// on the issuer, which is wrong for a leaf-only TLS server
	// cert; CheckSignature only validates the bytes, which is the
	// property we actually want here.
	if err := cert.CheckSignature(cert.SignatureAlgorithm, cert.RawTBSCertificate, cert.Signature); err != nil {
		t.Errorf("self-signature: %v", err)
	}

	// SANs round-trip.
	if got, want := cert.DNSNames, dnsNames; !equalStringSlice(got, want) {
		t.Errorf("DNSNames: got %v, want %v", got, want)
	}
	if len(cert.IPAddresses) != 1 || !cert.IPAddresses[0].Equal(ipSANs[0]) {
		t.Errorf("IPAddresses: got %v, want %v", cert.IPAddresses, ipSANs)
	}

	// Validity covers "now"; not-before grace is in the past, not-
	// after is roughly now+certValidity.
	now := time.Now()
	if !cert.NotBefore.Before(now) {
		t.Errorf("NotBefore should be in the past: %v vs now=%v", cert.NotBefore, now)
	}
	if !cert.NotAfter.After(now.Add(certValidity - 24*time.Hour)) {
		t.Errorf("NotAfter %v should be ~%v in the future", cert.NotAfter, certValidity)
	}

	// Server-auth EKU.
	hasServerAuth := false
	for _, ku := range cert.ExtKeyUsage {
		if ku == x509.ExtKeyUsageServerAuth {
			hasServerAuth = true
			break
		}
	}
	if !hasServerAuth {
		t.Errorf("ExtKeyUsage missing ServerAuth: %v", cert.ExtKeyUsage)
	}

	// Common name pass-through.
	if cert.Subject.CommonName != "y-c-test.local.test" {
		t.Errorf("CommonName: got %q", cert.Subject.CommonName)
	}
}

// TestGenerateSelfSignedCert_DistinctSerial guards against the
// "same serial every time" footgun. Each call to crypto/rand
// should produce a fresh random serial, so two consecutive calls
// must differ. (A constant serial would still validate, but if
// Hetzner ever indexes by serial or a TLS client builds a serial-
// blocklist, dev clusters would collide.)
func TestGenerateSelfSignedCert_DistinctSerial(t *testing.T) {
	cert1, _, err := generateSelfSignedCert("a.local.test", []string{"a.local.test"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	cert2, _, err := generateSelfSignedCert("a.local.test", []string{"a.local.test"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	c1, _ := x509.ParseCertificate(decodePEM(t, cert1, "CERTIFICATE"))
	c2, _ := x509.ParseCertificate(decodePEM(t, cert2, "CERTIFICATE"))
	if c1.SerialNumber.Cmp(c2.SerialNumber) == 0 {
		t.Errorf("two consecutive certs share the same serial: %v", c1.SerialNumber)
	}
}

// TestCertSubjectsForContext pins the SAN composition rules:
// leaf FQDN, wildcard, and IP -- so a future tweak doesn't drop
// one of the three on the floor.
func TestCertSubjectsForContext(t *testing.T) {
	cn, dns, ips := certSubjectsForContext("y-c-test", "local.test", "203.0.113.1")
	if cn != "y-c-test.local.test" {
		t.Errorf("CN: got %q", cn)
	}
	want := []string{"y-c-test.local.test", "*.y-c-test.local.test"}
	if !equalStringSlice(dns, want) {
		t.Errorf("dns: got %v, want %v", dns, want)
	}
	if len(ips) != 1 || ips[0].String() != "203.0.113.1" {
		t.Errorf("ips: got %v", ips)
	}
}

// TestCertSubjectsForContext_DefaultDomain: empty fqdnDomain
// falls back to local.test, the RFC 6761 reserved test TLD the
// rest of the provisioner uses.
func TestCertSubjectsForContext_DefaultDomain(t *testing.T) {
	cn, _, _ := certSubjectsForContext("ctx", "", "")
	if !strings.HasSuffix(cn, ".local.test") {
		t.Errorf("default domain should be local.test: got %q", cn)
	}
}

// TestCertSubjectsForContext_NoLbIP: when the LB IPv4 is empty
// or unparseable, ipSANs is empty rather than [<nil>] (which
// would crash crypto/x509).
func TestCertSubjectsForContext_NoLbIP(t *testing.T) {
	if _, _, ips := certSubjectsForContext("ctx", "local.test", ""); len(ips) != 0 {
		t.Errorf("empty lbIPv4 should produce no IP SANs, got %v", ips)
	}
	if _, _, ips := certSubjectsForContext("ctx", "local.test", "not-an-ip"); len(ips) != 0 {
		t.Errorf("garbage lbIPv4 should produce no IP SANs, got %v", ips)
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

// decodePEM extracts the bytes of a single PEM block of expected
// type. Test helper so the cert-parse boilerplate doesn't
// duplicate.
func decodePEM(t *testing.T, data []byte, wantType string) []byte {
	t.Helper()
	block, _ := pem.Decode(data)
	if block == nil || block.Type != wantType {
		t.Fatalf("expected %q PEM block, got %+v", wantType, block)
	}
	return block.Bytes
}
