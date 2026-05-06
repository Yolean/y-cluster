package gateway

import (
	"encoding/json"
	"sort"
	"strconv"
)

// Summary is a derived, fully-typed projection of the cluster's
// reconciled Gateway state into industry-neutral routing-tree
// terms (listener -> host -> route -> match/backend). Built
// deterministically by BuildSummary from the existing State
// fields; no kubectl, no dataplane lookups, no json.RawMessage
// in the output.
//
// The intent is a reader-friendly view that avoids both
// kubernetes-specific terminology (HTTPRoute, ClientTrafficPolicy,
// ParentRef) and envoy-internal terminology (virtual_host, HCM,
// RouteConfiguration). Fields below use the words a network
// operator would reach for: listener / host / route / match /
// backend / redirect.
//
// State.Envoy still ships the verbatim envoy /config_dump for
// callers who need ground truth from the dataplane; Summary is
// the ergonomic tier on top.
type Summary struct {
	// Listeners enumerates each Gateway listener in the cluster
	// (one row per <gateway, listener>). A listener with no
	// attached routes still appears so a consumer can tell
	// "configured but unused" from "not configured at all".
	Listeners []SummaryListener `json:"listeners"`
}

// SummaryListener is the routing-tree root: a single ingress
// terminator (port + protocol) on a specific Gateway.
type SummaryListener struct {
	// Gateway is the qualifying owner in "<namespace>/<name>"
	// form. A cluster with multiple Gateways would surface each
	// listener tagged with its Gateway so consumers can tell
	// them apart.
	Gateway string `json:"gateway"`

	// Name is the listener's section name on the Gateway spec.
	// Used to disambiguate when the same Gateway has more than
	// one listener on the same protocol family.
	Name string `json:"name"`

	// Port is the L4 port the listener binds.
	Port int `json:"port"`

	// Protocol is HTTP / HTTPS / TLS / TCP / UDP -- the
	// gateway-api protocol enum, surfaced as-is.
	Protocol string `json:"protocol"`

	// Programmed is the reconciled Programmed=True signal --
	// "envoy-gateway accepted this listener and is serving it".
	Programmed bool `json:"programmed"`

	// NumTrustedHops, when non-nil, is the per-listener
	// X-Forwarded-For trust depth applied via a
	// ClientTrafficPolicy. Pointer (rather than int + omitempty)
	// so a deliberate 0 is distinguishable from "policy absent".
	// Listener-level placement is correct for envoy-gateway
	// today: ClientTrafficPolicy targets a Gateway (with
	// optional sectionName), not individual route matches.
	NumTrustedHops *int `json:"numTrustedHops,omitempty"`

	// TrustedCIDRs lists the per-listener X-Forwarded-For
	// trusted-source CIDRs from the same ClientTrafficPolicy.
	// numTrustedHops and trustedCIDRs are alternative tuning
	// knobs for the same reverse-proxy-trust problem (count
	// hops vs. trust source ranges); newer envoy-gateway
	// versions treat them as mutually exclusive on a single
	// policy, but a snapshot may surface either or neither.
	TrustedCIDRs []string `json:"trustedCIDRs,omitempty"`

	// Hosts groups routes by the hostname declared on the
	// underlying HTTPRoute / GRPCRoute. Routes that declare no
	// hostname land in the "*" bucket, listed last so a
	// consumer eyeballing the JSON sees the named hosts first.
	Hosts []SummaryHost `json:"hosts"`
}

// SummaryHost groups routes by hostname under one listener.
type SummaryHost struct {
	// Hostname is the literal value declared on the source
	// route, or "*" when the route declares no hostname (catch-
	// all on this listener).
	Hostname string `json:"hostname"`

	// Routes are the route entries that match this hostname on
	// this listener. One entry per <route, rule index>; a single
	// HTTPRoute with three rules produces three entries.
	Routes []SummaryRoute `json:"routes"`
}

// SummaryRoute is one rule of one route attached to a listener.
type SummaryRoute struct {
	// Source identifies the origin route + rule index in
	// "<Kind>/<namespace>/<name>#<rule-idx>" form so a consumer
	// can find the underlying spec object.
	Source string `json:"source"`

	// Matches are the OR'd match conditions on this rule. An
	// empty list means "match everything on this hostname".
	Matches []SummaryMatch `json:"matches"`

	// Backends are the destinations traffic flows to when a
	// match hits. Multiple entries when the rule defines
	// weighted splits, or when both a redirect filter and a
	// backendRef are present.
	Backends []SummaryBackend `json:"backends"`
}

