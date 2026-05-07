package gateway

import (
	"context"
	"encoding/json"
	"sort"
	"strings"

	"github.com/Yolean/y-cluster/pkg/provision/envoygateway"
)

// raw kubectl item shapes. We unmarshal kubectl's JSON output
// into these intermediate types, then project the fields we
// care about into the typed *State* shape. Most fields stay as
// json.RawMessage so kubectl's exact output passes through
// without our partial schema getting between consumers and the
// upstream gateway-api types.

type rawList struct {
	Items []json.RawMessage `json:"items"`
}

type rawMetadata struct {
	Name        string            `json:"name"`
	Namespace   string            `json:"namespace,omitempty"`
	Annotations map[string]string `json:"annotations,omitempty"`
	Labels      map[string]string `json:"labels,omitempty"`
}

type rawCondition struct {
	Type    string `json:"type"`
	Status  string `json:"status"`
	Reason  string `json:"reason,omitempty"`
	Message string `json:"message,omitempty"`
}

func toConditions(in []rawCondition) []Condition {
	if len(in) == 0 {
		return nil
	}
	out := make([]Condition, len(in))
	for i, c := range in {
		out[i] = Condition(c)
	}
	return out
}

// === GatewayClass ===

type rawGatewayClass struct {
	Metadata rawMetadata `json:"metadata"`
	Spec     struct {
		ControllerName string `json:"controllerName"`
	} `json:"spec"`
	Status struct {
		Conditions []rawCondition `json:"conditions"`
	} `json:"status"`
}

func fetchGatewayClass(ctx context.Context, kubectlContext string) (*GatewayClass, error) {
	var list rawList
	if err := kubectlGetJSON(ctx, kubectlContext, "gatewayclass", &list); err != nil {
		return nil, err
	}
	for _, raw := range list.Items {
		var gc rawGatewayClass
		if err := json.Unmarshal(raw, &gc); err != nil {
			return nil, err
		}
		if gc.Spec.ControllerName != envoygateway.EGControllerName {
			continue
		}
		return &GatewayClass{
			Name:           gc.Metadata.Name,
			ControllerName: gc.Spec.ControllerName,
			Annotations:    gc.Metadata.Annotations,
			Labels:         gc.Metadata.Labels,
			Conditions:     toConditions(gc.Status.Conditions),
		}, nil
	}
	// No envoy-gateway-controlled GatewayClass yet; return nil
	// rather than error -- the cluster may be pre-install.
	return nil, nil
}

// === Gateway ===

type rawGateway struct {
	Metadata rawMetadata `json:"metadata"`
	Spec     struct {
		GatewayClassName string         `json:"gatewayClassName"`
		Listeners        []rawListener  `json:"listeners"`
	} `json:"spec"`
	Status struct {
		Conditions []rawCondition         `json:"conditions"`
		Listeners  []rawListenerStatus    `json:"listeners"`
	} `json:"status"`
}

type rawListener struct {
	Name          string          `json:"name"`
	Port          int             `json:"port"`
	Protocol      string          `json:"protocol"`
	Hostname      string          `json:"hostname,omitempty"`
	AllowedRoutes json.RawMessage `json:"allowedRoutes,omitempty"`
	TLS           json.RawMessage `json:"tls,omitempty"`
}

type rawListenerStatus struct {
	Name           string         `json:"name"`
	AttachedRoutes int            `json:"attachedRoutes"`
	Conditions     []rawCondition `json:"conditions"`
}

func fetchGateways(ctx context.Context, kubectlContext string, out *State) error {
	var list rawList
	if err := kubectlGetJSON(ctx, kubectlContext, "gateway", &list); err != nil {
		return err
	}
	gws := make([]Gateway, 0, len(list.Items))
	for _, raw := range list.Items {
		var g rawGateway
		if err := json.Unmarshal(raw, &g); err != nil {
			return err
		}
		listeners := make([]Listener, len(g.Spec.Listeners))
		for i, l := range g.Spec.Listeners {
			listeners[i] = Listener(l)
		}
		listenerStatus := make([]ListenerStatus, len(g.Status.Listeners))
		for i, ls := range g.Status.Listeners {
			listenerStatus[i] = ListenerStatus{
				Name:           ls.Name,
				AttachedRoutes: ls.AttachedRoutes,
				Conditions:     toConditions(ls.Conditions),
				Programmed:     hasTrueCondition(ls.Conditions, "Programmed"),
			}
		}
		gws = append(gws, Gateway{
			Namespace:        g.Metadata.Namespace,
			Name:             g.Metadata.Name,
			GatewayClassName: g.Spec.GatewayClassName,
			Listeners:        listeners,
			Status: GatewayStatus{
				Conditions: toConditions(g.Status.Conditions),
				Listeners:  listenerStatus,
			},
		})
	}
	sortGateways(gws)
	out.Gateways = gws
	return nil
}

