# Balanced Consolidation Implementation Review

## Overview
Implementation of the Balanced consolidation policy for Karpenter, which scores consolidation moves by comparing savings-to-disruption ratios against a configurable threshold (k), rejecting moves where disruption outweighs savings.

## Files Changed (21 files, +309/-38)

### API Layer
- [x] `pkg/apis/v1/nodepool.go` — New `ConsolidationPolicyBalanced`, `ConsolidationThreshold` field, `DefaultConsolidationThreshold`
- [x] `pkg/apis/v1/nodepool_defaults.go` — Default threshold=2 when Balanced + nil threshold
- [x] `pkg/apis/v1/nodepool_status.go` — `ConditionTypeConsolidationPolicyUnsupported` status condition
- [x] `pkg/apis/v1/nodepool_validation.go` — Validate threshold only valid with Balanced policy
- [x] `pkg/apis/v1/zz_generated.deepcopy.go` — DeepCopy for *int32 ConsolidationThreshold
- [x] `pkg/apis/crds/karpenter.sh_nodepools.yaml` — CRD schema update
- [x] `kwok/charts/crds/karpenter.sh_nodepools.yaml` — KWOK CRD schema update

### Feature Gate
- [x] `pkg/operator/options/options.go` — `BalancedConsolidation` feature gate (default: false)

### Events
- [x] `pkg/events/reason.go` — `ConsolidationApproved` reason constant
- [x] `pkg/controllers/disruption/events/events.go` — Balanced event emitters (single/multi node, approved/rejected)

### Metrics
- [x] `pkg/controllers/disruption/metrics.go` — `ConsolidationScoreHistogram`, `ConsolidationMovesTotal`

### Core Scoring Logic (NEW FILE)
- [x] `pkg/controllers/disruption/balanced.go` — `ScoreMove`, `EvaluateBalancedMove`, `computeNodePoolTotals`, `ComputeMoveDisruptionCost`, `candidatePrice`, `AnyBalancedCandidate`, `EmitBalancedMultiNodeEvents`

### Controller Integration
- [x] `pkg/controllers/disruption/consolidation.go` — `NodePoolTotalsSetter`, `sortCandidates` (ratio-based for Balanced), `candidateSavingsRatio`, feature gate check in `ShouldDisrupt`
- [x] `pkg/controllers/disruption/controller.go` — Pass `nodePoolTotals` to methods via `NodePoolTotalsSetter`
- [x] `pkg/controllers/disruption/helpers.go` — `GetCandidates` returns `NodePoolTotals`, `computeNodePoolTotals` called before filtering
- [x] `pkg/controllers/disruption/singlenodeconsolidation.go` — Balanced scoring + events/metrics in `ComputeCommands`
- [x] `pkg/controllers/disruption/multinodeconsolidation.go` — Balanced scoring in `firstNConsolidationOption` binary search + events
- [x] `pkg/controllers/disruption/emptiness.go` — Policy check updated for Balanced
- [x] `pkg/controllers/disruption/validation.go` — Signature change for `GetCandidates` (now returns totals)

### Tests
- [x] `pkg/controllers/disruption/balanced_integration_test.go` — NEW: comprehensive unit tests for scoring, totals, events, sorting
- [x] `pkg/controllers/disruption/consolidation_test.go` — Updated for new `GetCandidates` signature
- [x] `pkg/controllers/disruption/emptiness_test.go` — Updated for new `GetCandidates` signature
- [x] `pkg/controllers/disruption/drift_test.go` — Updated for new `GetCandidates` signature

---

## Review Findings

### Critical Issues

- [ ] **CRIT-1: Design doc says IntOrString for consolidationPolicy, implementation uses separate field**
  The design doc proposes `consolidationPolicy` as an `IntOrString` where `Balanced` maps to k=2 and integer values 1-3 pass k directly. The implementation instead adds a separate `consolidationThreshold *int32` field alongside a string enum `consolidationPolicy`. This is a deliberate deviation from the RFC but should be called out — the API surface is different from what the design doc describes. If this is intentional, the design doc should be updated to match.

- [ ] **CRIT-2: `EstimatedSavings()` uses cheapest offering of first instance type for replacement cost**
  In `EstimatedSavings()`, the replacement cost is `nodeClaim.InstanceTypeOptions[0].Offerings.Cheapest().Price`. Since instance types are sorted by price before this point, `[0]` is the cheapest option. However, the actual launched instance may not be the cheapest (especially for spot). This means the score could overestimate savings. This is a pre-existing issue but becomes more impactful with scoring since marginal moves near the threshold could flip.

- [ ] **CRIT-3: `getCandidatePrices` returns 0 on missing offerings, causing `EstimatedSavings` to return 0**
  If any candidate in a multi-node batch has no compatible offerings, `getCandidatePrices` returns 0 for the entire batch (early return). This means `EstimatedSavings()` returns 0, and the score is 0 (rejected). This is conservative but could silently reject valid multi-node moves where only one candidate has stale offerings. Consider returning 0 only for the problematic candidate rather than the whole batch.

### Design Concerns

- [ ] **DES-1: Cross-NodePool savings attribution is proportional to source cost, not actual savings**
  In `EvaluateBalancedMove`, cross-pool savings are attributed as `savings * (poolCost / totalCost)`. This assumes savings are proportional to source cost, which isn't true for REPLACE moves where the replacement cost is shared. For DELETE-only cross-pool moves this is correct. For mixed moves, the attribution could be inaccurate. The design doc acknowledges this is a simplification.

