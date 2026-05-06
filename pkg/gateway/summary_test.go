package gateway

import (
	"encoding/json"
	"testing"
)

// TestBuildSummary_Nil documents the nil-safety contract:
// callers can BuildSummary(nil) without panicking and get a
// non-nil Summary with an empty Listeners slice (so consumers
// can json.Marshal without nil-checks).
func TestBuildSummary_Nil(t *testing.T) {
	s := BuildSummary(nil)
	if s == nil {
		t.Fatal("BuildSummary(nil) returned nil; want empty Summary")
	}
	if s.Listeners == nil {
		t.Errorf("Listeners is nil; want []SummaryListener{}")
	}
	if len(s.Listeners) != 0 {
		t.Errorf("Listeners len=%d; want 0", len(s.Listeners))
	}
}

// TestBuildSummary_HTTPRouteServiceBackend pins the basic
// shape: one Gateway with one HTTPS listener; one HTTPRoute
// declaring a hostname, a PathPrefix match, and a Service
// backend. The summary should produce one listener row, one
// host bucket for the declared hostname, one route row with
// path "PathPrefix=/" and a service backend.
func TestBuildSummary_HTTPRouteServiceBackend(t *testing.T) {
	st := &State{
		Gateways: []Gateway{{
			Namespace: "y-cluster",
			Name:      "y-cluster",
			Listeners: []Listener{{Name: "https", Port: 443, Protocol: "HTTPS"}},
			Status: GatewayStatus{
				Listeners: []ListenerStatus{{Name: "https", Programmed: true}},
			},
		}},
		HTTPRoutes: []HTTPRoute{{
			Namespace:  "keycloak-v3",
			Name:       "keycloak-admin",
			ParentRefs: rawJSON(t, `[{"name":"y-cluster","namespace":"y-cluster","sectionName":"https"}]`),
			Hostnames:  []string{"keycloak-admin"},
			Rules: rawJSON(t, `[{
				"matches": [{"path": {"type": "PathPrefix", "value": "/"}}],
				"backendRefs": [{"name": "keycloak", "namespace": "keycloak-v3", "port": 8080}]
			}]`),
		}},
	}

	sum := BuildSummary(st)
	if len(sum.Listeners) != 1 {
		t.Fatalf("got %d listeners, want 1: %+v", len(sum.Listeners), sum)
	}
	l := sum.Listeners[0]
	if l.Gateway != "y-cluster/y-cluster" {
		t.Errorf("listener.gateway=%q want %q", l.Gateway, "y-cluster/y-cluster")
	}
	if l.Port != 443 || l.Protocol != "HTTPS" || !l.Programmed {
		t.Errorf("listener port/proto/programmed: %+v", l)
	}
	if l.NumTrustedHops != nil {
		t.Errorf("numTrustedHops should be nil with no CTP, got %d", *l.NumTrustedHops)
	}
	if len(l.Hosts) != 1 || l.Hosts[0].Hostname != "keycloak-admin" {
		t.Fatalf("hosts: %+v", l.Hosts)
	}
	rs := l.Hosts[0].Routes
	if len(rs) != 1 {
		t.Fatalf("routes: %+v", rs)
	}
	if rs[0].Source != "HTTPRoute/keycloak-v3/keycloak-admin#0" {
		t.Errorf("source=%q", rs[0].Source)
	}
	if len(rs[0].Matches) != 1 || rs[0].Matches[0].Path != "PathPrefix=/" {
		t.Errorf("matches: %+v", rs[0].Matches)
	}
	if len(rs[0].Backends) != 1 || rs[0].Backends[0].Type != "service" {
		t.Fatalf("backends: %+v", rs[0].Backends)
	}
	svc := rs[0].Backends[0].Service
	if svc == nil || svc.Name != "keycloak" || svc.Namespace != "keycloak-v3" || svc.Port != 8080 {
		t.Errorf("service: %+v", svc)
	}
}

