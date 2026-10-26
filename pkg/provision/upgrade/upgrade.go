// Package upgrade is the rule for how far the release of an upstream
// component that y-cluster installs may move on a cluster that already
// runs one. Envoy Gateway uses it; cert-manager and Cilium have pins
// of the same kind and can.
//
// From the installed release to the configured one:
//
//   - nothing installed: install;
//   - the same tag: apply again, which changes nothing;
//   - a newer patch of the same minor: upgrade;
//   - a newer minor of the same major: upgrade only when the caller
//     allows it, because a minor may change CRDs and defaults, and
//     nothing removes what the older release had that the newer one
//     does not ship;
//   - an older patch, an older minor or another major: refused.
//
// Tags are read strictly as vMAJOR.MINOR.PATCH. Any other tag, a
// pre-release included, is accepted only when it is the installed tag
// itself.
package upgrade

import (
	"fmt"
	"regexp"
	"strconv"
)

// Version is a release tag vMAJOR.MINOR.PATCH.
type Version struct {
	Major, Minor, Patch int
}

var tagPattern = regexp.MustCompile(`^v(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)$`)

// Parse reads a release tag: a leading v and three numbers without
// leading zeros, with nothing before or after them.
func Parse(tag string) (Version, error) {
	m := tagPattern.FindStringSubmatch(tag)
	if m == nil {
		return Version{}, fmt.Errorf("%q is not a release tag vMAJOR.MINOR.PATCH", tag)
	}
	var v Version
	for i, n := range []*int{&v.Major, &v.Minor, &v.Patch} {
		var err error
		if *n, err = strconv.Atoi(m[i+1]); err != nil {
			return Version{}, fmt.Errorf("%q: %w", tag, err)
		}
	}
	return v, nil
}

func (v Version) String() string {
	return fmt.Sprintf("v%d.%d.%d", v.Major, v.Minor, v.Patch)
}

// Step is what the move from the installed release to the configured
// one is.
type Step int

const (
	// Install: no release is installed.
	Install Step = iota
	// Same: the configured release is the installed one.
	Same
	// Patch: a newer patch of the installed minor.
	Patch
	// Minor: a newer minor of the installed major, which the caller
	// allowed.
	Minor
)

func (s Step) String() string {
	switch s {
	case Install:
		return "install"
	case Same:
		return "same"
	case Patch:
		return "patch upgrade"
	case Minor:
		return "minor upgrade"
	}
	return fmt.Sprintf("Step(%d)", int(s))
}

// RefusedError is a move the rule does not allow.
type RefusedError struct {
	// Installed is empty when nothing is installed.
	Installed  string
	Configured string
	Rule       string
	// MinorNotAllowed is set when the move is a minor upgrade that the
	// caller did not allow, so that a CLI can name its flag for it.
	MinorNotAllowed bool
}

func (e *RefusedError) Error() string {
	if e.Installed == "" {
		return fmt.Sprintf("%s is configured: %s", e.Configured, e.Rule)
	}
	return fmt.Sprintf("%s is installed and %s is configured: %s", e.Installed, e.Configured, e.Rule)
}

// Check applies the rule to the installed tag, empty when nothing is
// installed, and the configured one. allowMinor permits a minor
// upgrade. A refusal is a *RefusedError.
func Check(installed, configured string, allowMinor bool) (Step, error) {
	refuse := func(rule string) (Step, error) {
		return 0, &RefusedError{Installed: installed, Configured: configured, Rule: rule}
	}
	if installed != "" && installed == configured {
		return Same, nil
	}
	want, err := Parse(configured)
	if err != nil {
		return refuse(fmt.Sprintf("%v, and only such a tag can be installed or moved to", err))
	}
	if installed == "" {
		return Install, nil
	}
	have, err := Parse(installed)
	if err != nil {
		return refuse(fmt.Sprintf("%v, so no move from it can be checked", err))
	}
	switch {
	case want.Major != have.Major:
		return refuse("a change of major version is refused")
	case want.Minor == have.Minor && want.Patch > have.Patch:
		return Patch, nil
	case want.Minor > have.Minor && allowMinor:
		return Minor, nil
	case want.Minor > have.Minor:
		return 0, &RefusedError{Installed: installed, Configured: configured, MinorNotAllowed: true,
			Rule: "a minor upgrade is refused unless it is allowed; it is best-effort: CRDs and defaults may change, and objects the older release had are not removed"}
	default:
		return refuse("a downgrade is refused")
	}
}
