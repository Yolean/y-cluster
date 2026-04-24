package e2e_configmap

import (
	"yolean.se/ystack/yconverge/verify"
	"yolean.se/ystack/e2e-namespace:e2e_namespace"
)

_dep_namespace: e2e_namespace.step

step: verify.#Step & {
	checks: [{
		kind:        "exec"
		command:     "kubectl --context=$CONTEXT -n $NAMESPACE get configmap e2e-config"
		timeout:     "10s"
		description: "configmap exists"
	}]
}
