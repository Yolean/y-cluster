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
// from a present-but-empty one. envoyProxyName, when non-empty,
// adds a parametersRef pointing at the EnvoyProxy CR of that name
// in the envoy-gateway-system namespace -- the upstream-blessed
// extension point for tuning the data-plane proxy's resources.
//
// Pure function so unit tests can pin the rendered shape against
// a known-good baseline.
func GatewayClassYAML(name, dnsHintIP, envoyProxyName string) []byte {
	var annotations string
	if dnsHintIP != "" {
		annotations = fmt.Sprintf("  annotations:\n    %s: %s\n", DNSHintIPAnnotation, dnsHintIP)
	}
	var parametersRef string
	if envoyProxyName != "" {
		parametersRef = fmt.Sprintf(`  parametersRef:
    group: gateway.envoyproxy.io
    kind: EnvoyProxy
    name: %s
    namespace: %s
`, envoyProxyName, Namespace)
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
%s`, name, name, annotations, EGControllerName, parametersRef))
}

// EnvoyProxyName is the metadata.name of the EnvoyProxy CR
// y-cluster applies in the envoy-gateway-system namespace. The
// default GatewayClass references it via parametersRef so
// Gateways under that class inherit the tuned resources without
// any per-Gateway boilerplate.
const EnvoyProxyName = "y-cluster"

// EnvoyProxyYAML renders the EnvoyProxy CR that tunes the
// data-plane envoy proxy pod's resource requests. cpuRequest /
// memRequest land under spec.provider.kubernetes.envoyDeployment
// .container.resources.requests. Limits are left for EG's
// defaults (and the cluster's LimitRange, if any).
//
// The CR lives in envoy-gateway-system because that's the only
// namespace EG looks at for parametersRef of GatewayClass.
//
// Pure function for unit-test pinning.
func EnvoyProxyYAML(cpuRequest, memRequest string) []byte {
	return []byte(fmt.Sprintf(`---
# y-cluster's tuning for the per-Gateway envoy proxy pod.
# Referenced by the GatewayClass via parametersRef.
apiVersion: gateway.envoyproxy.io/v1alpha1
kind: EnvoyProxy
metadata:
  name: %s
  namespace: %s
spec:
  provider:
    type: Kubernetes
    kubernetes:
      envoyDeployment:
        container:
          resources:
            requests:
              cpu: %s
              memory: %s
`, EnvoyProxyName, Namespace, cpuRequest, memRequest))
}

// ControllerResourcesYAML is a partial Deployment manifest
// declaring ownership over the envoy-gateway controller
// container's resources.requests under server-side apply. The
// apply uses field-manager y-cluster; existing fields (image,
// env, replicas, container args) stay with their original
// owners. Limits are not declared -- intentional, so the
// upstream limit (currently 1Gi memory, no CPU cap) stays in
// effect.
func ControllerResourcesYAML(cpuRequest, memRequest string) []byte {
	return []byte(fmt.Sprintf(`---
apiVersion: apps/v1
kind: Deployment
metadata:
  name: %s
  namespace: %s
spec:
  template:
    spec:
      containers:
      - name: envoy-gateway
        resources:
          requests:
            cpu: %s
            memory: %s
`, DeploymentName, Namespace, cpuRequest, memRequest))
}

