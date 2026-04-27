// `go generate ./pkg/provision/...` (or this package directly)
// regenerates the per-provisioner schema files in
// pkg/provision/schema/. CI runs the same command and fails if the
// working tree differs afterwards, so the generator output and the
// source struct tags stay in lockstep.
//
// The generator imports this package (and any sibling provider
// packages) so adding a new provisioner means: add a new struct in
// pkg/provision/config/, register it in cmd/internal/schemagen, run
// `go generate`, commit the new schema file alongside the code.

//go:generate go run ../../../cmd/internal/schemagen

package config
