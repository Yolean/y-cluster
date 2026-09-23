// Package devcert issues the self-signed TLS certificate a dev
// cluster terminates HTTPS with: hetzner hands it to the Hetzner
// load balancer, glesys puts it in a Secret the Gateway's HTTPS
// listener references. Self-signed because the FQDNs are under the
// RFC 6761 test TLD, which no CA can issue for; consumers accept the
// certificate out-of-band.
package devcert

import (
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"fmt"
	"math/big"
	"net"
	"time"
)

// certValidity is how long the self-signed cert is valid. One year
// is generous for the dev-cluster shape (typical cluster lifetime
// is hours to days); the trade is that an operator who keeps a
// cluster running >1 year sees TLS warnings until they reprovision.
// Nothing rotates the certificate.
const certValidity = 365 * 24 * time.Hour

// certKeySize is the RSA key length. 2048 is the sweet spot: still
// universally trusted, fast to generate (~50ms), small handshake.
// 4096 would double the handshake work for no perceptible security
// win on a 1-year-validity dev cert.
const certKeySize = 2048

// certClockSkewGrace lets a freshly generated cert work even if
// the operator's host clock is a minute behind the validating
// client. Value chosen by intuition; a Hetzner LB validating
// behind a strict clock would still accept.
const certClockSkewGrace = 1 * time.Hour

// GenerateSelfSigned returns PEM-encoded cert + private key
// for the given common name, with the supplied SAN list (DNS +
// IP). The cert is self-signed (CA == leaf): a trust chain isn't
// useful for a one-off dev TLS that consumers explicitly accept
// out-of-band, and the simpler shape means fewer moving parts.
//
// commonName is what shows up as the cert's CN; conventionally we
// pass the leaf FQDN. dnsNames carries the full SAN list (CN
// alone isn't honoured by modern TLS clients, so the FQDNs MUST
// also live in dnsNames). ipSANs covers direct-IP dials -- a
// consumer that bypasses DNS hint by hitting the LB IP gets the
// matching cert too.
//
// Returns ASCII-armored PEM blocks, which is what both Hetzner's
// Certificate.Create and a kubernetes.io/tls Secret take.
func GenerateSelfSigned(commonName string, dnsNames []string, ipSANs []net.IP) (certPEM, keyPEM []byte, err error) {
	key, err := rsa.GenerateKey(rand.Reader, certKeySize)
	if err != nil {
		return nil, nil, fmt.Errorf("rsa keygen: %w", err)
	}
	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	if err != nil {
		return nil, nil, fmt.Errorf("serial number: %w", err)
	}
	now := time.Now()
	template := x509.Certificate{
		SerialNumber: serial,
		Subject: pkix.Name{
			CommonName:   commonName,
			Organization: []string{"y-cluster"},
		},
		NotBefore: now.Add(-certClockSkewGrace),
		NotAfter:  now.Add(certValidity),
		KeyUsage: x509.KeyUsageDigitalSignature |
			x509.KeyUsageKeyEncipherment,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		DNSNames:              dnsNames,
		IPAddresses:           ipSANs,
		BasicConstraintsValid: true,
	}
	derBytes, err := x509.CreateCertificate(rand.Reader, &template, &template, &key.PublicKey, key)
	if err != nil {
		return nil, nil, fmt.Errorf("x509 create: %w", err)
	}
	certPEM = pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: derBytes})
	keyPEM = pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)})
	return certPEM, keyPEM, nil
}
