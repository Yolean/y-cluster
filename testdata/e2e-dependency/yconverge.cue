package e2e_dependency

import (
	"yolean.se/ystack/yconverge/verify"
	"yolean.se/ystack/e2e-configmap:e2e_configmap"
)

_dep_configmap: e2e_configmap.step

step: verify.#Step & {
	checks: [{
		kind:        "exec"
		command:     "kubectl --context=$CONTEXT -n $NAMESPACE get configmap e2e-dependent"
		timeout:     "10s"
		description: "dependent configmap exists"
	}]
}
