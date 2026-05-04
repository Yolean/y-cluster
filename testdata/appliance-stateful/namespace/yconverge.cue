package applianceStatefulNs

import "yolean.se/ystack/yconverge/verify"

// Apply this base, then wait until the apiserver reports the
// namespace as Active. Importing modules can rely on the
// namespace existing in their own checks because yconverge
// runs deps' apply + checks BEFORE the importer's apply.
step: verify.#Step & {
	checks: [
		{
			kind:        "wait"
			resource:    "namespace/appliance-stateful"
			for:         "jsonpath={.status.phase}=Active"
			timeout:     "30s"
			description: "appliance-stateful namespace Active"
		},
	]
}
