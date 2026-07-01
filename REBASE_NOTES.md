# Rebase Notes — cleanup/pr-2894-llm-hits

Rebased from `feat/pod-deletion-cost-management` (head `455e1ef3`) onto
`origin/main` (head `82671c0b`) on 2026-06-22.

Two textual conflicts hit, both in the `feat:` commit that introduces the
`PodDeletionCostManagement` feature gate. Both resolutions add the new gate
*alongside* the upstream `CapacityBuffer` gate that landed in PR #3028
(merged on main 2026-06-19); neither feature is dropped.

## `pkg/operator/options/options.go` (4 hunks)

Upstream merged `CapacityBuffer` into the `FeatureGates` struct, the
`--feature-gates` default string, `DefaultFeatureGates`, and the
`ParseFeatureGates` switch. PR #2894's `feat:` commit added
`PodDeletionCostManagement` to the same four call sites. Resolution: keep
both. Order: upstream features first (alphabetical within their original
ordering), then `PodDeletionCostManagement` last. The `--feature-gates`
default-string list now reads
`...,StaticCapacity=false,CapacityBuffer=false,PodDeletionCostManagement=false`
and the usage string is updated to enumerate both new gates.

## `pkg/test/options.go` (2 hunks)

Mirror of the above: the test-helper `FeatureGates` struct and the
`Options()` builder both had `CapacityBuffer` added upstream and
`PodDeletionCostManagement` added in the PR. Resolution: keep both in the
same order as in the production struct.

## No semantic conflicts

The remaining 5 commits in the PR's series (`fix(test):`,
`fix(deletioncost):`, `refactor(deletioncost): ponytail`,
`test(deletioncost):`, `refactor(deletioncost): gocyclo`) all replayed
cleanly with no conflicts. The merge commit from the PR's original series
(`a6323a0c`) was dropped by `git rebase` because the merged content was
already linearized onto the new base.

Post-rebase build: `go build ./...` clean.
Post-rebase test: `go test ./pkg/controllers/pod/deletioncost/... ./pkg/controllers/disruption/...` clean.
