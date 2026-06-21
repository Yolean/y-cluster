package lifetime

import (
	"strings"
	"testing"
)

func TestGCPFlags(t *testing.T) {
	tests := []struct {
		in      string
		want    string
		wantErr bool
	}{
		{"", "", false},
		{"0", "", false},
		{"8h", "--max-run-duration=28800s --instance-termination-action=DELETE", false},
		{"90m", "--max-run-duration=5400s --instance-termination-action=DELETE", false},
		{"banana", "", true},
		{"-5m", "", true},
	}
	for _, tt := range tests {
		got, err := GCPFlags(tt.in)
		if tt.wantErr {
			if err == nil {
				t.Errorf("GCPFlags(%q): expected error, got %q", tt.in, got)
			}
			continue
		}
		if err != nil {
			t.Errorf("GCPFlags(%q): unexpected error %v", tt.in, err)
			continue
		}
		if got != tt.want {
			t.Errorf("GCPFlags(%q) = %q, want %q", tt.in, got, tt.want)
		}
	}
}

// The DELETE action is what preserves the separately-attached data
// disk while stopping compute billing -- guard it explicitly.
func TestGCPFlags_DeletesInstance(t *testing.T) {
	got, err := GCPFlags("1h")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(got, "--instance-termination-action=DELETE") {
		t.Fatalf("expected DELETE termination action, got %q", got)
	}
}