// SummaryMatch is one match clause: path + optional method +
// optional header set. Pluralism (multiple matches per rule) is
// handled by the surrounding SummaryRoute.Matches slice.
type SummaryMatch struct {
	// Path is "<Type>=<Value>" -- e.g. "PathPrefix=/auth",
	// "Exact=/healthz", "RegularExpression=^/api/v[12]/.*".
	// For GRPCRoute matches we render the method clause here as
	// "Method=<Type>:<Service>/<Method>" so consumers don't need
	// a separate field for the gRPC case.
	Path string `json:"path,omitempty"`

	// Method is the HTTP method when the rule constrains it.
	Method string `json:"method,omitempty"`

	// Headers, when non-empty, lists header-name -> expected-value
	// pairs. The match type ("Exact" vs "RegularExpression") is
	// dropped from this projection; consumers who need it walk
	// HTTPRoutes[].rules in State.
	Headers map[string]string `json:"headers,omitempty"`
}

// SummaryBackend is one destination on a match. Exactly one of
// Service or Redirect is set; Type discriminates.
type SummaryBackend struct {
	// Type is "service" or "redirect".
	Type string `json:"type"`

	// Service is set when Type == "service".
	Service *SummaryService `json:"service,omitempty"`

	// Redirect is set when Type == "redirect".
	Redirect *SummaryRedirect `json:"redirect,omitempty"`
}

// SummaryService is the resolved upstream-service reference.
type SummaryService struct {
	Namespace string `json:"namespace,omitempty"`
	Name      string `json:"name"`
	Port      int    `json:"port,omitempty"`
	// Weight is the rule's traffic-split weight when present.
	// Omitted (0) for single-backend rules.
	Weight int `json:"weight,omitempty"`
}

// SummaryRedirect is a RequestRedirect filter projection.
type SummaryRedirect struct {
	Scheme   string `json:"scheme,omitempty"`
	Hostname string `json:"hostname,omitempty"`
	Port     int    `json:"port,omitempty"`
	// Path, when set, is "<Type>=<Value>" (e.g.
	// "ReplacePrefixMatch=/v2"). Empty when the redirect keeps
	// the original path.
	Path string `json:"path,omitempty"`
	// Status is the redirect HTTP status code (commonly 301 or
	// 302). 0 when the source route omits it.
	Status int `json:"status,omitempty"`
}

// BuildSummary derives a Summary from an already-populated
// *State. Pure function, deterministic, no I/O. Callers
// typically invoke it at the end of Fetch() so the produced
// JSON carries both the raw resource view and this projection.
//
// Nil input yields a Summary with an empty Listeners slice (so
// consumers can json.Marshal the result without nil-checking).
func BuildSummary(s *State) *Summary {
	out := &Summary{Listeners: []SummaryListener{}}
	if s == nil {
		return out
	}
	for _, gw := range s.Gateways {
		gwKey := gw.Namespace + "/" + gw.Name
		for _, l := range gw.Listeners {
			sl := SummaryListener{
				Gateway:    gwKey,
				Name:       l.Name,
				Port:       l.Port,
				Protocol:   l.Protocol,
				Programmed: listenerProgrammed(gw, l.Name),
				Hosts:      collectHosts(s, gw, l),
			}
			if xff := xForwardedForFor(s.ClientTrafficPolicies, gw, l); xff != nil {
				if xff.NumTrustedHops != nil {
					v := *xff.NumTrustedHops
					sl.NumTrustedHops = &v
				}
				sl.TrustedCIDRs = xff.TrustedCIDRs
			}
			out.Listeners = append(out.Listeners, sl)
		}
	}
	return out
}

// xffSettings is the projection of a ClientTrafficPolicy's
// spec.clientIPDetection.xForwardedFor block. Returned by
// xForwardedForFor when at least one CTP applies to the
// listener; nil when no policy applies.
type xffSettings struct {
	NumTrustedHops *int
	TrustedCIDRs   []string
}

