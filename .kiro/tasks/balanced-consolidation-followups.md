# Balanced Consolidation — Follow-up Tasks

## Critical

- [ ] **Reconcile API design with design doc** — Design doc proposes IntOrString `consolidationPolicy` (Balanced=k=2, integers 1-3). Implementation uses separate `consolidationThreshold *int32` field. Update the design doc to match the implementation or vice versa.
- [ ] **Fix feature gate fallback behavior** — Design doc says disabling the gate should fall back to `WhenEmptyOrUnderutilized`. Current code rejects Balanced candidates entirely (no consolidation). Either implement the fallback or update the design doc.
- [ ] **Fix `getCandidatePrices` batch-zero issue** — One candidate with missing offerings zeros out savings for the entire multi-node batch. Return 0 only for the problematic candidate.

## Design

- [ ] **Document multi-node binary search monotonicity limitation** — Adding a high-value candidate to a failing batch could push the score above threshold, but the binary search assumes monotonicity. Document as known limitation.
- [ ] **Sort single-node candidates by savings ratio for Balanced** — `SingleNodeConsolidation.SortCandidates` always sorts by DisruptionCost. High-value Balanced candidates could hit the 3-min timeout before being evaluated.
- [ ] **Verify status condition persistence** — `ShouldDisrupt` sets `ConsolidationPolicyUnsupported` in-memory. Confirm the controller persists NodePool status to the API server.
- [ ] **Cross-NodePool savings attribution accuracy** — Proportional attribution by source cost isn't accurate for REPLACE moves. Acceptable for v1, revisit if cross-pool replaces become common.

## Code Quality

- [ ] **Remove double scoring in multi-node path** — Binary search scores inside the loop, then `EmitBalancedMultiNodeEvents` re-scores. Pass the result through instead.
- [ ] **Avoid wasted `computeNodePoolTotals` in validation** — `GetCandidates` computes totals on every call including validation paths that discard them. Add a variant without totals or cache them.
- [ ] **Unify `getCandidatePrices` and `candidatePrice`** — Both compute offering prices with slightly different semantics. Consider having the batch version call the single version.
- [ ] **Align event message terminology** — Events say "consolidationThreshold", design doc says "k". Pick one.

## Tests

- [ ] **Add full-loop integration tests** — Test `ComputeCommands` → `EvaluateBalancedMove` → `Validate` with real cluster state for both single-node and multi-node paths.
- [ ] **Add k=3 behavior test** — Verify a move that passes at k=3 but fails at k=2, matching the design doc's cross-family pair analysis.
- [ ] **Add feature gate fallback test** — Test the actual behavior when gate is disabled after Balanced was configured.
- [ ] **Extract shared `approxEqual` test helper** — Duplicated in `balanced_test.go` and `balanced_integration_test.go`.
