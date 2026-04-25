//go:build !unix

package serve

import "fmt"

// spawnBackground is not supported on non-unix platforms in the first
// release. Release artifacts target linux and darwin.
func spawnBackground(execPath string, args []string, paths StatePaths) (int, error) {
	return 0, fmt.Errorf("background daemon not supported on this platform; pass --foreground")
}