// listenerProgrammed pulls the Programmed=True signal from the
// matching ListenerStatus row. Default false when the controller
// hasn't reported on this listener yet.
func listenerProgrammed(gw Gateway, listenerName string) bool {
	for _, ls := range gw.Status.Listeners {
		if ls.Name == listenerName {
			return ls.Programmed
		}
	}
	return false
}

// xForwardedForFor walks the ClientTrafficPolicy list and
// returns the first applicable policy's xForwardedFor block
// projected into xffSettings. Returns nil when no CTP applies
// to this listener.
//
// "First" is deterministic because Fetch sorts policies by
// (namespace, name) before populating State. Multiple
// overlapping policies are rare and a reconciliation conflict
// in their own right; consumers wanting full disambiguation
// drill into State.ClientTrafficPolicies.
func xForwardedForFor(ctps []ClientTrafficPolicy, gw Gateway, l Listener) *xffSettings {
	for _, ctp := range ctps {
		if !ctpAppliesTo(ctp, gw, l) {
			continue
		}
		var spec struct {
			ClientIPDetection struct {
				XForwardedFor struct {
					NumTrustedHops *int     `json:"numTrustedHops"`
					TrustedCIDRs   []string `json:"trustedCIDRs"`
				} `json:"xForwardedFor"`
			} `json:"clientIPDetection"`
		}
		if err := json.Unmarshal(ctp.Spec, &spec); err != nil {
			continue
		}
		xff := spec.ClientIPDetection.XForwardedFor
		if xff.NumTrustedHops == nil && len(xff.TrustedCIDRs) == 0 {
			continue
		}
		return &xffSettings{
			NumTrustedHops: xff.NumTrustedHops,
			TrustedCIDRs:   xff.TrustedCIDRs,
		}
	}
	return nil
}

// ctpAppliesTo returns true if ctp's targetRefs include the
// given Gateway, with sectionName either absent (whole-gateway
// scope) or matching this listener.
func ctpAppliesTo(ctp ClientTrafficPolicy, gw Gateway, l Listener) bool {
	for _, t := range parseTargetRefs(ctp.TargetRefs, ctp.Namespace) {
		if t.Kind != "" && t.Kind != "Gateway" {
			continue
		}
		if t.Namespace != gw.Namespace || t.Name != gw.Name {
			continue
		}
		if t.SectionName != "" && t.SectionName != l.Name {
			continue
		}
		return true
	}
	return false
}

// parsedRef is the union shape we extract from parentRefs and
// targetRefs across HTTPRoute / GRPCRoute / *TrafficPolicy.
// Gateway-api and envoy-gateway use the same field names for
// these references, so one parse covers both.
type parsedRef struct {
	Group       string
	Kind        string
	Namespace   string
	Name        string
	SectionName string
	Port        int
}

func parseTargetRefs(refs json.RawMessage, defaultNS string) []parsedRef {
	if len(refs) == 0 {
		return nil
	}
	var raw []struct {
		Group       string `json:"group"`
		Kind        string `json:"kind"`
		Namespace   string `json:"namespace"`
		Name        string `json:"name"`
		SectionName string `json:"sectionName"`
		Port        int    `json:"port"`
	}
	if err := json.Unmarshal(refs, &raw); err != nil {
		return nil
	}
	out := make([]parsedRef, 0, len(raw))
	for _, r := range raw {
		ns := r.Namespace
		if ns == "" {
			ns = defaultNS
		}
		out = append(out, parsedRef{
			Group: r.Group, Kind: r.Kind,
			Namespace: ns, Name: r.Name,
			SectionName: r.SectionName, Port: r.Port,
		})
	}
	return out
}

// routeAttachesTo decides whether a route's parentRefs include
// the given (gw, l). sectionName empty == applies to all
// listeners on the gateway; non-empty must match l.Name. port
// == 0 means "any port"; non-zero must match l.Port.
func routeAttachesTo(parentRefs json.RawMessage, gw Gateway, l Listener, defaultNS string) bool {
	for _, r := range parseTargetRefs(parentRefs, defaultNS) {
		if r.Kind != "" && r.Kind != "Gateway" {
			continue
		}
		if r.Namespace != gw.Namespace || r.Name != gw.Name {
			continue
		}
		if r.SectionName != "" && r.SectionName != l.Name {
			continue
		}
		if r.Port != 0 && r.Port != l.Port {
			continue
		}
		return true
	}
	return false
}

