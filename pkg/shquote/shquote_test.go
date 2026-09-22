package shquote

import (
	"os/exec"
	"strings"
	"testing"
)

func TestQuote(t *testing.T) {
	for in, want := range map[string]string{
		"":                "''",
		"v1.35.4+k3s1":    "'v1.35.4+k3s1'",
		"--disable=a b":   "'--disable=a b'",
		"it's":            `'it'\''s'`,
		"'leading":        `''\''leading'`,
		"trailing'":       `'trailing'\'''`,
		"a'b'c":           `'a'\''b'\''c'`,
		"$(rm -rf /) `x`": "'$(rm -rf /) `x`'",
	} {
		if got := Quote(in); got != want {
			t.Errorf("Quote(%q) = %s, want %s", in, got, want)
		}
	}
}

func TestJoin_Empty(t *testing.T) {
	if got := Join(nil); got != "" {
		t.Errorf("Join(nil) = %q, want empty", got)
	}
}

// TestJoin_SurvivesShell is the requirement itself: whatever the
// words are, sh hands exactly those words to the command.
func TestJoin_SurvivesShell(t *testing.T) {
	sh, err := exec.LookPath("sh")
	if err != nil {
		t.Skip("no sh on PATH")
	}
	words := []string{
		"plain",
		"",
		"two words",
		"it's",
		"'''",
		`back\slash`,
		"$HOME `id` $(id)",
		"semi;colon && amp | pipe > redirect",
		"glob * ? [a-z]",
		"new\nline",
		"tab\there",
		`"double"`,
		"#comment",
		"~",
	}
	// printf with a NUL after every word, so newlines inside a word
	// stay distinguishable from the separator.
	out, err := exec.Command(sh, "-c", `printf '%s\0' `+Join(words)).Output()
	if err != nil {
		t.Fatalf("sh: %v", err)
	}
	got := strings.Split(strings.TrimSuffix(string(out), "\x00"), "\x00")
	if len(got) != len(words) {
		t.Fatalf("sh saw %d words, want %d: %q", len(got), len(words), got)
	}
	for i := range words {
		if got[i] != words[i] {
			t.Errorf("word %d: sh saw %q, want %q", i, got[i], words[i])
		}
	}
}
