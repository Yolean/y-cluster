package frontend

import (
	"yolean.se/ystack/yconverge/verify"
	"yolean.se/ystack/e2e-backend/base:backend"
)

_dep_backend: backend.step

step: verify.#Step & {
	checks: [{
		kind:        "exec"
		command:     "kubectl --context=$CONTEXT get configmap frontend-config"
		timeout:     "10s"
		description: "frontend config exists"
	}]
}