// TestBuildSummary_RedirectFilter pins the redirect-only case:
// port 80 with a RequestRedirect filter and no backendRefs
// should surface as a single backend of type "redirect" with
// scheme + status carried through.
func TestBuildSummary_RedirectFilter(t *testing.T) {
	st := &State{
		Gateways: []Gateway{{
			Namespace: "y-cluster",
			Name:      "y-cluster",
			Listeners: []Listener{{Name: "http", Port: 80, Protocol: "HTTP"}},
			Status: GatewayStatus{
				Listeners: []ListenerStatus{{Name: "http", Programmed: true}},
			},
		}},
		HTTPRoutes: []HTTPRoute{{
			Namespace:  "y-cluster",
			Name:       "external-http",
			ParentRefs: rawJSON(t, `[{"name":"y-cluster","sectionName":"http"}]`),
			Hostnames:  []string{"keycloak-admin.example.com"},
			Rules: rawJSON(t, `[{
				"filters": [{"type": "RequestRedirect", "requestRedirect": {"scheme": "https", "statusCode": 301}}]
			}]`),
		}},
	}

	sum := BuildSummary(st)
	rs := sum.Listeners[0].Hosts[0].Routes
	if len(rs) != 1 || len(rs[0].Backends) != 1 {
		t.Fatalf("backends: %+v", rs)
	}
	b := rs[0].Backends[0]
	if b.Type != "redirect" || b.Redirect == nil {
		t.Fatalf("backend not a redirect: %+v", b)
	}
	if b.Redirect.Scheme != "https" || b.Redirect.Status != 301 {
		t.Errorf("redirect scheme/status: %+v", b.Redirect)
	}
}

// TestBuildSummary_NoHostnameBucketsAsStar pins the wildcard
// bucket: a route declaring no hostname must show up under the
// "*" host so consumers can tell "served on all hosts" from
// "served on no host".
func TestBuildSummary_NoHostnameBucketsAsStar(t *testing.T) {
	st := &State{
		Gateways: []Gateway{{
			Namespace: "y-cluster",
			Name:      "y-cluster",
			Listeners: []Listener{{Name: "https", Port: 443, Protocol: "HTTPS"}},
		}},
		HTTPRoutes: []HTTPRoute{{
			Namespace:  "y-cluster",
			Name:       "fallback",
			ParentRefs: rawJSON(t, `[{"name":"y-cluster"}]`),
			// no hostnames
			Rules: rawJSON(t, `[{
				"backendRefs": [{"name": "echo", "port": 80}]
			}]`),
		}},
	}

	sum := BuildSummary(st)
	hosts := sum.Listeners[0].Hosts
	if len(hosts) != 1 || hosts[0].Hostname != "*" {
		t.Errorf("host bucket: %+v (want single \"*\")", hosts)
	}
}

// TestBuildSummary_StarSortsLast pins the order: named hosts
// alphabetical, "*" trailing. A consumer skimming the JSON
// should see the named hosts first.
func TestBuildSummary_StarSortsLast(t *testing.T) {
	st := &State{
		Gateways: []Gateway{{
			Namespace: "y-cluster",
			Name:      "y-cluster",
			Listeners: []Listener{{Name: "https", Port: 443, Protocol: "HTTPS"}},
		}},
		HTTPRoutes: []HTTPRoute{
			{
				Namespace:  "y-cluster",
				Name:       "catch-all",
				ParentRefs: rawJSON(t, `[{"name":"y-cluster"}]`),
				Rules:      rawJSON(t, `[{"backendRefs": [{"name": "echo"}]}]`),
			},
			{
				Namespace:  "y-cluster",
				Name:       "zeta",
				ParentRefs: rawJSON(t, `[{"name":"y-cluster"}]`),
				Hostnames:  []string{"zeta.example.com"},
				Rules:      rawJSON(t, `[{"backendRefs": [{"name": "echo"}]}]`),
			},
			{
				Namespace:  "y-cluster",
				Name:       "alpha",
				ParentRefs: rawJSON(t, `[{"name":"y-cluster"}]`),
				Hostnames:  []string{"alpha.example.com"},
				Rules:      rawJSON(t, `[{"backendRefs": [{"name": "echo"}]}]`),
			},
		},
	}

	hosts := BuildSummary(st).Listeners[0].Hosts
	if len(hosts) != 3 {
		t.Fatalf("hosts: %+v", hosts)
	}
	want := []string{"alpha.example.com", "zeta.example.com", "*"}
	for i, h := range hosts {
		if h.Hostname != want[i] {
			t.Errorf("host[%d]=%q want %q", i, h.Hostname, want[i])
		}
	}
}

