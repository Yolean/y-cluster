package db

import "yolean.se/ystack/yconverge/verify"

step: verify.#Step & {
	checks: [
		{
			kind:        "exec"
			command:     "kubectl --context=$CONTEXT get configmap db-config"
			timeout:     "10s"
			description: "db config exists"
		},
		{
			kind:        "exec"
			command:     "kubectl --context=$CONTEXT create configmap db-check-marker --from-literal=checked=true 2>/dev/null || kubectl --context=$CONTEXT get configmap db-check-marker"
			timeout:     "10s"
			description: "db check marker created"
		},
	]
}