// collectHosts walks the route lists in State and bunches them
// into per-hostname buckets under this listener. "*" is the
// catch-all bucket for routes that declare no hostname; it
// sorts to the end so consumers reading the JSON top-down see
// the named hosts first.
func collectHosts(s *State, gw Gateway, l Listener) []SummaryHost {
	buckets := map[string][]SummaryRoute{}
	addRoute := func(hostnames []string, summarized []SummaryRoute) {
		if len(summarized) == 0 {
			return
		}
		if len(hostnames) == 0 {
			buckets["*"] = append(buckets["*"], summarized...)
			return
		}
		for _, h := range hostnames {
			buckets[h] = append(buckets[h], summarized...)
		}
	}
	for _, r := range s.HTTPRoutes {
		if !routeAttachesTo(r.ParentRefs, gw, l, r.Namespace) {
			continue
		}
		addRoute(r.Hostnames, summarizeHTTPRules(r))
	}
	for _, r := range s.GRPCRoutes {
		if !routeAttachesTo(r.ParentRefs, gw, l, r.Namespace) {
			continue
		}
		addRoute(r.Hostnames, summarizeGRPCRules(r))
	}
	keys := make([]string, 0, len(buckets))
	for k := range buckets {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool {
		// "*" sorts last; everything else alphabetical.
		if keys[i] == "*" {
			return false
		}
		if keys[j] == "*" {
			return true
		}
		return keys[i] < keys[j]
	})
	out := make([]SummaryHost, 0, len(keys))
	for _, k := range keys {
		out = append(out, SummaryHost{Hostname: k, Routes: buckets[k]})
	}
	return out
}

// rawHTTPRule mirrors the gateway-api HTTPRoute rule shape at
// the field level we need. Filter / match types are partial --
// the projection drops the variants we don't render.
type rawHTTPRule struct {
	Matches     []rawHTTPMatch      `json:"matches"`
	Filters     []rawHTTPFilter     `json:"filters"`
	BackendRefs []rawHTTPBackendRef `json:"backendRefs"`
}

type rawHTTPMatch struct {
	Path *struct {
		Type  string `json:"type"`
		Value string `json:"value"`
	} `json:"path"`
	Method  string `json:"method"`
	Headers []struct {
		Name  string `json:"name"`
		Value string `json:"value"`
		Type  string `json:"type"`
	} `json:"headers"`
}

type rawHTTPFilter struct {
	Type            string `json:"type"`
	RequestRedirect *struct {
		Scheme     string `json:"scheme"`
		Hostname   string `json:"hostname"`
		Port       int    `json:"port"`
		StatusCode int    `json:"statusCode"`
		Path       *struct {
			Type               string `json:"type"`
			ReplaceFullPath    string `json:"replaceFullPath"`
			ReplacePrefixMatch string `json:"replacePrefixMatch"`
		} `json:"path"`
	} `json:"requestRedirect"`
}

type rawHTTPBackendRef struct {
	Group     string `json:"group"`
	Kind      string `json:"kind"`
	Namespace string `json:"namespace"`
	Name      string `json:"name"`
	Port      int    `json:"port"`
	Weight    int    `json:"weight"`
}

func summarizeHTTPRules(r HTTPRoute) []SummaryRoute {
	if len(r.Rules) == 0 {
		return nil
	}
	var rules []rawHTTPRule
	if err := json.Unmarshal(r.Rules, &rules); err != nil {
		return nil
	}
	out := make([]SummaryRoute, 0, len(rules))
	for i, rule := range rules {
		out = append(out, SummaryRoute{
			Source:   "HTTPRoute/" + r.Namespace + "/" + r.Name + "#" + strconv.Itoa(i),
			Matches:  summarizeHTTPMatches(rule.Matches),
			Backends: summarizeHTTPBackends(rule.Filters, rule.BackendRefs, r.Namespace),
		})
	}
	return out
}

func summarizeHTTPMatches(matches []rawHTTPMatch) []SummaryMatch {
	out := make([]SummaryMatch, 0, len(matches))
	for _, m := range matches {
		sm := SummaryMatch{Method: m.Method}
		if m.Path != nil {
			t := m.Path.Type
			if t == "" {
				// gateway-api default match type for path is
				// PathPrefix when omitted.
				t = "PathPrefix"
			}
			sm.Path = t + "=" + m.Path.Value
		}
		if len(m.Headers) > 0 {
			sm.Headers = make(map[string]string, len(m.Headers))
			for _, h := range m.Headers {
				sm.Headers[h.Name] = h.Value
			}
		}
		out = append(out, sm)
	}
	return out
}