// TestBuildSummary_MultiHostnameRouteAppearsInEachBucket pins
// the duplication policy: a single HTTPRoute with three
// hostnames produces one route entry under each hostname
// bucket. A consumer scanning hosts shouldn't have to check
// "does this route also appear elsewhere".
func TestBuildSummary_MultiHostnameRouteAppearsInEachBucket(t *testing.T) {
	st := &State{
		Gateways: []Gateway{{
			Namespace: "y-cluster",
			Name:      "y-cluster",
			Listeners: []Listener{{Name: "https", Port: 443, Protocol: "HTTPS"}},
		}},
		HTTPRoutes: []HTTPRoute{{
			Namespace:  "keycloak-v3",
			Name:       "keycloak-admin",
			ParentRefs: rawJSON(t, `[{"name":"y-cluster","namespace":"y-cluster"}]`),
			Hostnames:  []string{"keycloak-admin", "keycloak-admin.example.com"},
			Rules: rawJSON(t, `[{
				"backendRefs": [{"name": "keycloak", "port": 8080}]
			}]`),
		}},
	}

	hosts := BuildSummary(st).Listeners[0].Hosts
	if len(hosts) != 2 {
		t.Fatalf("expected 2 host buckets, got %d: %+v", len(hosts), hosts)
	}
	for _, h := range hosts {
		if len(h.Routes) != 1 {
			t.Errorf("host %q routes: %+v", h.Hostname, h.Routes)
		}
	}
}

// TestBuildSummary_NumTrustedHopsGatewayScope pins the
// gateway-wide CTP application: a policy with a Gateway
// targetRef (no sectionName) propagates numTrustedHops to
// every listener on that Gateway.
func TestBuildSummary_NumTrustedHopsGatewayScope(t *testing.T) {
	st := &State{
		Gateways: []Gateway{{
			Namespace: "y-cluster",
			Name:      "y-cluster",
			Listeners: []Listener{
				{Name: "http", Port: 80, Protocol: "HTTP"},
				{Name: "https", Port: 443, Protocol: "HTTPS"},
			},
		}},
		ClientTrafficPolicies: []ClientTrafficPolicy{{
			Namespace:  "y-cluster",
			Name:       "trust-lb-xff",
			TargetRefs: rawJSON(t, `[{"kind":"Gateway","name":"y-cluster","namespace":"y-cluster"}]`),
			Spec:       rawJSON(t, `{"clientIPDetection":{"xForwardedFor":{"numTrustedHops":1}}}`),
		}},
	}

	for _, l := range BuildSummary(st).Listeners {
		if l.NumTrustedHops == nil || *l.NumTrustedHops != 1 {
			t.Errorf("listener %q numTrustedHops: %v", l.Name, l.NumTrustedHops)
		}
	}
}

// TestBuildSummary_NumTrustedHopsListenerScope pins the
// listener-scoped CTP: a sectionName on the targetRef narrows
// application to that listener only; the other listener must
// stay nil.
func TestBuildSummary_NumTrustedHopsListenerScope(t *testing.T) {
	st := &State{
		Gateways: []Gateway{{
			Namespace: "y-cluster",
			Name:      "y-cluster",
			Listeners: []Listener{
				{Name: "http", Port: 80, Protocol: "HTTP"},
				{Name: "https", Port: 443, Protocol: "HTTPS"},
			},
		}},
		ClientTrafficPolicies: []ClientTrafficPolicy{{
			Namespace:  "y-cluster",
			Name:       "https-only",
			TargetRefs: rawJSON(t, `[{"kind":"Gateway","name":"y-cluster","namespace":"y-cluster","sectionName":"https"}]`),
			Spec:       rawJSON(t, `{"clientIPDetection":{"xForwardedFor":{"numTrustedHops":2}}}`),
		}},
	}

	listenersByName := map[string]SummaryListener{}
	for _, l := range BuildSummary(st).Listeners {
		listenersByName[l.Name] = l
	}
	if listenersByName["http"].NumTrustedHops != nil {
		t.Errorf("http listener should have nil numTrustedHops: %v", listenersByName["http"].NumTrustedHops)
	}
	if h := listenersByName["https"].NumTrustedHops; h == nil || *h != 2 {
		t.Errorf("https listener numTrustedHops: %v", h)
	}
}

