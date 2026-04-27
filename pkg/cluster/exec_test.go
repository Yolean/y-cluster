package cluster

import (
	"testing"
)

// TestBuildQemuRemote covers the bit of the qemu node-exec path
// we still own ourselves: shaping the single shell-string ssh
// passes as the remote command. Transport-level concerns (auth,
// connection refused, exit codes) belong to pkg/sshexec and its
// integration tests.
func TestBuildQemuRemote_ShellQuotes(t *testing.T) {
	got := buildQemuRemote("crictl", []string{"images", "--label", "app=isn't"})
	want := `sudo k3s crictl 'images' '--label' 'app=isn'\''t'`
	if got != want {
		t.Fatalf("remote cmd:\n got: %q\nwant: %q", got, want)
	}
}

func TestBuildQemuRemote_NoArgs(t *testing.T) {
	got := buildQemuRemote("ctr", nil)
	want := "sudo k3s ctr"
	if got != want {
		t.Fatalf("got %q want %q", got, want)
	}
}

func TestShellQuoteJoin(t *testing.T) {
	cases := []struct {
		in   []string
		want string
	}{
		{nil, ""},
		{[]string{}, ""},
		{[]string{"a"}, " 'a'"},
		{[]string{"a", "b"}, " 'a' 'b'"},
		{[]string{"a b"}, " 'a b'"},
		{[]string{"it's"}, ` 'it'\''s'`},
	}
	for _, c := range cases {
		if got := shellQuoteJoin(c.in); got != c.want {
			t.Errorf("shellQuoteJoin(%v) = %q, want %q", c.in, got, c.want)
		}
	}
}
