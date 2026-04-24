package e2e_namespace_check

import "yolean.se/ystack/yconverge/verify"

step: verify.#Step & {
	checks: [{
		kind:        "exec"
		command:     "test \"$NAMESPACE\" = \"y-cluster-e2e\""
		timeout:     "5s"
		description: "NAMESPACE env var is set correctly"
	}]
}
