# Karpenter Changes Needed for Sim Verification: Next Round

**Bead:** k8s-y8od
**Date:** 2026-03-25
**Depends on:** k8s-9kuc (verification report), k8s-f6jd (gap analysis), k8s-l0as (re-run results)

---

## 1. What the Sim Verification Proved and What It Couldn't

### Proved

1. **Simulator policy ordering is directionally correct.** The simulator predicts
   CostJustified variants cost less than WhenEmpty, and KWOK provisioning data
   shows the same directional relationship (1 node vs 14 nodes for CostJustified
   vs WhenEmpty/Underutilized), even though absolute magnitudes differ due to
   infrastructure issues.

2. **Simulator tradeoff gradient is internally consistent.** The benchmark-tradeoff-kwok
   analysis (20 runs × 10 variants) shows the expected behavior:
   - Thresholds 0.25–1.00: identical aggressive consolidation (54.3 disruptions, 15.3% savings)
   - Threshold 1.50: knee point (40.9 disruptions, 6.1% savings)
   - Thresholds 3.00–5.00: minimal disruption (1.9–2.0 disruptions, 3.4–3.5% savings)
   - WhenEmpty baseline: 0 disruptions, 0% savings
   - WhenEmptyOrUnderutilized: most disruptive (438.2 disruptions, 14.3% savings)

3. **WhenEmpty and WhenEmptyOrUnderutilized behave as expected in KWOK** (when pods
   schedule). The re-run (k8s-l0as) with fixed KWOK environment showed these legacy
   policies producing real consolidation events with `Empty/` and `Underutilized/`
   disruption paths.

### Could Not Prove

1. **WhenCostJustifiesDisruption behavior.** The Karpenter build from PR #2893 has
   the CRD schema for `consolidateWhen` and `decisionRatioThreshold` (fields accepted
   without validation errors), but the disruption controller does not read them. All
   cost-justified variants fall back to legacy `WhenEmptyOrUnderutilized` behavior.
   Zero `CostJustified/` disruption paths or `decision.ratio` log entries were observed.

2. **Threshold-driven consolidation gradient.** Varying `decisionRatioThreshold` from
   0.25 to 5.00 should produce a smooth gradient. Instead, KWOK results cluster into
   two groups matching legacy policies (low thresholds → Underutilized behavior, high
   thresholds → Empty behavior). The transition is a fallback artifact, not
   threshold-driven.

3. **Simulator calibration against real Karpenter.** Without a working
   `WhenCostJustifiesDisruption` implementation, the simulator's `find_cost_justified_nodes()`
   and `decision_ratio()` functions cannot be validated against real behavior.

---

## 2. Karpenter PR #2893: What Needs to Change

PR #2893 currently provides CRD schema only. The disruption controller must be
modified to read and act on the new fields.

### 2.1 Controller Must Read `consolidateWhen`

**Current state:** The disruption controller reads `consolidationPolicy` (legacy
field: `WhenEmpty` | `WhenEmptyOrUnderutilized`). When `consolidateWhen` is set
instead, the controller ignores it and falls back to default behavior.

**Required change:** The disruption controller's consolidation evaluation loop must:
1. Check for `consolidateWhen` field on the NodePool spec
2. If present, use it instead of `consolidationPolicy`
3. When `consolidateWhen: WhenCostJustifiesDisruption`, enter the cost-justified
   evaluation path instead of the empty/underutilized path

**Code path to modify:** The consolidation controller entry point that selects
between Empty and Underutilized evaluation. This is where the new
`WhenCostJustifiesDisruption` branch must be added.

### 2.2 Decision Ratio Evaluation Must Be Implemented

**Required:** When `consolidateWhen: WhenCostJustifiesDisruption`, the controller must:

1. For each candidate node, compute:
   ```
   decision_ratio = normalized_cost_savings / normalized_disruption_cost
   ```
   Where:
   - `normalized_cost_savings = node_cost / max_cost_in_pool` (how expensive this
     node is relative to the most expensive node in the pool)
   - `normalized_disruption_cost = pods_on_node / max_pods_in_pool` (how many pods
     would be disrupted relative to the most loaded node)

2. Compare `decision_ratio >= decisionRatioThreshold`

3. Only consolidate nodes where the ratio exceeds the threshold

This matches the simulator's implementation in
`crates/kubesim-karpenter/src/consolidation.rs::decision_ratio()` (line ~297)
and `find_cost_justified_nodes()` (line ~320).

### 2.3 Structured Logging for `decision.ratio`

**Required:** Emit structured log entries when evaluating cost-justified consolidation:

```
{"level":"info","msg":"evaluating cost-justified consolidation",
 "node":"<name>","decision.ratio":<float>,"threshold":<float>,
 "action":"consolidate|skip"}
```

