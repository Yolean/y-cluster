package config

import (
	"fmt"
	"strconv"
	"strings"
)

// positiveInt parses a decimal string that must be a whole number
// greater than zero. Used for the CommonConfig fields that are
// strings for the local providers' sake (qemu and docker pass them
// straight through) but have to be integers where they end up.
func positiveInt(s string) (int, error) {
	n, err := strconv.Atoi(strings.TrimSpace(s))
	if err != nil {
		return 0, fmt.Errorf("not a whole number: %q", s)
	}
	if n <= 0 {
		return 0, fmt.Errorf("must be > 0, got %d", n)
	}
	return n, nil
}
