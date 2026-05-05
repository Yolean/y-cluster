package hetzner

import "testing"

// TestAtJobIDRE_Variants pins the regex against the line shapes
// observed across at(1) implementations. The "warning: commands
// will be executed using /bin/sh" preamble is implementation-
// dependent; the "job N at <date>" line is the contract we rely on.
func TestAtJobIDRE_Variants(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string // empty == no match expected
	}{
		{
			name: "GNU at, with warning preamble",
			in:   "warning: commands will be executed using /bin/sh\njob 7 at Mon May 12 18:34:00 2025\n",
			want: "7",
		},
		{
			name: "BSD at, no warning, single tab",
			in:   "job\t42\tat Mon May 12 18:34:00 2025\n",
			want: "42",
		},
		{
			name: "no job line",
			in:   "warning: commands will be executed using /bin/sh\n",
			want: "",
		},
		{
			name: "embedded line, must anchor",
			in:   "lots of preamble\n  job 1 at Mon\n",
			// Must NOT match -- the leading spaces would create
			// false positives if we relaxed the anchor.
			want: "",
		},
		{
			name: "at end-of-line marker (multi-job log)",
			in:   "job 99 at Tue Jan 01 00:00:00 2030\njob 100 at Tue Jan 01 00:00:00 2030\n",
			// First match is what scheduleAutoTeardown returns;
			// at(1) emits one job id per invocation, so seeing
			// two means our parser stays consistent under
			// concurrent log noise.
			want: "99",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			m := atJobIDRE.FindStringSubmatch(tc.in)
			if tc.want == "" {
				if m != nil {
					t.Errorf("expected no match, got %v", m)
				}
				return
			}
			if m == nil {
				t.Fatalf("no match in %q", tc.in)
			}
			if m[1] != tc.want {
				t.Errorf("job id: got %q, want %q", m[1], tc.want)
			}
		})
	}
}
