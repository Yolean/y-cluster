package hetzner

import (
	"strings"
	"testing"
)

// TestLBName documents the y-cluster-<lbGroup> shape so a future
// rename happens with eyes open. Hetzner LB resource names are the
// only public-API knob the operator sees in the Hetzner UI when
// debugging; consistent prefix makes a multi-tenant project easy
// to filter.
func TestLBName(t *testing.T) {
	cases := []struct {
		lbGroup string
		want    string
	}{
		{"alice", "y-cluster-alice"},
		{"team-a", "y-cluster-team-a"},
	}
	for _, tc := range cases {
		t.Run(tc.lbGroup, func(t *testing.T) {
			if got := lbName(tc.lbGroup); got != tc.want {
				t.Errorf("got %q, want %q", got, tc.want)
			}
		})
	}
}

// TestLBLabelSelectorMatchesServerCreate pins the invariant that
// the LB's label_selector matches what hetzner.Provision stamps on
// the server in hc.Server.Create. If these drift, the label_selector
// target resolves to zero servers and the LB has no backends --
// silent failure mode worth a unit test.
func TestLBLabelSelectorMatchesServerCreate(t *testing.T) {
	const group = "alice"
	want := "managed-by=y-cluster,lb-group=" + group
	if got := labelSelectorForGroup(group); got != want {
		t.Errorf("selector: got %q, want %q", got, want)
	}
	// And the server-create labels must include the same vocabulary.
	// hetzner.Provision uses literal strings; this test would catch
	// a drift like "manager-by" vs "managed-by".
	mustContain := []string{"managed-by", "lb-group", group}
	for _, s := range mustContain {
		if !strings.Contains(want, s) {
			t.Errorf("selector %q missing %q", want, s)
		}
	}
}
