// Package shquote quotes strings for a POSIX shell. y-cluster builds
// command lines for ssh, `multipass exec`, at(1) and `sh -c` from
// values an operator controls (context names, file names, version
// strings), and every one of them has to survive the remote shell
// unchanged.
package shquote

import "strings"

// Quote wraps s in single quotes. Nothing is special inside single
// quotes except the single quote itself, which is written by closing
// the quoted string, adding a backslash-escaped quote and reopening.
func Quote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// Join quotes every arg and joins them with spaces, giving a command
// line whose words are exactly args.
func Join(args []string) string {
	quoted := make([]string, len(args))
	for i, a := range args {
		quoted[i] = Quote(a)
	}
	return strings.Join(quoted, " ")
}
