package k8swait

import (
	"errors"
	"strings"
	"testing"
)

func TestSplitResource(t *testing.T) {
	cases := []struct {
		in            string
		wantKind, wantName string
		wantErr       bool
	}{
		{"deployment/my-app", "deployment", "my-app", false},
		{"namespace/dev", "namespace", "dev", false},
		{"httproute.gateway.networking.k8s.io/foo", "httproute.gateway.networking.k8s.io", "foo", false},
		{"deployment/", "", "", true},
		{"/name", "", "", true},
		{"justakind", "", "", true},
	}
	for _, c := range cases {
		k, n, err := splitResource(c.in)
		if (err != nil) != c.wantErr {
			t.Errorf("splitResource(%q) err=%v wantErr=%v", c.in, err, c.wantErr)
			continue
		}
		if !c.wantErr && (k != c.wantKind || n != c.wantName) {
			t.Errorf("splitResource(%q) = %q/%q want %q/%q", c.in, k, n, c.wantKind, c.wantName)
		}
	}
}

func TestCompileForSpec_Condition(t *testing.T) {
	p, err := compileForSpec("condition=Ready")
	if err != nil {
		t.Fatal(err)
	}
	if p.conditionT != "Ready" || p.conditionV != "True" {
		t.Fatalf("got T=%q V=%q", p.conditionT, p.conditionV)
	}
}

func TestCompileForSpec_ConditionExplicitStatus(t *testing.T) {
	p, err := compileForSpec("condition=Available=False")
	if err != nil {
		t.Fatal(err)
	}
	if p.conditionT != "Available" || p.conditionV != "False" {
		t.Fatalf("got T=%q V=%q", p.conditionT, p.conditionV)
	}
}

func TestCompileForSpec_Delete(t *testing.T) {
	p, err := compileForSpec("delete")
	if err != nil {
		t.Fatal(err)
	}
	if !p.OnDelete {
		t.Fatal("OnDelete should be true")
	}
}

func TestCompileForSpec_JSONPath(t *testing.T) {
	p, err := compileForSpec(`jsonpath={.status.phase}=Active`)
	if err != nil {
		t.Fatal(err)
	}
	if p.expected != "Active" {
		t.Fatalf("expected=%q", p.expected)
	}
	if p.jsonPathExp == nil {
		t.Fatal("jsonPathExp not set")
	}
}

func TestCompileForSpec_Unsupported(t *testing.T) {
	_, err := compileForSpec("create")
	if !errors.Is(err, ErrUnsupportedFor) {
		t.Fatalf("got %v, want ErrUnsupportedFor", err)
	}
}

func TestCompileForSpec_JSONPathMalformed(t *testing.T) {
	_, err := compileForSpec(`jsonpath=bare-no-braces=foo`)
	if !errors.Is(err, ErrUnsupportedFor) {
		t.Fatalf("got %v, want ErrUnsupportedFor", err)
	}
}

// TestPredicate_ConditionMatch builds a fake unstructured object
// (just maps) and runs predicate.eval to verify the matching
// logic without needing a real apiserver.
func TestPredicate_ConditionMatch(t *testing.T) {
	obj := map[string]any{
		"status": map[string]any{
			"conditions": []any{
				map[string]any{"type": "Available", "status": "True"},
				map[string]any{"type": "Progressing", "status": "True"},
			},
		},
	}
	for _, c := range []struct {
		spec string
		want bool
	}{
		{"condition=Available", true},
		{"condition=Progressing=True", true},
		{"condition=Progressing=False", false},
		{"condition=NoSuchType", false},
	} {
		p, err := compileForSpec(c.spec)
		if err != nil {
			t.Fatal(err)
		}
		got, err := p.eval(obj)
		if err != nil {
			t.Fatal(err)
		}
		if got != c.want {
			t.Errorf("eval(%q) = %v want %v", c.spec, got, c.want)
		}
	}
}

func TestPredicate_JSONPathMatch(t *testing.T) {
	obj := map[string]any{
		"status": map[string]any{
			"phase": "Active",
		},
	}
	p, err := compileForSpec(`jsonpath={.status.phase}=Active`)
	if err != nil {
		t.Fatal(err)
	}
	got, err := p.eval(obj)
	if err != nil {
		t.Fatal(err)
	}
	if !got {
		t.Fatal("expected match")
	}

	// Mismatch should return false.
	obj["status"].(map[string]any)["phase"] = "Terminating"
	got, _ = p.eval(obj)
	if got {
		t.Fatal("should not match")
	}
}

func TestRolloutChecker_Unsupported(t *testing.T) {
	_, err := rolloutChecker(nil, "configmap")
	if !errors.Is(err, ErrUnsupportedKind) {
		t.Fatalf("want ErrUnsupportedKind, got %v", err)
	}
}

// TestErrorMessages_Verbatim guards the error wording so callers
// who switch on substrings (logs, metrics) don't quietly drift.
func TestErrorMessages_Verbatim(t *testing.T) {
	if !strings.Contains(ErrTimeout.Error(), "timed out") {
		t.Fatalf("ErrTimeout message: %q", ErrTimeout.Error())
	}
	if !strings.Contains(ErrUnsupportedFor.Error(), "unsupported") {
		t.Fatalf("ErrUnsupportedFor message: %q", ErrUnsupportedFor.Error())
	}
}