And use a `CostJustified/` prefix for disruption commands (analogous to `Empty/`
and `Underutilized/`). This is essential for verification — without these log
entries, there is no way to confirm the code path is active.

### 2.4 Prometheus Metric

**Required:** Expose `karpenter_consolidation_decision_ratio` histogram so the
verification harness can observe the distribution of ratios across nodes and
confirm threshold behavior without log scraping.

### 2.5 Field Precedence

Define clear precedence when both fields are set:
- If `consolidateWhen` is set, it takes precedence over `consolidationPolicy`
- If only `consolidationPolicy` is set, legacy behavior is preserved
- If neither is set, default to `WhenEmptyOrUnderutilized` (current default)

Add a webhook validation or status condition warning when both fields are set
simultaneously.

---

## 3. KWOK Test Harness Changes

### 3.1 Node Readiness (Critical — Fixed in k8s-l0as)

The initial KWOK runs (k8s-f6jd) failed because KWOK nodes did not report `Ready`
condition or accurate `allocatable` resources. Pods stayed `Pending` throughout
all runs, making consolidation impossible.

**Status:** Fixed in the k8s-l0as re-run. The fix involved proper KWOK controller
configuration so nodes report Ready with correct allocatable resources.

**For next round:** Keep the fix and add a pod scheduling verification gate:
```bash
kubectl wait --for=condition=Ready pod -l app=workload-a --timeout=120s
```
If this gate fails, mark the variant as `infra-failure` rather than recording
misleading zero-eviction results.

### 3.2 Scale Must Match Simulator

The fast-mode runs used 50 replicas per deployment vs the simulator's 500. This
10× scale reduction changes bin-packing ratios and consolidation dynamics.

**For next round:** Use full-scale parameters:
- 500 replicas per deployment (1000 total pods)
- 35-minute scale sequence (not 2-minute fast mode)
- `consolidateAfter: 30s` (unchanged)

### 3.3 Multiple Iterations Per Variant

The KWOK runs used 1 iteration per variant. The simulator used 20 runs per
variant. Single-iteration KWOK results have unknown run-to-run variance.

**For next round:** Run each variant at least 3 times and report mean ± stddev.
This provides minimum statistical confidence for comparison against the
simulator's 20-run averages.

### 3.4 Verify Karpenter Build Contains Controller Logic

Before running the full matrix, verify the Karpenter binary contains the
cost-justified controller logic (not just CRD schema):

```bash
# Check binary for cost-justified strings
kubectl exec -n kube-system deploy/karpenter -- strings /karpenter \
  | grep -i "CostJustifies\|decision.ratio"

# Check CRD schema has the fields
kubectl get crd nodepools.karpenter.sh -o json | \
  jq '.spec.versions[].schema.openAPIV3Schema.properties.spec.properties.disruption.properties | keys'
```

If the binary check returns no matches but the CRD check shows the fields, the
build has schema-only changes and the controller logic is missing. Do not proceed
with cost-justified variants until this is resolved.

---

## 4. Karpenter Code Paths to Verify/Modify

Based on the Karpenter codebase structure (kubernetes-sigs/karpenter), the
following files are the likely locations for changes:

### 4.1 Disruption Controller — Consolidation Evaluation

**File:** `pkg/controllers/disruption/consolidation/` (or similar path in the
Karpenter repo)

This is where the controller decides which consolidation strategy to use. The
current code selects between Empty and Underutilized based on `consolidationPolicy`.
A new branch for `WhenCostJustifiesDisruption` must be added here.

**What to verify:**
- Does the controller read `spec.disruption.consolidateWhen`?
- Is there a code path that computes decision ratios?
- Is there a `CostJustified` consolidation method/type?

### 4.2 NodePool API Types

**File:** `pkg/apis/v1/nodepool_types.go` (or equivalent)

**What to verify:**
- `ConsolidateWhen` field exists in the disruption spec struct
- `DecisionRatioThreshold` field exists with appropriate validation tags
- The field is wired to the controller (not just a CRD schema addition)

### 4.3 Disruption Budgets Integration

**What to verify:**
- Cost-justified consolidation respects disruption budgets
- The `CostJustified` reason is included in per-reason budget configuration
- Budget accounting counts cost-justified disruptions correctly

### 4.4 Webhook Validation

**What to verify:**
- Webhook validates `decisionRatioThreshold` is positive
- Webhook warns or rejects when both `consolidationPolicy` and `consolidateWhen`
  are set
- Default value for `decisionRatioThreshold` is documented (simulator uses 1.0)

---

## 5. Next-Round Verification Plan

### 5.1 Prerequisites (Must Complete Before Running)

