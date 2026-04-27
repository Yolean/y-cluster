package envsubst

import (
	"strings"
	"testing"
)

func mapLookup(m map[string]string) LookupFunc {
	return func(name string) (string, bool) {
		v, ok := m[name]
		return v, ok
	}
}

func TestExpand_Basics(t *testing.T) {
	env := mapLookup(map[string]string{
		"FOO":   "value",
		"EMPTY": "",
	})

	cases := []struct {
		in, want string
	}{
		{"plain", "plain"},
		{"${FOO}", "value"},
		{"prefix-${FOO}-suffix", "prefix-value-suffix"},
		{"${FOO}${FOO}", "valuevalue"},
		{"${EMPTY}", ""},
		{"${MISSING:-fallback}", "fallback"},
		{"${MISSING:-}", ""},
		{"${FOO:-overridden}", "value"},   // present beats default
		{"${EMPTY:-fallback}", ""},        // empty-but-set beats default; matches POSIX :- semantics for set
		{"$$ literal $$", "$ literal $"},  // $$ escape
		{"$$ {NOT_A_VAR}", "$ {NOT_A_VAR}"},
	}
	for _, tc := range cases {
		got, err := Expand(tc.in, env)
		if err != nil {
			t.Errorf("Expand(%q): unexpected error: %v", tc.in, err)
			continue
		}
		if got != tc.want {
			t.Errorf("Expand(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

// POSIX shell distinguishes ${VAR:-} (use default if unset OR
// empty) from ${VAR-} (use default only if unset). We currently
// only implement :- but use the "unset only" semantics, so
// document that here as the contract: empty-but-set beats default.
func TestExpand_EmptyButSetDoesNotTriggerDefault(t *testing.T) {
	got, err := Expand("${X:-fallback}", mapLookup(map[string]string{"X": ""}))
	if err != nil {
		t.Fatal(err)
	}
	if got != "" {
		t.Fatalf("got %q, want empty (set-but-empty should not use default)", got)
	}
}

func TestExpand_UndefinedErrors(t *testing.T) {
	_, err := Expand("hello ${MISSING}", mapLookup(nil))
	if err == nil || !strings.Contains(err.Error(), "MISSING") {
		t.Fatalf("want undefined error naming MISSING, got %v", err)
	}
}

func TestExpand_NoSubstitutionFastPath(t *testing.T) {
	// Strings without a $ shouldn't even consult the lookup.
	called := false
	got, err := Expand("nothing here", func(string) (string, bool) {
		called = true
		return "", false
	})
	if err != nil {
		t.Fatal(err)
	}
	if got != "nothing here" {
		t.Fatalf("got %q", got)
	}
	if called {
		t.Fatal("lookup invoked for $-free string")
	}
}

// --- Apply: struct walking ---

type leaf struct {
	Plain string `yaml:"plain"`
	Token string `yaml:"token" envsubst:"true"`
}

type wrap struct {
	Name string `yaml:"name"`
	Leaf leaf   `yaml:"leaf"`
}

func TestApply_TaggedFieldExpands(t *testing.T) {
	w := wrap{Name: "static", Leaf: leaf{Plain: "literal", Token: "${TOK}"}}
	if err := Apply(&w, mapLookup(map[string]string{"TOK": "abc"})); err != nil {
		t.Fatal(err)
	}
	if w.Leaf.Token != "abc" {
		t.Fatalf("token: %q", w.Leaf.Token)
	}
	if w.Name != "static" || w.Leaf.Plain != "literal" {
		t.Fatalf("untagged fields mutated: %+v", w)
	}
}

func TestApply_UntaggedRejectsRef(t *testing.T) {
	w := wrap{Name: "static-${WHO}", Leaf: leaf{}}
	err := Apply(&w, mapLookup(map[string]string{"WHO": "x"}))
	if err == nil || !strings.Contains(err.Error(), "name") {
		t.Fatalf("want policy error naming `name`, got %v", err)
	}
	if !strings.Contains(err.Error(), "envsubst") {
		t.Fatalf("error should hint at the tag fix: %v", err)
	}
}

func TestApply_UntaggedPlainStringIsFine(t *testing.T) {
	w := wrap{Name: "ystack", Leaf: leaf{Plain: "no dollars here"}}
	if err := Apply(&w, mapLookup(nil)); err != nil {
		t.Fatalf("plain strings without $ should not error: %v", err)
	}
}

// Sequences and maps: tagged at field level, applied to elements.

type registries struct {
	Mirrors map[string]mirror `yaml:"mirrors"`
	Configs map[string]auth   `yaml:"configs"`
}

type mirror struct {
	Endpoint []string `yaml:"endpoint" envsubst:"true"`
}

type auth struct {
	Username string `yaml:"username" envsubst:"true"`
	Password string `yaml:"password" envsubst:"true"`
}

func TestApply_SliceElements(t *testing.T) {
	r := registries{Mirrors: map[string]mirror{
		"prod-registry.svc.local": {Endpoint: []string{"http://${MIRROR_IP}", "http://fallback"}},
	}}
	if err := Apply(&r, mapLookup(map[string]string{"MIRROR_IP": "10.43.0.50"})); err != nil {
		t.Fatal(err)
	}
	got := r.Mirrors["prod-registry.svc.local"].Endpoint
	if got[0] != "http://10.43.0.50" || got[1] != "http://fallback" {
		t.Fatalf("endpoints: %v", got)
	}
}

func TestApply_MapValues(t *testing.T) {
	r := registries{Configs: map[string]auth{
		"europe-docker.pkg.dev": {Username: "oauth2accesstoken", Password: "${GCP_TOKEN}"},
	}}
	if err := Apply(&r, mapLookup(map[string]string{"GCP_TOKEN": "ya29.secret"})); err != nil {
		t.Fatal(err)
	}
	if r.Configs["europe-docker.pkg.dev"].Password != "ya29.secret" {
		t.Fatalf("password: %q", r.Configs["europe-docker.pkg.dev"].Password)
	}
}

// Map keys are never substituted, even when the value type's
// fields are tagged. This is the forward-compat guard against
// dynamic keys.
type keyMap struct {
	M map[string]auth `yaml:"m"`
}

func TestApply_MapKeyRejectsRef(t *testing.T) {
	k := keyMap{M: map[string]auth{
		"${REGISTRY_HOST}": {Password: "${TOK}"},
	}}
	err := Apply(&k, mapLookup(map[string]string{"REGISTRY_HOST": "x", "TOK": "y"}))
	if err == nil || !strings.Contains(err.Error(), "key") {
		t.Fatalf("want key-rejected error, got %v", err)
	}
}

func TestApply_NilTarget(t *testing.T) {
	if err := Apply(nil, nil); err == nil {
		t.Fatal("want error for nil target")
	}
}

func TestApply_NonPointerTarget(t *testing.T) {
	if err := Apply(leaf{}, nil); err == nil {
		t.Fatal("want error for non-pointer target")
	}
}

// PathInError ensures users get a YAML-shaped path, not Go field
// names, so the message lines up with what they see in the file.
type pathFix struct {
	Outer struct {
		Inner string `yaml:"inner"`
	} `yaml:"outer"`
}

func TestApply_PathReportsYAMLNames(t *testing.T) {
	var p pathFix
	p.Outer.Inner = "${BOOM}"
	err := Apply(&p, mapLookup(nil))
	if err == nil {
		t.Fatal("want error")
	}
	if !strings.Contains(err.Error(), "outer.inner") {
		t.Fatalf("error path should include yaml name `outer.inner`, got %v", err)
	}
}

// Undefined-var inside a tagged field surfaces with the path so
// the operator knows which leaf failed.
func TestApply_UndefinedInTaggedSurfacesPath(t *testing.T) {
	w := wrap{Leaf: leaf{Token: "${REQUIRED_BUT_UNSET}"}}
	err := Apply(&w, mapLookup(nil))
	if err == nil || !strings.Contains(err.Error(), "REQUIRED_BUT_UNSET") {
		t.Fatalf("want undefined error naming the var, got %v", err)
	}
	if !strings.Contains(err.Error(), "leaf.token") {
		t.Fatalf("error should name the path leaf.token, got %v", err)
	}
}
