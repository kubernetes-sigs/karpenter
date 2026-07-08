# Pod Deletion Cost Controller

Manages the upstream `controller.kubernetes.io/pod-deletion-cost` annotation on pods
running on Karpenter-managed nodes so that the ReplicaSet controller's scale-down
ordering aligns with Karpenter's consolidation ranking.

Design RFC: [kubernetes-sigs/karpenter#2935](https://github.com/kubernetes-sigs/karpenter/pull/2935).

## Overview

When enabled via the `PodDeletionCostManagement` feature gate, this controller:

1. Ranks Karpenter-managed nodes into four tiers (Group A draining, Group B drifted,
   Group C disruptable, Group D not-disruptable).
2. Writes `controller.kubernetes.io/pod-deletion-cost` on pods on Groups A/B/C nodes
   so the ReplicaSet controller targets them first during scale-in.
3. Clears the annotation on pods on Group D nodes so the ReplicaSet controller applies
   its default scale-in heuristic for those pods.
4. Bounds annotation updates to the top 50 nodes per cycle.

## Ranking: Four-Tier Partitioning

Nodes are partitioned into four groups, each sorted by pod count ascending:

| Group | Nodes | Deletion Cost |
|-------|-------|---------------|
| A (floor) | Disrupted + PDB-blocked | `math.MinInt32` (excluded from budget) |
| B (lowest) | Drifted nodes | Sequential rank, deleted first |
| C (middle) | Normal nodes | Sequential rank, deleted second |
| D (cleared) | Do-not-disrupt nodes | Annotations cleared, not ranked |

Groups B and C receive sequential ranks starting at `-len(B+C+D)`.
Group A nodes get `math.MinInt32` and do not count against the annotation budget.
Group D nodes have any pod-deletion-cost annotation removed.

## Components

- **`RankingEngine`** (`ranking.go`) — PodCount-based ranking with four-tier
  partitioning. Pre-fetches per-node pod lists and PDB selectors once per
  reconcile so the per-node loop is allocation-free.
- **`AnnotationManager`** (`annotation.go`) — Pod annotation updates via
  optimistic-lock merge patches, with no-op skipping when the desired value
  already matches and aggregated `errors.Join` for partial failures.
- **`Controller`** (`controller.go`) — Orchestrates ranking and bounded
  labeling. Uses the `state.Cluster` `ConsolidationState` timestamp to
  short-circuit when nothing has changed since the last reconcile (O(1)
  comparison, zero API calls).

## Configuration

### Feature Gate

```
--feature-gates=PodDeletionCostManagement=true
```

The gate defaults to `false`. Registration is conditional: when the gate is
off at process start, the controller is not registered and nothing in this
package runs. The `Reconcile` method also checks the gate on every loop and
returns early if it has been flipped to false at runtime, providing a
defensive shutoff path that doesn't require a process restart.

### Graduation criteria

This is an alpha feature, opt-in by default. Promotion to beta default-on
will require:

- At least one minor release of soak with the gate on in a non-trivial
  cluster.
- No outstanding correctness issues on the [upstream pod-deletion-cost
  semantic](https://kubernetes.io/docs/concepts/workloads/controllers/replicaset/#pod-deletion-cost).
- Documented operator guidance for migrating consumers of
  `controller.kubernetes.io/pod-deletion-cost` to
  `karpenter.sh/disruption-cost` (see Annotations below).

If the feature does not graduate after two minor releases the gate will be
revisited.

### Gate-flip behavior

- Gate **OFF → ON**: the controller starts ranking on its next reconcile and
  begins overwriting any existing `pod-deletion-cost` values on managed pods.
- Gate **ON → OFF**: the controller stops touching the annotation. Existing
  values that the controller wrote remain on pods until something else clears
  them. There is no automatic cleanup of stale values when the gate is
  turned off; operators who need a clean-slate can drop the annotation via
  `kubectl annotate pods --all`.

## Annotations

| Annotation | Purpose |
|-----------|---------|
| `controller.kubernetes.io/pod-deletion-cost` | Upstream Kubernetes annotation. Consumed by the ReplicaSet controller for scale-down ordering. Written by this controller when the gate is on. |
| `karpenter.sh/disruption-cost` | User-facing Karpenter annotation for steering consolidation. Consumed by Karpenter's consolidation scoring. Not consumed by the ReplicaSet controller. |

## Consolidation Steering

Customers steering Karpenter consolidation should use `karpenter.sh/disruption-cost`:

```yaml
annotations:
  karpenter.sh/disruption-cost: "2147483647"  # high = expensive to disrupt
```

Karpenter's consolidation scoring (`EvictionCost` in `pkg/utils/disruption`) reads the
new annotation depending on the gate:

- **Gate ON:** consolidation scoring reads only `karpenter.sh/disruption-cost`.
  The controller manages `controller.kubernetes.io/pod-deletion-cost` for RS
  coordination, and those values are not interpreted as consolidation cost.
- **Gate OFF:** consolidation scoring reads `karpenter.sh/disruption-cost`
  first; if absent, falls back to
  `controller.kubernetes.io/pod-deletion-cost`. This preserves current
  behavior for customers who have not migrated.

## Controller interactions

The deletion-cost controller writes pod annotations; the disruption controller
reads pod annotations via `EvictionCost`. The two run independently and are
not synchronized.

This is intentional: consolidation re-evaluates on every state change, so a
stale annotation only delays optimal scale-down by at most one reconcile
cycle. The race window between a new node entering the cluster and the
deletion-cost controller writing its annotation is bounded by
`reconcileInterval` (60 s) plus the cluster's controller lag. We trade
strong consistency for a much smaller code footprint and avoid taking a
direct dependency on the disruption controller's internal state.

## Bounded Node Labeling

Each reconcile cycle annotates at most **50 nodes**. When nodes drop out of the
top-50 set, their pod annotations are cleaned up automatically on the next
cycle.

## Metric cardinality and worst-case emit rate

Metrics exported by this controller are bounded and small:

- `nodes_ranked` gauge, no labels.
- `reconcile_skipped_total` counter, no labels.
- `pods_updated_total` counter with one label `result` taking three values
  (`updated`, `skipped_unchanged`, `error`) — cardinality 3. The `error`
  value counts per-pod patch failures only.
- `nodes_errored_total` counter, no labels. Counts nodes whose pod-list
  fetch failed and were therefore skipped entirely. Kept separate from
  `pods_updated_total{result=error}` so operators can distinguish a flaky
  apiserver-list path from a flaky per-pod patch path.
- `ranking_duration_seconds` and `annotation_duration_seconds` histograms,
  no labels.

Worst-case emit rate on `pods_updated_total`:

```
(maxNodesPerCycle nodes per reconcile) * (avg pods per node) / reconcileInterval * (3 result-label values)
= 50 * 30 / 60s * 3
~= 75 increments per second
```

For a cluster with 30 pods per node the steady-state emit rate is well within
single-digit overhead. There is no per-pod metric, so cardinality does not
grow with cluster size.

## Feature-gate rationale

The `PodDeletionCostManagement` gate is provisional and is the only opt-in.
We did not add a NodePool-level API field for three reasons:

- The behavior is cluster-wide (the controller writes the same annotation on
  every managed pod regardless of NodePool), so a NodePool field would not
  express the operator's intent.
- Graduating from gate to default-on requires the soak window described in
  Graduation Criteria. An API field would be permanent; the gate is not.
- Customers who want to disable on a single NodePool can use
  `consolidateAfter: Never`, which already routes every node on that pool to
  Group D so the controller clears the annotation.

If the feature graduates to default-on the gate will be removed, not promoted
to an API field.

## Per-pool budget scoping

The per-NodePool disruption budget computed here is enforced **per NodePool**,
not across the cluster. Operators who run multiple NodePools as a logical-or
fall-through pool (e.g. one Spot pool plus one On-Demand pool serving the same
workload) will see the annotation budget applied independently to each pool.
This matches the existing semantics of `disruption.BuildDisruptionBudgetMapping`,
so consolidation and deletion-cost ranking stay in sync, but operators with
that topology should expect both pools to be ranked simultaneously rather than
one drained-then-the-other.

## Migration: `karpenter.sh/disruption-cost`

`karpenter.sh/disruption-cost` is a new public annotation key introduced by
this PR (see `pkg/utils/disruption/disruption.go`). It did not exist in any
prior Karpenter release. Customers who have not previously set an annotation
of that exact key are unaffected. Customers who happen to have set it for
other purposes will see their value parsed as a consolidation cost on upgrade;
operators are expected to audit cluster YAML for the key before enabling the
gate.

The gate-OFF read path (`EvictionCost`) reads the new annotation first and
falls back to `controller.kubernetes.io/pod-deletion-cost` if absent. That
fallback preserves the prior consolidation behavior. The gate-ON read path
skips the fallback entirely so the controller's writes do not feed back into
the consolidation scorer.

## Testing

```bash
go test ./pkg/controllers/pod/deletioncost/...
```

The test suite mixes direct-call unit tests against `RankNodes` and
`UpdatePodDeletionCosts` (`ranking_test.go`, `annotation_test.go`) with
end-to-end tests that drive the outermost `Controller.Reconcile`
(`controller_test.go`). The direct-call tests cover the four-tier
partitioning algorithm, the per-pool budget arithmetic, and the patch
classification logic; the reconcile-level tests cover the gate-check,
state-change short-circuit, and 50-node cap. New behavior changes must add at
least one reconcile-level test so the integration path is exercised, not just
the algorithm.
