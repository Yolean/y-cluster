package config

import (
	"os"
	"testing"
)

// TestMain pins $USER. HetznerConfig defaults lbGroup from it, and a
// container or CI job that has none must not change what these tests
// see. Tests about the default itself set their own value.
func TestMain(m *testing.M) {
	if err := os.Setenv("USER", "tester"); err != nil {
		panic(err)
	}
	os.Exit(m.Run())
}
