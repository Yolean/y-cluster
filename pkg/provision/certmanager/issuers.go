package certmanager

import "fmt"

// Names of the issuers y-cluster installs. SelfSignedIssuer only signs
// the CA; CAIssuer is what workloads reference for in-cluster
// certificates (webhooks, Gateway TLS until a site has a public
// issuer). The CA's key stays in the cluster, in CASecret.
const (
	SelfSignedIssuer = "y-cluster-selfsigned"
	CAIssuer         = "y-cluster-ca"
	CACertificate    = "y-cluster-ca"
	CASecret         = "y-cluster-ca"
)

// IssuersYAML renders the self-signed bootstrap ClusterIssuer, the CA
// Certificate it signs (in Namespace, where cert-manager looks up a
// ClusterIssuer's CA secret), and the CA ClusterIssuer. Pure, so tests
// pin the shape.
func IssuersYAML() []byte {
	return []byte(fmt.Sprintf(`apiVersion: cert-manager.io/v1
kind: ClusterIssuer
metadata:
  name: %[1]s
spec:
  selfSigned: {}
---
apiVersion: cert-manager.io/v1
kind: Certificate
metadata:
  name: %[2]s
  namespace: %[5]s
spec:
  isCA: true
  commonName: %[2]s
  secretName: %[3]s
  duration: 87600h
  privateKey:
    algorithm: ECDSA
    size: 256
  issuerRef:
    group: cert-manager.io
    kind: ClusterIssuer
    name: %[1]s
---
apiVersion: cert-manager.io/v1
kind: ClusterIssuer
metadata:
  name: %[4]s
spec:
  ca:
    secretName: %[3]s
`, SelfSignedIssuer, CACertificate, CASecret, CAIssuer, Namespace))
}
