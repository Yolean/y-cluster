package config

import (
	"strings"
	"testing"
)

// Values the schema constrains were accepted at load when the schema
// was not in play (no editor, a generated file), and failed later in
// provision or, for reclaimPolicy, inside the cluster.
func TestValidateCommon_RejectsWhatTheSchemaRejects(t *testing.T) {
	for _, tc := range []struct {
		name    string
		mut     func(*QEMUConfig)
		wantErr string
	}{
		{"defaults", func(*QEMUConfig) {}, ""},
		{"memory with a unit", func(c *QEMUConfig) { c.Memory = "8G" }, "memory"},
		{"memory zero", func(c *QEMUConfig) { c.Memory = "0" }, "memory"},
		{"cpus as a word", func(c *QEMUConfig) { c.CPUs = "four" }, "cpus"},
		{"fractional cpus", func(c *QEMUConfig) { c.CPUs = "0.5" }, "cpus"},
		{"unknown reclaim policy", func(c *QEMUConfig) { c.Storage.ReclaimPolicy = "Recycle" }, "storage.reclaimPolicy"},
		{"reclaim policy Delete", func(c *QEMUConfig) { c.Storage.ReclaimPolicy = "Delete" }, ""},
		{"guest port missing", func(c *QEMUConfig) { c.PortForwards = append(c.PortForwards, PortForward{Host: "8080"}) }, "portForwards[3].guest"},
		{"guest port out of range", func(c *QEMUConfig) { c.PortForwards[0].Guest = "70000" }, "portForwards[0].guest"},
		{"host port not a number", func(c *QEMUConfig) { c.PortForwards[0].Host = "https" }, "portForwards[0].host"},
		{"host port forwarded twice", func(c *QEMUConfig) { c.PortForwards[1].Host = c.PortForwards[0].Host }, "forwarded twice"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := &QEMUConfig{}
			c.ApplyDefaults()
			tc.mut(c)
			err := c.Validate()
			if tc.wantErr == "" {
				if err != nil {
					t.Fatalf("unexpected error: %v", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("want error containing %q, got %v", tc.wantErr, err)
			}
		})
	}
}

// docker assigns a host port when none is given. qemu cannot: its
// hostfwd rule is rejected with "Bad host port", and the documented
// "SLIRP-assigned" never existed.
func TestPortForwards_EmptyHostPerProvider(t *testing.T) {
	forwards := []PortForward{{Host: "6443", Guest: "6443"}, {Host: "", Guest: "8080"}}

	d := &DockerConfig{CommonConfig: CommonConfig{PortForwards: forwards}}
	d.ApplyDefaults()
	if err := d.Validate(); err != nil {
		t.Errorf("docker: empty host port is docker-assigned, got %v", err)
	}

	q := &QEMUConfig{CommonConfig: CommonConfig{PortForwards: forwards}}
	q.ApplyDefaults()
	if err := q.Validate(); err == nil || !strings.Contains(err.Error(), "portForwards[1].host is empty") {
		t.Errorf("qemu: want a rejection naming the forward, got %v", err)
	}

	clash := &QEMUConfig{SSHPort: "6443"}
	clash.ApplyDefaults()
	if err := clash.Validate(); err == nil || !strings.Contains(err.Error(), "also sshPort") {
		t.Errorf("qemu: a forward on the ssh port must be rejected, got %v", err)
	}
}
