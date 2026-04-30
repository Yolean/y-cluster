package envoygateway

import "fmt"

// EGControllerName is the controllerName Envoy Gateway watches for
// when picking up GatewayClass resources. Fixed by EG; the
// y-cluster-installed GatewayClass references it.
const EGControllerName = "gateway.envoyproxy.io/gatewayclass-controller"

// DNSHintIPAnnotation publishes the host-side IP at which the
// developer's machine reaches the cluster's HTTP ingress, so
// consumer tooling (ystack's y-k8s-ingress-hosts, etc.) can rewrite
// /etc/hosts without depending on user-supplied config or the
// previous OVERRIDE_IP env-var chain.
//
// Lives on the GatewayClass because that resource exists at
// provision time, is cluster-scoped, and is the natural lookup
// point from any Gateway resource (consumers walk Gateway ->
// gatewayClassName -> GatewayClass to find it). Absent annotation
// = no host-side override; consumers fall back to whatever they
// did before.
const DNSHintIPAnnotation = "yolean.se/dns-hint-ip"

// GatewayClassYAML renders the default GatewayClass manifest with
// the configured class name. dnsHintIP is the value the provisioner
// stamps under the DNSHintIPAnnotation; empty string omits the
// annotations block entirely so an absent hint is distinguishable
// from a present-but-empty one.
//
// Pure function so unit tests can pin the rendered shape against
// a known-good baseline.
func GatewayClassYAML(name, dnsHintIP string) []byte {
	var annotations string
	if dnsHintIP != "" {
		annotations = fmt.Sprintf("  annotations:\n    %s: %s\n", DNSHintIPAnnotation, dnsHintIP)
	}
	return []byte(fmt.Sprintf(`---
# y-cluster default GatewayClass for the bundled Envoy Gateway
# install. Consumer Gateway resources reference this name via
# gatewayClassName: %s.
#
# y-cluster does NOT install a cluster Gateway here -- listener
# port and TLS choices belong to the consumer's kustomize bases.
apiVersion: gateway.networking.k8s.io/v1
kind: GatewayClass
metadata:
  name: %s
%sspec:
  controllerName: %s
`, name, name, annotations, EGControllerName))
}