func hasTrueCondition(conditions []rawCondition, t string) bool {
	for _, c := range conditions {
		if c.Type == t && strings.EqualFold(c.Status, "True") {
			return true
		}
	}
	return false
}

func sortGateways(in []Gateway) {
	sort.Slice(in, func(i, j int) bool {
		if in[i].Namespace != in[j].Namespace {
			return in[i].Namespace < in[j].Namespace
		}
		return in[i].Name < in[j].Name
	})
}

// === Routes (HTTPRoute + GRPCRoute share the shape) ===

type rawRoute struct {
	Metadata rawMetadata `json:"metadata"`
	Spec     struct {
		ParentRefs json.RawMessage `json:"parentRefs,omitempty"`
		Hostnames  []string        `json:"hostnames,omitempty"`
		Rules      json.RawMessage `json:"rules,omitempty"`
	} `json:"spec"`
	Status struct {
		Parents []rawRouteParentStatus `json:"parents"`
	} `json:"status"`
}

type rawRouteParentStatus struct {
	ParentRef      json.RawMessage `json:"parentRef,omitempty"`
	ControllerName string          `json:"controllerName,omitempty"`
	Conditions     []rawCondition  `json:"conditions"`
}

func toRouteParents(in []rawRouteParentStatus) []RouteParentStatus {
	if len(in) == 0 {
		return nil
	}
	out := make([]RouteParentStatus, len(in))
	for i, p := range in {
		out[i] = RouteParentStatus{
			ParentRef:      p.ParentRef,
			ControllerName: p.ControllerName,
			Conditions:     toConditions(p.Conditions),
		}
	}
	return out
}

func fetchHTTPRoutes(ctx context.Context, kubectlContext string, out *State) error {
	var list rawList
	if err := kubectlGetJSON(ctx, kubectlContext, "httproute", &list); err != nil {
		return err
	}
	routes := make([]HTTPRoute, 0, len(list.Items))
	for _, raw := range list.Items {
		var r rawRoute
		if err := json.Unmarshal(raw, &r); err != nil {
			return err
		}
		routes = append(routes, HTTPRoute{
			Namespace:  r.Metadata.Namespace,
			Name:       r.Metadata.Name,
			ParentRefs: r.Spec.ParentRefs,
			Hostnames:  r.Spec.Hostnames,
			Rules:      r.Spec.Rules,
			Status:     RouteStatus{Parents: toRouteParents(r.Status.Parents)},
		})
	}
	sort.Slice(routes, func(i, j int) bool {
		if routes[i].Namespace != routes[j].Namespace {
			return routes[i].Namespace < routes[j].Namespace
		}
		return routes[i].Name < routes[j].Name
	})
	out.HTTPRoutes = routes
	return nil
}

func fetchGRPCRoutes(ctx context.Context, kubectlContext string, out *State) error {
	var list rawList
	if err := kubectlGetJSON(ctx, kubectlContext, "grpcroute", &list); err != nil {
		return err
	}
	routes := make([]GRPCRoute, 0, len(list.Items))
	for _, raw := range list.Items {
		var r rawRoute
		if err := json.Unmarshal(raw, &r); err != nil {
			return err
		}
		routes = append(routes, GRPCRoute{
			Namespace:  r.Metadata.Namespace,
			Name:       r.Metadata.Name,
			ParentRefs: r.Spec.ParentRefs,
			Hostnames:  r.Spec.Hostnames,
			Rules:      r.Spec.Rules,
			Status:     RouteStatus{Parents: toRouteParents(r.Status.Parents)},
		})
	}
	sort.Slice(routes, func(i, j int) bool {
		if routes[i].Namespace != routes[j].Namespace {
			return routes[i].Namespace < routes[j].Namespace
		}
		return routes[i].Name < routes[j].Name
	})
	out.GRPCRoutes = routes
	return nil
}

