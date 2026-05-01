package selector_target

import (
	"yolean.se/ystack/yconverge/verify"
	"yolean.se/ystack/e2e-selector-dep/base:selector_dep"
)

_dep: selector_dep.step

step: verify.#Step & {
	checks: []
}
