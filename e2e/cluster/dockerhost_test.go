//go:build e2e

package cluster

import "testing"

func TestPublishHost(t *testing.T) {
	for host, want := range map[string]string{
		"":                            "127.0.0.1",
		"unix:///var/run/docker.sock": "127.0.0.1",
		"tcp://10.88.1.2:2376":        "10.88.1.2",
		"tcp://dockerd:2375":          "dockerd",
		"ssh://user@host":             "127.0.0.1",
	} {
		t.Setenv("DOCKER_HOST", host)
		if got := publishHost(); got != want {
			t.Errorf("DOCKER_HOST=%q: got %q, want %q", host, got, want)
		}
	}
}
