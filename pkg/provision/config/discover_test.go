package config

import (
	"testing"
)

// DiscoverProvider's inputs are the kernel (/dev/kvm) and the
// docker daemon, neither of which is mock-friendly. These tests
// exercise the contract -- never invent a provider name, be
// idempotent, helper functions don't panic -- and rely on the
// per-provider e2e tests for the "actually provisions" coverage.

func TestDiscoverProvider_ReturnsKnownProviderOrEmpty(t *testing.T) {
	got := DiscoverProvider()
	if got == "" {
		return
	}
	for _, p := range AllProviders {
		if got == p {
			return
		}
	}
	t.Fatalf("DiscoverProvider returned %q which is not in AllProviders %v",
		got, AllProviders)
}

func TestDiscoverProvider_Idempotent(t *testing.T) {
	if a, b := DiscoverProvider(), DiscoverProvider(); a != b {
		t.Fatalf("not idempotent: %q vs %q", a, b)
	}
}

func TestHasKVM_DoesNotPanic(t *testing.T) {
	_ = hasKVM()
}

func TestHasBinary_NotFound(t *testing.T) {
	if hasBinary("y-cluster-discovery-no-such-binary-1234567890") {
		t.Fatal("no such binary should not be found on PATH")
	}
}

func TestHasBinary_FoundOnPosixHost(t *testing.T) {
	if !hasBinary("sh") {
		t.Skip("test host has no sh; nothing to verify")
	}
}
