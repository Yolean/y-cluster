package multipass

import (
	"strings"
	"testing"
)

// The host dials the VM by the address multipass gave it, so the
// apiserver cert has to carry that address.
func TestK3sServerFlags_TLSSanForVMIP(t *testing.T) {
	c := &Cluster{vmIP: "192.168.64.10"}
	if flags := c.k3sServerFlags(); !strings.HasSuffix(flags, " --tls-san=192.168.64.10") {
		t.Fatalf("flags: %q", flags)
	}
}
