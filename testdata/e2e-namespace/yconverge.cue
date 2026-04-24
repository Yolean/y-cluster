package e2e_namespace

import "yolean.se/ystack/yconverge/verify"

step: verify.#Step & {
	checks: [{
		kind:        "wait"
		resource:    "ns/y-cluster-e2e"
		for:         "jsonpath={.status.phase}=Active"
		timeout:     "10s"
		description: "namespace is active"
	}]
}
