package envoygateway

import (
	"strings"
	"testing"
)

// TestVersionFormat keeps the constant looking like a release
// tag so tools that build URLs from it (image refs, GitHub
// release links) get a sensible value.
func TestVersionFormat(t *testing.T) {
	if !strings.HasPrefix(Version, "v") {
		t.Fatalf("Version %q must start with 'v' to match upstream release tag form", Version)
	}
}