func summarizeHTTPBackends(filters []rawHTTPFilter, refs []rawHTTPBackendRef, defaultNS string) []SummaryBackend {
	var out []SummaryBackend
	for _, f := range filters {
		if f.Type != "RequestRedirect" || f.RequestRedirect == nil {
			continue
		}
		sr := &SummaryRedirect{
			Scheme:   f.RequestRedirect.Scheme,
			Hostname: f.RequestRedirect.Hostname,
			Port:     f.RequestRedirect.Port,
			Status:   f.RequestRedirect.StatusCode,
		}
		if p := f.RequestRedirect.Path; p != nil {
			switch {
			case p.ReplaceFullPath != "":
				sr.Path = "ReplaceFullPath=" + p.ReplaceFullPath
			case p.ReplacePrefixMatch != "":
				sr.Path = "ReplacePrefixMatch=" + p.ReplacePrefixMatch
			}
		}
		out = append(out, SummaryBackend{Type: "redirect", Redirect: sr})
	}
	for _, r := range refs {
		// Skip non-Service backendRefs (e.g. envoy-gateway's
		// Backend CRD); their resolution isn't representable as
		// a service tuple. Consumers that need them drill into
		// State.HTTPRoutes[].rules.
		if r.Kind != "" && r.Kind != "Service" {
			continue
		}
		ns := r.Namespace
		if ns == "" {
			ns = defaultNS
		}
		out = append(out, SummaryBackend{
			Type: "service",
			Service: &SummaryService{
				Namespace: ns,
				Name:      r.Name,
				Port:      r.Port,
				Weight:    r.Weight,
			},
		})
	}
	return out
}

// rawGRPCRule is the GRPCRoute rule shape. Distinct from
// HTTPRoute's because gRPC matches on (service, method) rather
// than path. Backends use the same shape.
type rawGRPCRule struct {
	Matches     []rawGRPCMatch      `json:"matches"`
	BackendRefs []rawHTTPBackendRef `json:"backendRefs"`
}

type rawGRPCMatch struct {
	Method *struct {
		Type    string `json:"type"`
		Service string `json:"service"`
		Method  string `json:"method"`
	} `json:"method"`
	Headers []struct {
		Name  string `json:"name"`
		Value string `json:"value"`
		Type  string `json:"type"`
	} `json:"headers"`
}

func summarizeGRPCRules(r GRPCRoute) []SummaryRoute {
	if len(r.Rules) == 0 {
		return nil
	}
	var rules []rawGRPCRule
	if err := json.Unmarshal(r.Rules, &rules); err != nil {
		return nil
	}
	out := make([]SummaryRoute, 0, len(rules))
	for i, rule := range rules {
		out = append(out, SummaryRoute{
			Source:   "GRPCRoute/" + r.Namespace + "/" + r.Name + "#" + strconv.Itoa(i),
			Matches:  summarizeGRPCMatches(rule.Matches),
			Backends: summarizeHTTPBackends(nil, rule.BackendRefs, r.Namespace),
		})
	}
	return out
}

func summarizeGRPCMatches(matches []rawGRPCMatch) []SummaryMatch {
	out := make([]SummaryMatch, 0, len(matches))
	for _, m := range matches {
		sm := SummaryMatch{}
		if m.Method != nil {
			t := m.Method.Type
			if t == "" {
				t = "Exact"
			}
			svc := m.Method.Service
			meth := m.Method.Method
			// Render as "Method=<Type>:<Service>/<Method>". Empty
			// service or method just collapse the separator so we
			// don't emit "/" or ":/" with stray punctuation.
			body := svc
			if svc != "" && meth != "" {
				body = svc + "/" + meth
			} else if meth != "" {
				body = "/" + meth
			}
			sm.Path = "Method=" + t + ":" + body
		}
		if len(m.Headers) > 0 {
			sm.Headers = make(map[string]string, len(m.Headers))
			for _, h := range m.Headers {
				sm.Headers[h.Name] = h.Value
			}
		}
		out = append(out, sm)
	}
	return out
}
