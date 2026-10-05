package dockerhost

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"fmt"
	"math/big"
	"net"
	"os"
	"path/filepath"
	"time"
)

// certValidity bounds every certificate of a guest. A guest is
// recreated, with new certificates from a new CA, long before: the
// maximum age is at most maxMaxAge, and provision recreates a guest
// one day past its maximum age at the latest.
const certValidity = 90 * 24 * time.Hour

// certClockSkew backdates NotBefore so a guest or client whose clock
// is a little behind the host's accepts a fresh certificate.
const certClockSkew = time.Hour

// tlsMaterial is what one provision issues. The CA's private key is
// not part of it: issueTLS signs the server and client certificates
// and drops the key, so it is never written anywhere -- not to the
// guest, not to the host. Re-issuing means recreating the guest, which
// issues everything anew under a new CA.
type tlsMaterial struct {
	CACert     []byte
	ServerCert []byte
	ServerKey  []byte
	ClientCert []byte
	ClientKey  []byte
	NotAfter   time.Time
}

// issueTLS makes a CA, a server certificate for the address clients
// dial (the guest's own address, or 127.0.0.1 behind the test
// harness's forwards) and one client certificate. ECDSA P-256 keys.
func issueTLS(address net.IP, now time.Time) (tlsMaterial, error) {
	if address == nil {
		return tlsMaterial{}, fmt.Errorf("issue TLS: no address for the server certificate")
	}
	notBefore, notAfter := now.Add(-certClockSkew), now.Add(certValidity)

	caKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return tlsMaterial{}, err
	}
	caTemplate := &x509.Certificate{
		SerialNumber:          mustSerial(),
		Subject:               pkix.Name{CommonName: "y-cluster dockerhost CA " + now.UTC().Format("2006-01-02T15:04:05Z"), Organization: []string{"y-cluster"}},
		NotBefore:             notBefore,
		NotAfter:              notAfter,
		IsCA:                  true,
		BasicConstraintsValid: true,
		MaxPathLenZero:        true,
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageCRLSign,
	}
	caDER, err := x509.CreateCertificate(rand.Reader, caTemplate, caTemplate, &caKey.PublicKey, caKey)
	if err != nil {
		return tlsMaterial{}, fmt.Errorf("create CA: %w", err)
	}
	ca, err := x509.ParseCertificate(caDER)
	if err != nil {
		return tlsMaterial{}, err
	}

	leaf := func(cn string, usage x509.ExtKeyUsage, ips []net.IP) (certPEM, keyPEM []byte, err error) {
		key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
		if err != nil {
			return nil, nil, err
		}
		tmpl := &x509.Certificate{
			SerialNumber:          mustSerial(),
			Subject:               pkix.Name{CommonName: cn, Organization: []string{"y-cluster"}},
			NotBefore:             notBefore,
			NotAfter:              notAfter,
			KeyUsage:              x509.KeyUsageDigitalSignature,
			ExtKeyUsage:           []x509.ExtKeyUsage{usage},
			BasicConstraintsValid: true,
			IPAddresses:           ips,
		}
		der, err := x509.CreateCertificate(rand.Reader, tmpl, ca, &key.PublicKey, caKey)
		if err != nil {
			return nil, nil, fmt.Errorf("sign %s: %w", cn, err)
		}
		keyDER, err := x509.MarshalPKCS8PrivateKey(key)
		if err != nil {
			return nil, nil, err
		}
		return pemBlock("CERTIFICATE", der), pemBlock("PRIVATE KEY", keyDER), nil
	}

	m := tlsMaterial{CACert: pemBlock("CERTIFICATE", caDER), NotAfter: notAfter}
	if m.ServerCert, m.ServerKey, err = leaf("y-cluster dockerhost", x509.ExtKeyUsageServerAuth, []net.IP{address}); err != nil {
		return tlsMaterial{}, err
	}
	if m.ClientCert, m.ClientKey, err = leaf("y-cluster dockerhost client", x509.ExtKeyUsageClientAuth, nil); err != nil {
		return tlsMaterial{}, err
	}
	return m, nil
}

func pemBlock(typ string, der []byte) []byte {
	return pem.EncodeToMemory(&pem.Block{Type: typ, Bytes: der})
}

func mustSerial() *big.Int {
	n, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 127))
	if err != nil {
		panic(fmt.Sprintf("serial number: %v", err))
	}
	return n
}

// writeClientDir replaces dir with the client's files under the names
// docker (DOCKER_CERT_PATH) and buildctl (--tlsdir) both read: ca.pem,
// cert.pem and key.pem. The directory is 0700 and every file 0600. The
// new set is complete in a sibling directory before it takes dir's
// place, so a client never reads a mix of two guests' files.
func writeClientDir(dir string, m tlsMaterial) error {
	parent := filepath.Dir(dir)
	if err := os.MkdirAll(parent, 0o700); err != nil {
		return err
	}
	tmp, err := os.MkdirTemp(parent, filepath.Base(dir)+".new-*")
	if err != nil {
		return err
	}
	defer func() { _ = os.RemoveAll(tmp) }()
	if err := os.Chmod(tmp, 0o700); err != nil {
		return err
	}
	for name, content := range map[string][]byte{"ca.pem": m.CACert, "cert.pem": m.ClientCert, "key.pem": m.ClientKey} {
		if err := os.WriteFile(filepath.Join(tmp, name), content, 0o600); err != nil {
			return err
		}
	}
	if err := os.RemoveAll(dir); err != nil {
		return err
	}
	return os.Rename(tmp, dir)
}
