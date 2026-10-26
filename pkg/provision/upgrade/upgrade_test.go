package upgrade

import (
	"errors"
	"strings"
	"testing"
)

func TestParse(t *testing.T) {
	for tag, want := range map[string]Version{
		"v1.9.1":   {1, 9, 1},
		"v0.0.0":   {0, 0, 0},
		"v1.10.0":  {1, 10, 0},
		"v12.3.45": {12, 3, 45},
	} {
		got, err := Parse(tag)
		if err != nil || got != want {
			t.Errorf("Parse(%q) = %v, %v; want %v", tag, got, err, want)
		}
		if got.String() != tag {
			t.Errorf("Parse(%q).String() = %q", tag, got.String())
		}
	}
	for _, tag := range []string{
		"",
		"1.9.1",
		"v1.9",
		"v1.9.1.2",
		"v1.9.1-rc.1",
		"v1.9.0+build",
		"v01.9.1",
		"v1.09.1",
		"v1.9.01",
		"V1.9.1",
		" v1.9.1",
		"v1.9.1 ",
		"v1.9.x",
		"latest",
		"v99999999999999999999.0.0",
	} {
		if v, err := Parse(tag); err == nil {
			t.Errorf("Parse(%q) = %v, want an error", tag, v)
		}
	}
}

// One case per row of the rule, and the parse errors.
func TestCheck(t *testing.T) {
	for _, c := range []struct {
		installed, configured string
		allowMinor            bool
		want                  Step
		refused               string
	}{
		{installed: "", configured: "v1.9.1", want: Install},
		{installed: "", configured: "v1.9.1", allowMinor: true, want: Install},
		{installed: "v1.9.1", configured: "v1.9.1", want: Same},
		{installed: "v1.9.1", configured: "v1.9.2", want: Patch},
		{installed: "v1.9.1", configured: "v1.9.12", want: Patch},
		{installed: "v1.9.1", configured: "v1.9.2", allowMinor: true, want: Patch},
		{installed: "v1.8.5", configured: "v1.9.1", refused: "a minor upgrade is refused unless it is allowed"},
		{installed: "v1.8.5", configured: "v1.9.1", allowMinor: true, want: Minor},
		{installed: "v1.8.5", configured: "v1.10.0", allowMinor: true, want: Minor},
		{installed: "v1.9.2", configured: "v1.9.1", refused: "a downgrade is refused"},
		{installed: "v1.9.2", configured: "v1.9.1", allowMinor: true, refused: "a downgrade is refused"},
		{installed: "v1.9.1", configured: "v1.8.5", allowMinor: true, refused: "a downgrade is refused"},
		{installed: "v1.9.1", configured: "v1.8.9", refused: "a downgrade is refused"},
		{installed: "v1.9.1", configured: "v2.0.0", allowMinor: true, refused: "a change of major version is refused"},
		{installed: "v2.0.0", configured: "v1.9.1", allowMinor: true, refused: "a change of major version is refused"},
		// A tag that is not vMAJOR.MINOR.PATCH is refused unless it is
		// the installed one.
		{installed: "v1.9.0-rc.1", configured: "v1.9.0-rc.1", want: Same},
		{installed: "", configured: "v1.9.0-rc.1", refused: `"v1.9.0-rc.1" is not a release tag`},
		{installed: "v1.9.0-rc.1", configured: "v1.9.0", refused: `"v1.9.0-rc.1" is not a release tag`},
		{installed: "v1.9.0", configured: "v1.9.1-rc.1", refused: `"v1.9.1-rc.1" is not a release tag`},
		{installed: "latest", configured: "v1.9.1", allowMinor: true, refused: `"latest" is not a release tag`},
		{installed: "v1.9.1", configured: "v1.9", refused: `"v1.9" is not a release tag`},
	} {
		got, err := Check(c.installed, c.configured, c.allowMinor)
		name := c.installed + " -> " + c.configured
		if c.allowMinor {
			name += " (minor allowed)"
		}
		if c.refused == "" {
			if err != nil || got != c.want {
				t.Errorf("%s: %v, %v; want %v", name, got, err, c.want)
			}
			continue
		}
		var refused *RefusedError
		if !errors.As(err, &refused) {
			t.Errorf("%s: %v, %v; want a refusal", name, got, err)
			continue
		}
		if !strings.Contains(err.Error(), c.refused) {
			t.Errorf("%s: %q, want it to say %q", name, err, c.refused)
		}
		// A refusal names both versions.
		if !strings.Contains(err.Error(), c.configured) || !strings.Contains(err.Error(), c.installed) {
			t.Errorf("%s: %q does not name both versions", name, err)
		}
		if refused.MinorNotAllowed != strings.Contains(c.refused, "minor upgrade") {
			t.Errorf("%s: MinorNotAllowed %v", name, refused.MinorNotAllowed)
		}
	}
}

func TestRefusedError_NothingInstalled(t *testing.T) {
	_, err := Check("", "v1.9.1-rc.1", false)
	if err == nil || strings.Contains(err.Error(), "is installed") {
		t.Errorf("%v", err)
	}
}
