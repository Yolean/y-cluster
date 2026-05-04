package applianceStateful

import (
	"yolean.se/ystack/yconverge/verify"
	"yolean.se/ystack/appliance-stateful/namespace:applianceStatefulNs"
)

// Declare the namespace module as a dependency. yconverge
// converges _dep_ns FIRST (apply + checks), then this base.
// The "_dep_" prefix is convention; the field name is
// arbitrary, what matters is that we reference the imported
// step so cue's import-tracker pulls it into the graph.
_dep_ns: applianceStatefulNs.step

step: verify.#Step & {
	checks: [
		{
			kind:        "rollout"
			resource:    "statefulset/versitygw"
			namespace:   "appliance-stateful"
			timeout:     "180s"
			description: "versitygw StatefulSet ready"
		},
	]
}