// TestBuildSummary_TrustedCIDRs pins that the alternate XFF
// trust knob (trustedCIDRs) is surfaced on the listener
// alongside numTrustedHops. envoy-gateway treats the two as
// alternative tuning paths for the same problem -- a snapshot
// may carry one, the other, or both -- so the projection
// must surface whichever the policy declares.
func TestBuildSummary_TrustedCIDRs(t *testing.T) {
	st := &State{
		Gateways: []Gateway{{
			Namespace: "y-cluster",
			Name:      "y-cluster",
			Listeners: []Listener{{Name: "https", Port: 443, Protocol: "HTTPS"}},
		}},
		ClientTrafficPolicies: []ClientTrafficPolicy{{
			Namespace:  "y-cluster",
			Name:       "trust-cidrs",
			TargetRefs: rawJSON(t, `[{"kind":"Gateway","name":"y-cluster","namespace":"y-cluster"}]`),
			Spec: rawJSON(t, `{"clientIPDetection":{"xForwardedFor":{
				"trustedCIDRs":["10.0.0.0/8","100.64.0.0/10"]
			}}}`),
		}},
	}

	l := BuildSummary(st).Listeners[0]
	if l.NumTrustedHops != nil {
		t.Errorf("numTrustedHops should be nil with trustedCIDRs-only CTP, got %d", *l.NumTrustedHops)
	}
	want := []string{"10.0.0.0/8", "100.64.0.0/10"}
	if len(l.TrustedCIDRs) != len(want) {
		t.Fatalf("trustedCIDRs len: got %v want %v", l.TrustedCIDRs, want)
	}
	for i, c := range want {
		if l.TrustedCIDRs[i] != c {
			t.Errorf("trustedCIDRs[%d]=%q want %q", i, l.TrustedCIDRs[i], c)
		}
	}
}

// TestBuildSummary_TrustedCIDRsAndNumTrustedHops pins the
// "both knobs set" combination. Older envoy-gateway versions
// allow it; newer ones reject the policy at admission. We
// surface whatever the snapshot saw -- the consumer can then
// notice the policy's Accepted=False status if it matters.
func TestBuildSummary_TrustedCIDRsAndNumTrustedHops(t *testing.T) {
	st := &State{
		Gateways: []Gateway{{
			Namespace: "y-cluster",
			Name:      "y-cluster",
			Listeners: []Listener{{Name: "https", Port: 443, Protocol: "HTTPS"}},
		}},
		ClientTrafficPolicies: []ClientTrafficPolicy{{
			Namespace:  "y-cluster",
			Name:       "trust-both",
			TargetRefs: rawJSON(t, `[{"kind":"Gateway","name":"y-cluster","namespace":"y-cluster"}]`),
			Spec: rawJSON(t, `{"clientIPDetection":{"xForwardedFor":{
				"numTrustedHops": 2,
				"trustedCIDRs":["10.0.0.0/8"]
			}}}`),
		}},
	}

	l := BuildSummary(st).Listeners[0]
	if l.NumTrustedHops == nil || *l.NumTrustedHops != 2 {
		t.Errorf("numTrustedHops: %v", l.NumTrustedHops)
	}
	if len(l.TrustedCIDRs) != 1 || l.TrustedCIDRs[0] != "10.0.0.0/8" {
		t.Errorf("trustedCIDRs: %v", l.TrustedCIDRs)
	}
}

