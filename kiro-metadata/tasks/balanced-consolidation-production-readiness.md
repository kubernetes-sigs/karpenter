# Balanced Consolidation — Production Readiness Checklist

## Phase 1: Code Complete (Pre-CR)

### Must Fix
- [ ] Resolve API design: IntOrString `consolidationPolicy` (design doc) vs separate `consolidationThreshold` field (implementation). Update whichever is wrong.
- [ ] Fix feature gate fallback: currently Balanced candidates get zero consolidation when gate is off. Design doc says fall back to `WhenEmptyOrUnderutilized`. Decide and implement.
- [ ] Fix `getCandidatePrices` returning 0 for entire multi-node batch when one candidate has missing offerings.
- [ ] Verify `ConsolidationPolicyUnsupported` status condition is persisted to the API server (not just in-memory).

### CEL / Webhook Validation
- [ ] Add CEL validation tests for `consolidationPolicy: Balanced` in `nodepool_validation_cel_test.go`.
- [ ] Verify CRD validation rejects `consolidationThreshold` outside 1-3 range (kubebuilder markers handle this, but add a CEL test).
- [ ] Verify CRD validation rejects `consolidationThreshold` when policy is not Balanced (XValidation rule exists, add CEL test).

### Test Coverage
- [ ] Full-loop integration test: single-node Balanced consolidation with cluster state (ComputeCommands → score → validate → execute).
- [ ] Full-loop integration test: multi-node Balanced consolidation with cluster state.
- [ ] Test k=1 (deletes only, no replaces in uniform pools).
- [ ] Test k=3 (cross-family replaces that fail at k=2 but pass at k=3).
- [ ] Test feature gate disabled with Balanced policy configured (verify fallback behavior).
- [ ] Test cross-NodePool move where one pool is Balanced and one is WhenEmptyOrUnderutilized.
- [ ] Test empty node with Balanced policy (should always pass — per-node base cost only).
- [ ] Test single-node pool DELETE (score should be exactly 1.0).
- [ ] Test near-zero-cost nodes (ODCR pricing — divisor should cancel in score).
- [ ] Test `consolidateAfter` interaction: non-candidate nodes still contribute to denominators.

### Code Cleanup
- [ ] Remove double scoring in multi-node path (pass result from binary search to event emitter).
- [ ] Avoid wasted `computeNodePoolTotals` in validation `GetCandidates` calls.
- [ ] Unify `getCandidatePrices` and `candidatePrice` to reduce duplication.
- [ ] Extract shared `approxEqual` test helper.

---

## Phase 2: Helm / Config

- [ ] Add `balancedConsolidation` to `kwok/charts/values.yaml` under `featureGates` (currently missing — only `spotToSpotConsolidation` and `nodeRepair` are listed).
- [ ] Verify the feature gate string in `options.go` default matches the Helm chart default (`BalancedConsolidation=false`).
- [ ] If the provider-aws chart has its own values.yaml, add the gate there too.

---

## Phase 3: Documentation

- [ ] Update design doc to match final API (IntOrString vs separate field decision).
- [ ] Write user-facing docs: what Balanced does, how to enable, how to tune k, what events/metrics to watch.
- [ ] Document the scoring formula with examples (can pull from the design doc).
- [ ] Document the `ConsolidationPolicyUnsupported` status condition and what to do when you see it.
- [ ] Add troubleshooting guide: "I enabled Balanced but nothing is consolidating" (check feature gate, check events, check score histogram).
- [ ] Add migration guide: moving from `WhenEmptyOrUnderutilized` to `Balanced`.
- [ ] Update CHANGELOG / release notes with the new feature.

---

## Phase 4: Observability & Operational Readiness

### Metrics
- [ ] Verify `karpenter_consolidation_score` histogram works end-to-end (scrape in Prometheus, check buckets).
- [ ] Verify `karpenter_consolidation_moves_total` counter increments correctly for approved/rejected.
- [ ] Create sample Grafana dashboard or queries for operators:
  - Score distribution by NodePool (are most moves near the threshold?).
  - Approved vs rejected rate over time.
  - Correlation between score threshold and actual savings.

### Events
- [ ] Verify `ConsolidationApproved` and `ConsolidationRejected` events appear in `kubectl get events`.
- [ ] Verify multi-node events emit on the NodePool (not individual nodes).
- [ ] Verify single-node events emit on both Node and NodeClaim.
- [ ] Verify event deduplication works (same node shouldn't spam events).

### Logging
- [ ] Verify DEBUG-level scoring logs include all fields (nodepool, score, savings, disruption, threshold, decision, candidates).
- [ ] Verify no sensitive data leaks in logs.

---

## Phase 5: Testing in Staging / Pre-prod

### Functional Validation
- [ ] Deploy with `BalancedConsolidation=true` and `consolidationPolicy: Balanced` on a test cluster.
- [ ] Verify marginal moves are rejected (high-utilization nodes with small savings).
- [ ] Verify high-value moves are approved (oversized nodes, empty nodes).
- [ ] Verify same-type REPLACE scores 0 and is rejected (breaks cycling loops).
- [ ] Verify cross-NodePool consolidation works (on-demand source, spot destination).
- [ ] Verify `consolidateAfter` cooldown prevents rapid re-consolidation of destination nodes.

### Rollback Testing
- [ ] Enable Balanced, consolidate some nodes, then disable the feature gate. Verify:
  - Controller doesn't crash.
  - `ConsolidationPolicyUnsupported` condition appears on affected NodePools.
  - Consolidation behavior matches the decided fallback (zero or WhenEmptyOrUnderutilized).
- [ ] Re-enable the gate. Verify condition clears and Balanced scoring resumes.

### Performance / Scale
- [ ] Run with 100+ node cluster. Verify `computeNodePoolTotals` doesn't add measurable latency to the disruption loop.
- [ ] Verify multi-node binary search with Balanced scoring completes within the 1-minute timeout.
- [ ] Verify single-node consolidation with Balanced scoring completes within the 3-minute timeout on large clusters.
- [ ] Check memory footprint: `nodePoolTotals` map is small but verify no leaks across reconcile cycles.

### Cycling / Churn Validation
- [ ] Run a heterogeneous workload (mixed instance types, mixed pod costs) for 24+ hours with Balanced enabled.
- [ ] Verify no consolidation cycling loops (same nodes being replaced repeatedly).
- [ ] Compare consolidation event rate vs `WhenEmptyOrUnderutilized` — Balanced should have fewer total moves with similar or better savings.

---

## Phase 6: GA Graduation (Future)

- [ ] Collect alpha feedback from early adopters.
- [ ] Decide whether to change the feature gate default to `true` (beta).
- [ ] Decide whether `Balanced` should become the default `consolidationPolicy`.
- [ ] Remove the feature gate (GA).
- [ ] Consider exposing the scoring formula or threshold as a more flexible API if operators request it.