| # | Prerequisite | Verification |
|---|-------------|--------------|
| 1 | Karpenter PR #2893 has controller logic (not just CRD) | Binary strings check returns `CostJustifies` matches |
| 2 | `decision.ratio` structured logging is present | Karpenter logs show ratio entries during manual test |
| 3 | KWOK nodes report Ready with correct allocatable | `kubectl get nodes` shows Ready status |
| 4 | Pod scheduling gate passes | Pods transition Pending → Running within 120s |

### 5.2 Test Matrix

Same 10 variants as before:

| # | Variant | Config |
|---|---------|--------|
| 1 | when-empty | `consolidationPolicy: WhenEmpty` |
| 2 | when-underutilized | `consolidationPolicy: WhenEmptyOrUnderutilized` |
| 3–10 | cost-justified-{0.25,0.50,0.75,1.00,1.50,2.00,3.00,5.00} | `consolidateWhen: WhenCostJustifiesDisruption`, `decisionRatioThreshold: <value>` |

**Parameters:**
- 500 replicas per deployment (2 deployments, 1000 total pods)
- Scale sequence: 1 → 500 → 350 → 10 over 35 minutes
- `consolidateAfter: 30s`
- 3 iterations per variant (minimum)
- Instance types: `m-4x-amd64-linux`, `m-8x-amd64-linux` (KWOK types, matching simulator)

### 5.3 Acceptance Criteria

**Structural criteria (must all pass):**

| Criterion | Test | Threshold |
|-----------|------|-----------|
| Policy ordering preserved | Spearman rank correlation of cost across variants | ρ ≥ 0.85 |
| Disruption monotonicity | Higher threshold → fewer or equal evictions | Monotonic (allowing ±1 ties) |
| WhenEmpty zero-disruption | WhenEmpty variant eviction count | = 0 |
| Inflection point agreement | Threshold where evictions drop sharply | Within ±1 step of simulator |
| WhenEmptyOrUnderutilized most disruptive | Highest eviction count among all variants | True |

**Quantitative tolerances (informational, not blocking):**

| Metric | Tolerance |
|--------|-----------|
| Node count (time-weighted) | ±15% |
| Pods evicted | ±3 absolute |
| Cumulative cost | ±10% |
| Peak node count | ±5% |

### 5.4 New Acceptance Criterion: Decision Ratio Logging

For cost-justified variants, Karpenter logs must contain:
- At least 1 `decision.ratio` log entry per consolidation cycle
- `CostJustified/` disruption path prefix (not `Empty/` or `Underutilized/`)
- Ratio values that vary with node cost and pod count

If these are absent, the test is invalid regardless of other metrics — it means
the controller is still falling back to legacy behavior.

### 5.5 Execution Sequence

1. Build Karpenter from updated PR #2893 (with controller logic)
2. Deploy to KIND+KWOK cluster
3. Run prerequisite checks (§5.1) — abort if any fail
4. Run single smoke test: `when-empty` variant (fastest, validates harness)
5. Run full 10-variant × 3-iteration matrix (~21 hours)
6. Run `scripts/compare-kwok-vs-simulator.py` to generate comparison report
7. Evaluate against acceptance criteria (§5.3, §5.4)

### 5.6 Decision Tree on Results

| Outcome | Action |
|---------|--------|
| All structural + logging criteria pass | Simulator validated for WhenCostJustifiesDisruption |
| Structural pass, quantitative outside tolerance | Tune simulator parameters (consolidation timing, bin-packing) |
| Structural fail (ordering wrong) | Investigate simulator consolidation model bug |
| Logging criteria fail (no decision.ratio entries) | PR #2893 still lacks controller logic — escalate upstream |
| Infrastructure failure (pods Pending, nodes NotReady) | Fix KWOK harness, do not record results |

---

## 6. Summary of Changes by Owner

### Karpenter PR #2893 (upstream)

- [ ] Disruption controller reads `consolidateWhen` field
- [ ] Decision ratio computation implemented in controller
- [ ] `CostJustified/` disruption path prefix in logs
- [ ] `decision.ratio` structured log entries
- [ ] `karpenter_consolidation_decision_ratio` Prometheus histogram
- [ ] Field precedence: `consolidateWhen` > `consolidationPolicy`
- [ ] Webhook validation for `decisionRatioThreshold` > 0

### KWOK Test Harness (this repo)

- [ ] Pod scheduling verification gate in orchestrator script
- [ ] Full-scale parameters (500 replicas, 35-min sequence)
- [ ] 3+ iterations per variant
- [ ] Pre-flight binary check for controller logic
- [ ] Decision ratio log verification in comparison script

### Simulator (this repo, deferred)

- [ ] Calibrate timing parameters after successful KWOK verification
- [ ] Adjust bin-packing model if quantitative tolerances exceeded
- [ ] No changes needed until Karpenter controller logic is confirmed working
