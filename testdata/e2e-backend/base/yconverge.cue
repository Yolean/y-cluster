package backend

import (
	"yolean.se/ystack/yconverge/verify"
	"yolean.se/ystack/e2e-db/base:db"
)

_dep_db: db.step

step: verify.#Step & {
	checks: [
		{
			kind:        "exec"
			command:     "kubectl --context=$CONTEXT get configmap backend-config"
			timeout:     "10s"
			description: "backend config exists"
		},
		{
			// This check proves that db was converged (applied AND checked)
			// before backend was applied. If both were bundled into one
			// atomic apply, the db-check-marker would not exist because
			// db's check (which creates it) would not have run yet.
			kind:        "exec"
			command:     "kubectl --context=$CONTEXT get configmap db-check-marker -o jsonpath={.data.checked} | grep -q true"
			timeout:     "10s"
			description: "db check completed before backend apply"
		},
	]
}
