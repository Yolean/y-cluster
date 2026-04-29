package envoygateway

import "fmt"

// EGControllerName is the controllerName Envoy Gateway watches for
// when picking up GatewayClass resources. Fixed by EG; the
// y-cluster-installed GatewayClass references it.
const EGControllerName = "gateway.envoyproxy.io/gatewayclass-controller"

// GatewayClassYAML renders the default GatewayClass manifest with
// the configured class name. The YAML body is small enough
// (~10 lines) that we'd previously embedded it verbatim with the
// name hardcoded; rendering inline lets the operator pick a
// non-default name via cluster config.
//
// Pure function so unit tests can pin the rendered shape against
// a known-good baseline.
func GatewayClassYAML(name string) []byte {
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
spec:
  controllerName: %s
`, name, name, EGControllerName))
}
