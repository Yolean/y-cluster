package verify

// A convergence step: apply a kustomize base, then verify.
// The yconverge.cue file must be next to a kustomization.yaml.
// The kustomization path is implicit from the file location.
#Step: {
	// Checks that must pass after apply.
	// Empty list means the step is ready immediately after apply.
	checks: [...#Check]
}

// Check is a discriminated union. Each variant maps to a kubectl
// subcommand that manages its own timeout and output, or (for
// kind: "gateway") to an in-cluster ephemeral curl probe with
// auto-discovered Gateway address pinning.
#Check: #Wait | #Rollout | #Exec | #Gateway

// Thin wrapper around kubectl wait.
// Timeout and output are managed by kubectl.
#Wait: {
	kind:        "wait"
	resource:    string
	for:         string
	namespace?:  string
	timeout:     *"60s" | string
	description: *"" | string
}

// Thin wrapper around kubectl rollout status.
// Timeout and output are managed by kubectl.
#Rollout: {
	kind:        "rollout"
	resource:    string
	namespace?:  string
	timeout:     *"60s" | string
	description: *"" | string
}

// Arbitrary command for checks that don't map to kubectl builtins.
// The engine retries until timeout.
#Exec: {
	kind:        "exec"
	command:     string
	timeout:     *"60s" | string
	description: string
}

// HTTP probe through the cluster's Gateway. The runtime discovers
// the Gateway's programmed address (Gateway.status.addresses) for
// the configured class, launches an ephemeral in-cluster curl Pod
// with `--resolve <host>:<port>:<gateway-addr>` so the request
// actually traverses Gateway -> HTTPRoute -> backend (no DNS or
// /etc/hosts dependency on the host running yconverge). The
// engine retries until timeout.
//
// expectCode is always a list -- single-status callers write
// `expectCode: [302]`. Empty defaults to `[200]` at runtime.
//
// expectLocation is a Go regexp matched against the response
// Location header; useful for asserting "redirected to oauth, on
// the right realm" without the curl-grep false-positives that
// kind: exec is prone to.
#Gateway: {
	kind:               "gateway"
	url:                string
	expectCode:         *[200] | [...int]
	expectLocation?:    string
	// Optional explicit override of the Gateway-address discovery
	// (curl --resolve target IP). Empty -> auto-discover from
	// Gateway.status.addresses.
	resolve?:           string
	// Optional GatewayClass narrowing. Empty -> first programmed
	// Gateway across all classes.
	gatewayClassName?:  string
	timeout:            *"60s" | string
	description:        *"" | string
}
