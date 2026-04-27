//go:build e2e

// Package cluster is the shared e2e harness. Tests call Kwok(t)
// (or, when added later, Docker / QEMU constructors) to get a
// running Kubernetes API and the kubeconfig metadata they need to
// drive it. Everything here is gated by `//go:build e2e` — the
// package is invisible to `go test ./...`.
//
// kwok is the default cheap backend: a fake apiserver in a single
// Docker container, fast enough that all tests in this package
// share one instance for the lifetime of `go test`. Real-cluster
// backends (docker, qemu) provision per-test because their setup
// cost is per-test acceptable and per-test isolation is what those
// tests actually want to verify.
package cluster

// Backend identifies which runtime is serving the Kubernetes API
// to the test. Tests rarely need to switch on this — the harness
// returns a *Cluster whose Context()/Kubeconfig() are uniform —
// but it's exposed so a test can document what it covered, and so
// CI matrix logs can attribute results to a backend.
type Backend string

const (
	BackendKwok   Backend = "kwok"
	BackendDocker Backend = "docker"
	BackendQEMU   Backend = "qemu"
)

// Cluster is the harness handle. Backends fill the same three
// fields so tests can be backend-agnostic: set KUBECONFIG to
// .Kubeconfig and pass `--context=<.Context>` to kubectl, or use
// the Go client with the same kubeconfig.
//
// The struct is intentionally a value type: the harness owns
// lifecycle (singleton kwok, per-test docker/qemu), tests just
// read the fields.
type Cluster struct {
	// Backend is which runtime served this cluster.
	Backend Backend

	// Context is the kubeconfig context name. kubectl
	// invocations should pass `--context=<this>` so they target
	// the e2e cluster regardless of what current-context is.
	Context string

	// Kubeconfig is the absolute path to the kubeconfig file.
	// Tests typically `os.Setenv("KUBECONFIG", c.Kubeconfig)`
	// for their lifetime; the harness handles cleanup via
	// t.Cleanup.
	Kubeconfig string
}
