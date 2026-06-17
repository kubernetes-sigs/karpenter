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

## Testing

```bash
go test ./pkg/controllers/pod/deletioncost/...
```
