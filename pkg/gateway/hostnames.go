package gateway

import "sort"

// Hostnames returns the deduped, sorted hostname list reachable
// through the cluster's gateways, derived from the typed Summary
// projection on State. The set is exactly:
//
//	{ h.Hostname for l in state.Summary.Listeners
//	             for h in l.Hosts
//	             if h.Hostname not in {"", "*"} }
//
// Used by the `y-cluster gateway hostnames` subcommand and by
// downstream consumers (e.g. an external LoadBalancer setup that
// needs the SAN list for its TLS cert).
//
// "*" is the catch-all bucket (route declared no `.spec.hostnames`)
// and is dropped: a wildcard-SAN cert is a different concern and
// out of scope here. Routes that explicitly declare a wildcard
// hostname like "*.example.com" pass through verbatim --
// consumers that can't handle them should filter further.
//
// Reading from Summary (rather than re-walking HTTPRoutes /
// GRPCRoutes here) keeps the parent-ref / listener-attachment
// filtering in one place. Summary is built deterministically
// from the same State, so this function is a pure projection.
func Hostnames(s *State) []string {
	out := []string{}
	if s == nil || s.Summary == nil {
		return out
	}
	seen := map[string]struct{}{}
	for _, l := range s.Summary.Listeners {
		for _, h := range l.Hosts {
			if h.Hostname == "" || h.Hostname == "*" {
				continue
			}
			if _, ok := seen[h.Hostname]; ok {
				continue
			}
			seen[h.Hostname] = struct{}{}
			out = append(out, h.Hostname)
		}
	}
	sort.Strings(out)
	return out
}
