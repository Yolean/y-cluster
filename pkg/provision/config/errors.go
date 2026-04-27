package config

import "fmt"

// errInvalid is the sole error constructor in this package. Centralizing
// it keeps validation messages uniform and lets callers test for the
// shape if they ever need to (`errors.As` against a future named type)
// without changing every call site.
func errInvalid(format string, args ...any) error {
	return fmt.Errorf(format, args...)
}
