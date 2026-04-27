package envoygateway

import _ "embed"

// gatewayClassYAML is the default `eg` GatewayClass manifest.
// Tiny (~10 lines) and y-cluster-owned, so it stays embedded
// rather than being downloaded -- nothing about it changes per
// EG release. install.yaml, by contrast, is the upstream release
// asset and lives in the per-version cache so a fresh provision
// can pick a different Version without recompiling.
//
//go:embed assets/gatewayclass.yaml
var gatewayClassYAML []byte

// GatewayClassYAML returns the embedded default GatewayClass
// bytes. Read-only.
func GatewayClassYAML() []byte { return gatewayClassYAML }
