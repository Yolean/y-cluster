# Downstream fixtures

Verbatim copies of files that repos depending on y-cluster keep, so
that y-cluster's unit tests fail when a change would break them.
They are inputs to `cmd/y-cluster/downstream_contract_test.go`; the
list of CLI verbs and flags those repos call lives in that test.

| Fixture | Source |
|---|---|
| `ystack-local-docker/`, `ystack-local-qemu/` | `Yolean/ystack` `cluster-configs/local-{docker,qemu}/y-cluster-provision.yaml` |
| `checkit-appliance-test/`, `checkit-appliance-import/` | checkit `cluster-local/e2e/appliance-{test,import}/y-cluster-provision.yaml` |

This is a tripwire, not the acceptance test. The real verification is
ystack's `e2e/agents-clusterautomation-acceptance-*.sh` and checkit's
`cluster-local/e2e/`, which run a downstream converge through
`APPLIANCE_SEED_CMD` / `APPLIANCE_VERIFY_CMD` (scripts/_hooks.sh).
When one of those repos starts using a new verb, flag or config key,
add it here.
