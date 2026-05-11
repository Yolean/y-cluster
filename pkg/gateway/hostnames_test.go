package gateway

import (
	"reflect"
	"testing"
)

// TestHostnames_Nil documents the nil-safety contract: callers
// can pass nil State (or a State with nil Summary) without
// panicking and get a non-nil empty slice back. JSON consumers
// can index without checks; bash consumers see no output.
func TestHostnames_Nil(t *testing.T) {
	if got := Hostnames(nil); got == nil {
		t.Errorf("nil input should yield empty slice, not nil")
	} else if len(got) != 0 {
		t.Errorf("nil input should yield empty slice, got %v", got)
	}
	if got := Hostnames(&State{}); got == nil || len(got) != 0 {
		t.Errorf("State with nil Summary should yield empty slice, got %v", got)
	}
}

// TestHostnames_Sorted pins the ordering contract: bash
// consumers piping through `sort -u | diff` would notice
// non-determinism otherwise.
func TestHostnames_Sorted(t *testing.T) {
	s := &State{Summary: &Summary{Listeners: []SummaryListener{{
		Hosts: []SummaryHost{
			{Hostname: "zeta.example.com"},
			{Hostname: "alpha.example.com"},
			{Hostname: "mid.example.com"},
		},
	}}}}
	got := Hostnames(s)
	want := []string{"alpha.example.com", "mid.example.com", "zeta.example.com"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got %v, want %v", got, want)
	}
}

// TestHostnames_DropsWildcardBucket pins that the "*" host
// (Summary's bucket for routes declaring no hostname) is not a
// hostname suitable for a cert SAN -- it's the in-snapshot
// sentinel for catch-all routes. Cert generators should not see
// it.
func TestHostnames_DropsWildcardBucket(t *testing.T) {
	s := &State{Summary: &Summary{Listeners: []SummaryListener{{
		Hosts: []SummaryHost{
			{Hostname: "real.example.com"},
			{Hostname: "*"},
			{Hostname: ""},
		},
	}}}}
	got := Hostnames(s)
	if len(got) != 1 || got[0] != "real.example.com" {
		t.Errorf("got %v, want [real.example.com]", got)
	}
}

// TestHostnames_DedupAcrossListeners pins that a hostname
// appearing on multiple listeners (e.g. the same HTTPRoute
// attached to both http and https) appears once in the output.
// A duplicated SAN entry isn't strictly invalid in a cert but
// makes downstream review noisy.
func TestHostnames_DedupAcrossListeners(t *testing.T) {
	s := &State{Summary: &Summary{Listeners: []SummaryListener{
		{
			Name: "http",
			Hosts: []SummaryHost{
				{Hostname: "site.example.com"},
			},
		},
		{
			Name: "https",
			Hosts: []SummaryHost{
				{Hostname: "site.example.com"},
				{Hostname: "extra.example.com"},
			},
		},
	}}}
	got := Hostnames(s)
	want := []string{"extra.example.com", "site.example.com"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got %v, want %v", got, want)
	}
}

// TestHostnames_FromHTTPRouteRoundTrip end-to-end: build a State
// with raw HTTPRoute payloads, call BuildSummary (which routes
// through to Summary.Listeners[].Hosts[]), then Hostnames. This
// guards against a regression where Summary's hostname-bucket
// shape changes; consumers using Hostnames don't have to track
// the intermediate.
func TestHostnames_FromHTTPRouteRoundTrip(t *testing.T) {
	st := &State{
		Gateways: []Gateway{{
			Namespace: "y-cluster",
			Name:      "y-cluster",
			Listeners: []Listener{{Name: "https", Port: 443, Protocol: "HTTPS"}},
		}},
		HTTPRoutes: []HTTPRoute{{
			Namespace:  "myapp",
			Name:       "keycloak-admin",
			ParentRefs: rawJSON(t, `[{"name":"y-cluster","namespace":"y-cluster"}]`),
			Hostnames:  []string{"keycloak-admin", "keycloak-admin.example.com"},
			Rules:      rawJSON(t, `[{"backendRefs":[{"name":"keycloak"}]}]`),
		}},
	}
	st.Summary = BuildSummary(st)

	got := Hostnames(st)
	want := []string{"keycloak-admin", "keycloak-admin.example.com"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got %v, want %v", got, want)
	}
}

// TestHostnames_FromGRPCRouteRoundTrip mirrors the HTTP case for
// gRPC: a GRPCRoute attached to the cluster Gateway contributes
// its hostname to the LB SAN list. (gRPC over TLS through the
// same LB is a real shape; even if rare today, including it
// here costs nothing.)
func TestHostnames_FromGRPCRouteRoundTrip(t *testing.T) {
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
			Rules:      rawJSON(t, `[{"backendRefs":[{"name":"live"}]}]`),
		}},
	}
	st.Summary = BuildSummary(st)

	got := Hostnames(st)
	if len(got) != 1 || got[0] != "live.example.com" {
		t.Errorf("got %v, want [live.example.com]", got)
	}
}
