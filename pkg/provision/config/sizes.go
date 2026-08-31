package config

import (
	"fmt"
	"strconv"
	"strings"
)

// positiveInt parses a decimal string that must be a whole number
// greater than zero. Used for the CommonConfig fields that are
// strings for the local providers' sake (qemu and docker pass them
// straight through) but have to be integers for a cloud API.
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

// DiskSizeGB converts a [num][KMGT] disk size - qemu's spelling,
// reused so one form covers every VM provisioner - into whole
// gigabytes for APIs that size disks in GB.
//
// A bare number is read as gigabytes, which is what an operator
// writing `diskSize: "30"` means. Units below a gigabyte are
// rejected rather than rounded: a 500M disk silently becoming 0GB
// (or 1GB) is a worse outcome than being told the unit is too
// small for this provider.
func DiskSizeGB(s string) (int, error) {
	v := strings.TrimSpace(s)
	if v == "" {
		return 0, fmt.Errorf("empty")
	}
	// Tolerate the "30GB" spelling alongside qemu's "30G".
	v = strings.TrimSuffix(v, "B")
	if v == "" {
		return 0, fmt.Errorf("no number before the unit")
	}

	unit := byte('G')
	if last := v[len(v)-1]; last < '0' || last > '9' {
		unit = last
		v = v[:len(v)-1]
	}

	n, err := strconv.Atoi(v)
	if err != nil {
		return 0, fmt.Errorf("not a number followed by an optional [KMGT] unit")
	}
	if n <= 0 {
		return 0, fmt.Errorf("must be > 0, got %d", n)
	}

	switch unit {
	case 'G', 'g':
		return n, nil
	case 'T', 't':
		return n * 1024, nil
	case 'K', 'k', 'M', 'm':
		return 0, fmt.Errorf("unit %q is smaller than the 1G granularity this provider sizes disks in", string(unit))
	default:
		return 0, fmt.Errorf("unknown unit %q, expected one of K M G T", string(unit))
	}
}
