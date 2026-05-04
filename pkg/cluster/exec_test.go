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

// TestSingleQuote covers the helper RunShell uses to wrap an
// entire `sh -c <cmd>` string for ssh / multipass-exec -- the
// caller's command must survive a second round of shell parsing
// without losing semantics. Embedded single quotes go through
// the standard `'\''` trick.
func TestSingleQuote(t *testing.T) {
	cases := []struct {
		in   string
		want string
	}{
		{"", "''"},
		{"hello", "'hello'"},
		{"hello world", "'hello world'"},
		{"it's", `'it'\''s'`},
		{"'leading", `''\''leading'`},
		{"trailing'", `'trailing'\'''`},
		{"a'b'c", `'a'\''b'\''c'`},
		{"echo $HOME", "'echo $HOME'"},
	}
	for _, c := range cases {
		if got := singleQuote(c.in); got != c.want {
			t.Errorf("singleQuote(%q) = %q, want %q", c.in, got, c.want)
		}
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
