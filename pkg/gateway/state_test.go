package gateway

import (
	"encoding/json"
	"strings"
	"testing"
)

// TestExtractTargetRefs covers the policy targetRef shape
// flattening: envoy-gateway's CRDs accept both `targetRefs`
// (plural array) and the legacy singular `targetRef` shape;
// our snapshot normalizes to a single `targetRefs` array so
// consumers don't have to branch.
func TestExtractTargetRefs(t *testing.T) {
	cases := []struct {
		name string
		spec string
		want string // JSON shape we want, "" for nil
	}{
		{
			name: "plural targetRefs passes through",
			spec: `{"targetRefs":[{"kind":"Gateway","name":"y-cluster"}]}`,
			want: `[{"kind":"Gateway","name":"y-cluster"}]`,
		},
		{
			name: "singular targetRef wrapped to single-element array",
			spec: `{"targetRef":{"kind":"Gateway","name":"y-cluster"}}`,
			want: `[{"kind":"Gateway","name":"y-cluster"}]`,
		},
		{
			name: "neither field present",
			spec: `{"clientIPDetection":{}}`,
			want: "",
		},
		{
			name: "empty spec",
			spec: ``,
			want: "",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := extractTargetRefs(json.RawMessage(tc.spec))
			if tc.want == "" {
				if got != nil {
					t.Errorf("expected nil, got %s", got)
				}
				return
			}
			if !jsonEqual(t, got, []byte(tc.want)) {
				t.Errorf("got %s, want %s", got, tc.want)
			}
		})
	}
}

// TestHasTrueCondition pins the case-insensitive Status compare
// (k8s convention is "True" but we don't want a stray "true"
// from a controller bug to silently flip Programmed to false).
func TestHasTrueCondition(t *testing.T) {
	conds := []rawCondition{
		{Type: "Accepted", Status: "True"},
		{Type: "Programmed", Status: "true"},
		{Type: "ResolvedRefs", Status: "False"},
	}
	if !hasTrueCondition(conds, "Accepted") {
		t.Error("Accepted=True should be true")
	}
	if !hasTrueCondition(conds, "Programmed") {
		t.Error("Programmed=true (lowercase) should be true (case-insensitive)")
	}
	if hasTrueCondition(conds, "ResolvedRefs") {
		t.Error("ResolvedRefs=False should be false")
	}
	if hasTrueCondition(conds, "Missing") {
		t.Error("missing condition should be false")
	}
}

// TestSchemaIDOnState locks the $schema serialization on a
// freshly-marshalled State. Consumers compare against the
// SchemaID constant; if these drift the schema doc would
// validate fine but the produced JSON wouldn't reference it.
func TestSchemaIDOnState(t *testing.T) {
	st := &State{SchemaID: SchemaID}
	out, err := json.Marshal(st)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(out), `"$schema":"`+SchemaID+`"`) {
		t.Errorf("$schema not surfaced in JSON output: %s", out)
	}
}

// TestStateZeroValueMarshals: the zero value must produce
// stable, parseable JSON with empty slices (not null) so
// consumers can index without nil-checking.
func TestStateZeroValueMarshals(t *testing.T) {
	st := &State{
		Gateways:               []Gateway{},
		HTTPRoutes:             []HTTPRoute{},
		GRPCRoutes:             []GRPCRoute{},
		ClientTrafficPolicies:  []ClientTrafficPolicy{},
		BackendTrafficPolicies: []BackendTrafficPolicy{},
		FetchedAt:              "2026-05-06T00:00:00Z",
		SchemaID:               SchemaID,
	}
	out, err := json.Marshal(st)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		`"gateways":[]`,
		`"httpRoutes":[]`,
		`"grpcRoutes":[]`,
		`"clientTrafficPolicies":[]`,
		`"backendTrafficPolicies":[]`,
		`"fetchedAt":"2026-05-06T00:00:00Z"`,
	} {
		if !strings.Contains(string(out), want) {
			t.Errorf("zero-value JSON missing %q\nfull: %s", want, out)
		}
	}
}

// jsonEqual compares two JSON byte slices for structural
// equality (ignoring whitespace + key order).
func jsonEqual(t *testing.T, a, b []byte) bool {
	t.Helper()
	var ax, bx any
	if err := json.Unmarshal(a, &ax); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(b, &bx); err != nil {
		t.Fatal(err)
	}
	ar, err := json.Marshal(ax)
	if err != nil {
		t.Fatal(err)
	}
	br, err := json.Marshal(bx)
	if err != nil {
		t.Fatal(err)
	}
	return string(ar) == string(br)
}
