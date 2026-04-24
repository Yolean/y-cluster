package db

import "yolean.se/ystack/yconverge/verify"

step: verify.#Step & {
	checks: [{
		kind:        "exec"
		command:     "kubectl --context=$CONTEXT get configmap db-config"
		timeout:     "10s"
		description: "db config exists"
	}]
}
