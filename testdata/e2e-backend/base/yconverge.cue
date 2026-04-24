package backend

import (
	"yolean.se/ystack/yconverge/verify"
	"yolean.se/ystack/e2e-db/base:db"
)

_dep_db: db.step

step: verify.#Step & {
	checks: [{
		kind:        "exec"
		command:     "kubectl --context=$CONTEXT get configmap backend-config"
		timeout:     "10s"
		description: "backend config exists"
	}]
}
