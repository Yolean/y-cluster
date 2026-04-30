package multipassexec

import (
	"errors"
	"testing"
)

func TestIsNotFoundOutput(t *testing.T) {
	cases := map[string]bool{
		"":                                       false,
		"instance \"foo\" does not exist":        true,
		"unknown instance \"foo\"":               true,
		"Error: instance not found: foo":         true,
		"info failed: foo not found":             true,
		"some other error":                       false,
		"failed to start, daemon not reachable":  false,
		"INSTANCE DOES NOT EXIST: foo":           true,
		"trace: error: instance does not exist": true,
	}
	for in, want := range cases {
		if got := isNotFoundOutput([]byte(in)); got != want {
			t.Errorf("isNotFoundOutput(%q) = %v, want %v", in, got, want)
		}
	}
}

func TestFirstIPv4(t *testing.T) {
	if got := FirstIPv4(&VMInfo{IPv4: nil}); got != "" {
		t.Fatalf("nil ipv4: %q", got)
	}
	if got := FirstIPv4(&VMInfo{IPv4: []string{""}}); got != "" {
		t.Fatalf("empty entry: %q", got)
	}
	if got := FirstIPv4(&VMInfo{IPv4: []string{"10.0.0.5"}}); got != "10.0.0.5" {
		t.Fatalf("single entry: %q", got)
	}
	if got := FirstIPv4(&VMInfo{IPv4: []string{"", "192.168.64.10"}}); got != "192.168.64.10" {
		t.Fatalf("skips empty entry: %q", got)
	}
}

func TestErrNotFound_IsSentinel(t *testing.T) {
	wrapped := errors.Join(ErrNotFound, errors.New("context"))
	if !errors.Is(wrapped, ErrNotFound) {
		t.Fatal("errors.Is should match wrapped ErrNotFound")
	}
}