- [ ] **DES-2: Multi-node binary search + balanced scoring interaction**
  The binary search in `firstNConsolidationOption` assumes monotonicity: if N nodes can't consolidate, N+1 can't either. With balanced scoring, this isn't strictly true — adding a high-value candidate to a failing batch could push the score above threshold. The sort-by-ratio mitigation helps but doesn't guarantee monotonicity. This could miss some valid multi-node consolidation opportunities. Acceptable for v1 but worth documenting.

- [ ] **DES-3: `ShouldDisrupt` sets status condition on NodePool but doesn't persist it**
  `ShouldDisrupt` calls `cn.NodePool.StatusConditions().SetTrueWithReason(...)` and `Clear(...)` but these are in-memory mutations on the NodePool object. The condition won't be persisted to the API server unless something else writes the NodePool status. Verify that the disruption controller's reconcile loop persists NodePool status updates.

### Code Quality

- [ ] **CODE-1: `sortCandidates` on `consolidation` vs `SortCandidates` on `SingleNodeConsolidation`**
  There are two sort methods: `consolidation.sortCandidates` (used by multi-node and emptiness) and `SingleNodeConsolidation.SortCandidates` (exported, used by single-node). The single-node version doesn't use the balanced ratio sort — it always sorts by `DisruptionCost` then interweaves by NodePool. This means single-node candidates aren't sorted by savings ratio even when Balanced is active. The scoring still works (each candidate is evaluated independently), but the evaluation order could be suboptimal — high-value candidates might be evaluated late and hit the timeout.

- [ ] **CODE-2: Duplicate scoring in multi-node consolidation**
  In `firstNConsolidationOption`, balanced scoring is applied inside the binary search loop (lines ~169-182). Then after the loop, `EmitBalancedMultiNodeEvents` re-scores the final command (line ~107). This double-scores the winning command. The second call is for events/metrics only, but it recomputes `ScoreMove` which is redundant. Consider passing the result from the binary search to the event emitter.

- [ ] **CODE-3: `computeNodePoolTotals` called on every `GetCandidates` invocation including validation**
  `GetCandidates` is called during both initial candidate discovery and validation. The validation path also computes `NodePoolTotals` but discards them (the `_` in `validatedCandidates, _, err := GetCandidates(...)`). This is wasted work. Consider either caching totals or having a separate `GetCandidatesWithoutTotals` for validation.

- [ ] **CODE-4: Event message format uses "consolidationThreshold" but design doc says "k"**
  The event message format is `"score %.2f >= threshold %.2f (consolidationThreshold: %d, savings %.1f%%, disruption %.1f%%)"`. The design doc uses `k` terminology. Minor inconsistency but operators reading events alongside the design doc might be confused. Consider using `k` in the event message or documenting the mapping.

### Test Coverage

- [ ] **TEST-1: No end-to-end integration tests with the full controller loop**
  The `balanced_integration_test.go` file tests the scoring functions in isolation with manually constructed candidates. There are no tests that exercise the full `ComputeCommands` → `EvaluateBalancedMove` → `Validate` flow with a real cluster state. The existing `consolidation_test.go` and `emptiness_test.go` changes are just signature updates. Consider adding at least one test that creates a Balanced NodePool and verifies the full single-node and multi-node consolidation paths.

- [ ] **TEST-2: No test for the feature gate fallback behavior**
  The design doc specifies: "If an operator enables BalancedConsolidation, sets consolidationPolicy: Balanced, then disables the feature gate during rollback, the controller falls back to WhenEmptyOrUnderutilized behavior." The `ShouldDisrupt` implementation rejects Balanced candidates when the gate is off (returning false), which means they won't consolidate at all — not fall back to WhenEmptyOrUnderutilized. This is a behavioral difference from the design doc. The test `TestShouldDisrupt_FeatureGateDisabled_RejectsBalanced` confirms the current behavior but doesn't test the fallback.

- [ ] **TEST-3: No test for `ConsolidationThreshold` values 1 and 3**
  Tests use `int32Ptr(2)` and `int32Ptr(1)` but don't verify that k=3 opens additional cross-family replace pairs as described in the design doc. Add a test showing a move that passes at k=3 but fails at k=2.

### Minor / Nits

- [ ] **NIT-1: `approxEqual` helper defined in both `balanced_test.go` and `balanced_integration_test.go`**
  Consider extracting to a shared test helper.

- [ ] **NIT-2: `candidatePrice` in `consolidation.go` duplicates logic from `balanced.go`**
  `getCandidatePrices` in `consolidation.go` and `candidatePrice` in `balanced.go` both compute offering prices. They have slightly different semantics (batch vs single, early-return-on-zero vs not). Consider unifying or at least having `getCandidatePrices` call `candidatePrice`.

- [ ] **NIT-3: Missing copyright header check for `balanced.go` and `balanced_integration_test.go`**
  Both new files have the standard Apache 2.0 header — this is fine. Just noting for completeness.

---

## Summary

The implementation is well-structured and closely follows the design doc. The scoring formula, per-NodePool normalization, cross-NodePool handling, feature gate, observability (events + metrics), and candidate filtering are all implemented correctly. The code is clean and well-documented.

Key areas to address before merge:
1. Reconcile the API design (IntOrString vs separate field) with the design doc
2. Verify the feature gate fallback behavior matches the design doc's intent
3. Add full-loop integration tests
4. Consider the single-node sort order for Balanced candidates
5. Address the `getCandidatePrices` batch-zero issue for multi-node moves
