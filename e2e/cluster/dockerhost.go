//go:build e2e

package cluster

import (
	"net/url"
	"os"
)

// publishHost is the address at which this machine reaches the
// harness containers' published ports: the host of a tcp://
// DOCKER_HOST (a remote daemon such as the y-cluster dockerhost
// guest), else 127.0.0.1 for the local daemon.
func publishHost() string {
	if u, err := url.Parse(os.Getenv("DOCKER_HOST")); err == nil && u.Scheme == "tcp" && u.Hostname() != "" {
		return u.Hostname()
	}
	return "127.0.0.1"
}