// === Envoy Gateway extension policies ===

type rawPolicy struct {
	Metadata rawMetadata `json:"metadata"`
	Spec     json.RawMessage `json:"spec"`
	Status   struct {
		Ancestors []rawPolicyAncestorStatus `json:"ancestors"`
	} `json:"status"`
}

type rawPolicyAncestorStatus struct {
	AncestorRef    json.RawMessage `json:"ancestorRef,omitempty"`
	ControllerName string          `json:"controllerName,omitempty"`
	Conditions     []rawCondition  `json:"conditions"`
}

func toPolicyAncestors(in []rawPolicyAncestorStatus) []PolicyAncestorStatus {
	if len(in) == 0 {
		return nil
	}
	out := make([]PolicyAncestorStatus, len(in))
	for i, a := range in {
		out[i] = PolicyAncestorStatus{
			AncestorRef:    a.AncestorRef,
			ControllerName: a.ControllerName,
			Conditions:     toConditions(a.Conditions),
		}
	}
	return out
}

// extractTargetRefs pulls the policy's targetRefs (or singular
// targetRef) field out of its spec. Both shapes appear in the
// envoy-gateway extension's CRDs depending on version. We
// surface them in a normalised `targetRefs` field at the top
// level of our snapshot so consumers don't have to chase the
// spec shape.
func extractTargetRefs(spec json.RawMessage) json.RawMessage {
	if len(spec) == 0 {
		return nil
	}
	var probe struct {
		TargetRefs json.RawMessage `json:"targetRefs,omitempty"`
		TargetRef  json.RawMessage `json:"targetRef,omitempty"`
	}
	if err := json.Unmarshal(spec, &probe); err != nil {
		return nil
	}
	if len(probe.TargetRefs) > 0 {
		return probe.TargetRefs
	}
	if len(probe.TargetRef) > 0 {
		// Wrap the singular targetRef into a single-element array
		// so consumers see one consistent shape.
		wrapped := []json.RawMessage{probe.TargetRef}
		b, err := json.Marshal(wrapped)
		if err != nil {
			return nil
		}
		return b
	}
	return nil
}

func fetchClientTrafficPolicies(ctx context.Context, kubectlContext string, out *State) error {
	var list rawList
	if err := kubectlGetJSON(ctx, kubectlContext, "clienttrafficpolicy", &list); err != nil {
		return err
	}
	policies := make([]ClientTrafficPolicy, 0, len(list.Items))
	for _, raw := range list.Items {
		var p rawPolicy
		if err := json.Unmarshal(raw, &p); err != nil {
			return err
		}
		policies = append(policies, ClientTrafficPolicy{
			Namespace:  p.Metadata.Namespace,
			Name:       p.Metadata.Name,
			TargetRefs: extractTargetRefs(p.Spec),
			Spec:       p.Spec,
			Status:     PolicyStatus{Ancestors: toPolicyAncestors(p.Status.Ancestors)},
		})
	}
	sort.Slice(policies, func(i, j int) bool {
		if policies[i].Namespace != policies[j].Namespace {
			return policies[i].Namespace < policies[j].Namespace
		}
		return policies[i].Name < policies[j].Name
	})
	out.ClientTrafficPolicies = policies
	return nil
}

func fetchBackendTrafficPolicies(ctx context.Context, kubectlContext string, out *State) error {
	var list rawList
	if err := kubectlGetJSON(ctx, kubectlContext, "backendtrafficpolicy", &list); err != nil {
		return err
	}
	policies := make([]BackendTrafficPolicy, 0, len(list.Items))
	for _, raw := range list.Items {
		var p rawPolicy
		if err := json.Unmarshal(raw, &p); err != nil {
			return err
		}
		policies = append(policies, BackendTrafficPolicy{
			Namespace:  p.Metadata.Namespace,
			Name:       p.Metadata.Name,
			TargetRefs: extractTargetRefs(p.Spec),
			Spec:       p.Spec,
			Status:     PolicyStatus{Ancestors: toPolicyAncestors(p.Status.Ancestors)},
		})
	}
	sort.Slice(policies, func(i, j int) bool {
		if policies[i].Namespace != policies[j].Namespace {
			return policies[i].Namespace < policies[j].Namespace
		}
		return policies[i].Name < policies[j].Name
	})
	out.BackendTrafficPolicies = policies
	return nil
}
