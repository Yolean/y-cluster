package envoygateway

import "fmt"

// EGControllerName is the controllerName Envoy Gateway watches for
// when picking up GatewayClass resources. Fixed by EG; the
// y-cluster-installed GatewayClass references it.
const EGControllerName = "gateway.envoyproxy.io/gatewayclass-controller"

// DNSHintIPAnnotation publishes the host-side IP at which the
// developer's machine reaches the cluster's HTTP ingress, so
// consumer tooling (ystack's y-k8s-ingress-hosts, etc.) can rewrite
// /etc/hosts without depending on user-supplied config or
// environment variables.
//
// Lives on the GatewayClass because that resource exists at
// provision time, is cluster-scoped, and is the natural lookup
// point from any Gateway resource (consumers walk Gateway ->
// gatewayClassName -> GatewayClass to find it). Absent annotation
// = no host-side override; consumers resolve the address their
// own way.
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
// externalTrafficPolicy Local is what lets a workload see the real
// client address. k3s ServiceLB (klipper-lb) publishes the node IP
// as the Service's load balancer address, and kube-proxy's rule for
// that address runs before the klipper hostPort rule; with Local it
// hands the connection to the envoy pod without SNAT. With Cluster,
// or before ServiceLB has published the address, the connection
// goes through the klipper pod, whose own MASQUERADE rule replaces
// the client address with the pod's. It is also EG's default, but
// too much depends on it to leave it implicit.
//
// The CR lives in envoy-gateway-system because that's the only
// namespace EG looks at for parametersRef of GatewayClass.
//
// externalIPs, when given, is for a cluster with no load balancer
// implementation at all (Talos on a rented server: no ServiceLB, no
// cloud controller). The envoy Service becomes a NodePort with
// spec.externalIPs set to the node's public addresses, which is
// what makes kube-proxy accept connections on <address>:80 and 443
// and hand them to the envoy pod. That is the ServiceLB outcome
// without ServiceLB. NodePort rather than LoadBalancer so the
// Service does not sit in Pending forever; externalTrafficPolicy
// Local applies to externalIPs traffic as it does to a load
// balancer's, so the client address still reaches the workload.
// EnvoyProxy has no field for externalIPs; the service patch is
// the upstream-blessed way to set what it has no field for.
//
// daemonSet runs the envoy fleet as a DaemonSet instead of a
// Deployment. With externalTrafficPolicy Local a node without an
// envoy pod drops what arrives on its address, so a cluster that
// lists several nodes' addresses needs envoy on each of them; a
// DaemonSet is the guarantee. Tainted nodes (a dedicated control
// plane) are skipped, which is right: their addresses are not
// listed.
//
// Pure function for unit-test pinning.
func EnvoyProxyYAML(cpuRequest, memRequest string, externalIPs []string, daemonSet, mergeGateways bool) []byte {
	var service string
	if len(externalIPs) > 0 {
		service = "        type: NodePort\n        patch:\n          type: StrategicMerge\n          value:\n            spec:\n              externalIPs:\n"
		for _, ip := range externalIPs {
			service += "                - " + ip + "\n"
		}
	}
	fleet := "envoyDeployment"
	if daemonSet {
		fleet = "envoyDaemonSet"
	}
	var merge string
	if mergeGateways {
		merge = "  mergeGateways: true\n"
	}
	return []byte(fmt.Sprintf(`---
# y-cluster's tuning for the per-Gateway envoy proxy pod.
# Referenced by the GatewayClass via parametersRef.
apiVersion: gateway.envoyproxy.io/v1alpha1
kind: EnvoyProxy
metadata:
  name: %s
  namespace: %s
spec:
%s  provider:
    type: Kubernetes
    kubernetes:
      envoyService:
        externalTrafficPolicy: Local
%s      %s:
        container:
          resources:
            requests:
              cpu: %s
              memory: %s
`, EnvoyProxyName, Namespace, merge, service, fleet, cpuRequest, memRequest))
}

// ControllerResourcesPatch is a strategic-merge patch body for
// the envoy-gateway controller container's resources.requests.
// Applied via `kubectl patch deployment envoy-gateway -n
// envoy-gateway-system --type=strategic --patch '<this>'`.
//
// We use kubectl patch rather than `kubectl apply --server-side`
// with a partial Deployment because the Deployment kind has
// required fields (spec.selector, template.metadata.labels,
// containers[*].image) that kubectl validates client-side before
// sending -- a partial SSA manifest fails with "Required value"
// errors even when SSA semantics would otherwise have merged
// cleanly. Strategic merge patch only validates the merge
// RESULT, which keeps the existing required fields intact.
//
// Strategic merge handles the containers list by merge key
// (name): the existing container's image, env, ports, args stay
// intact; only resources.requests fields land. Limits aren't
// declared, so the upstream limit (currently 1Gi memory, no
// CPU cap) stays in effect.
func ControllerResourcesPatch(cpuRequest, memRequest string) []byte {
	return []byte(fmt.Sprintf(`spec:
  template:
    spec:
      containers:
      - name: envoy-gateway
        resources:
          requests:
            cpu: %s
            memory: %s
`, cpuRequest, memRequest))
}