// TestBuildSummary_GRPCRouteMethodMatch pins the GRPC match
// rendering: a (service, method) clause shows up as
// "Method=Exact:<service>/<method>" on the SummaryMatch.Path
// field, sharing the same projection field as HTTP path
// matches.
func TestBuildSummary_GRPCRouteMethodMatch(t *testing.T) {
	st := &State{
		Gateways: []Gateway{{
			Namespace: "y-cluster",
			Name:      "y-cluster",
			Listeners: []Listener{{Name: "https", Port: 443, Protocol: "HTTPS"}},
		}},
		GRPCRoutes: []GRPCRoute{{
			Namespace:  "live-v3",
			Name:       "live-grpc",
			ParentRefs: rawJSON(t, `[{"name":"y-cluster","namespace":"y-cluster"}]`),
			Hostnames:  []string{"live.example.com"},
			Rules: rawJSON(t, `[{
				"matches": [{"method": {"type": "Exact", "service": "live.v1.LiveService", "method": "Subscribe"}}],
				"backendRefs": [{"name": "live", "namespace": "live-v3", "port": 9090}]
			}]`),
		}},
	}

	rs := BuildSummary(st).Listeners[0].Hosts[0].Routes
	if len(rs) != 1 || len(rs[0].Matches) != 1 {
		t.Fatalf("routes: %+v", rs)
	}
	got := rs[0].Matches[0].Path
	want := "Method=Exact:live.v1.LiveService/Subscribe"
	if got != want {
		t.Errorf("grpc match path=%q want %q", got, want)
	}
	if rs[0].Source != "GRPCRoute/live-v3/live-grpc#0" {
		t.Errorf("grpc source=%q", rs[0].Source)
	}
}

// TestBuildSummary_NonGatewayParentRefIgnored pins the
// scoping: a route whose parentRef points at a different
// Gateway (or a non-Gateway kind) must not show up under any
// listener of OUR Gateway. Cross-namespace routing on the
// same controller is real; we don't want adjacent Gateways'
// routes leaking into our summary.
func TestBuildSummary_NonGatewayParentRefIgnored(t *testing.T) {
	st := &State{
		Gateways: []Gateway{{
			Namespace: "y-cluster",
			Name:      "y-cluster",
			Listeners: []Listener{{Name: "https", Port: 443, Protocol: "HTTPS"}},
		}},
		HTTPRoutes: []HTTPRoute{
			{
				Namespace:  "other",
				Name:       "other-route",
				ParentRefs: rawJSON(t, `[{"name":"other-gateway","namespace":"other"}]`),
				Hostnames:  []string{"other.example.com"},
				Rules:      rawJSON(t, `[{"backendRefs": [{"name": "echo"}]}]`),
			},
		},
	}

	hosts := BuildSummary(st).Listeners[0].Hosts
	if len(hosts) != 0 {
		t.Errorf("expected no hosts (route on different gateway), got %+v", hosts)
	}
}

// TestBuildSummary_MarshalsWithEmptyEnvoy is the user's stated
// shape contract: tests build a State + Summary and an empty
// Envoy object, then JSON-marshal. The result must surface
// "summary" and "envoy" at the top level alongside the
// existing kind slices, all parsable.
func TestBuildSummary_MarshalsWithEmptyEnvoy(t *testing.T) {
	st := &State{
		Gateways: []Gateway{{
			Namespace: "y-cluster",
			Name:      "y-cluster",
			Listeners: []Listener{{Name: "https", Port: 443, Protocol: "HTTPS"}},
		}},
		Envoy:         &Envoy{}, // empty envoy as per the test contract
		FetchedAt:     "2026-05-06T00:00:00Z",
		SchemaID:      SchemaID,
		SchemaVersion: SchemaVersion,
	}
	st.Summary = BuildSummary(st)

	out, err := json.Marshal(st)
	if err != nil {
		t.Fatal(err)
	}
	var back map[string]any
	if err := json.Unmarshal(out, &back); err != nil {
		t.Fatalf("re-parse: %v", err)
	}
	if _, ok := back["summary"]; !ok {
		t.Errorf("summary missing from marshalled JSON: %s", out)
	}
	if _, ok := back["envoy"]; !ok {
		t.Errorf("envoy missing from marshalled JSON: %s", out)
	}
}

// rawJSON is a test helper that wraps a literal JSON string in
// json.RawMessage with a syntax check to fail loudly on a typo
// in fixture text.
func rawJSON(t *testing.T, s string) json.RawMessage {
	t.Helper()
	var probe any
	if err := json.Unmarshal([]byte(s), &probe); err != nil {
		t.Fatalf("invalid fixture JSON %q: %v", s, err)
	}
	return json.RawMessage(s)
}
