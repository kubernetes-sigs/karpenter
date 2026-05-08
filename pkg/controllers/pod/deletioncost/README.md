# Pod Deletion Cost Controller

Automatically manages the `controller.kubernetes.io/pod-deletion-cost` annotation on pods
to influence Kubernetes' pod selection during scale-in events.

## Overview

When enabled via the `PodDeletionCostManagement` feature gate, this controller:

1. Ranks Karpenter-managed nodes using **PodCount** strategy with three-tier drift partitioning
2. Assigns deletion cost annotations to pods based on their node's rank
3. Protects customer-set annotations from being overwritten
4. Detects third-party annotation conflicts and releases management
5. Bounds annotation updates to the top 50 nodes per cycle with cleanup

## Ranking: Three-Tier Drift Partitioning

Nodes are partitioned into three tiers, each sorted by pod count ascending:

| Tier | Nodes | Deletion Cost |
|------|-------|---------------|
| 1 (lowest) | Drifted nodes | Deleted first |
| 2 (middle) | Normal nodes | Deleted second |
| 3 (highest) | Do-not-disrupt nodes | Deleted last |

Ranks start at `-len(nodes)` and increment sequentially across tiers.

## Components

- **RankingEngine** (`ranking.go`) — PodCount-based ranking with three-tier partitioning
- **AnnotationManager** (`annotation.go`) — Safe pod annotation updates with third-party conflict detection
- **ChangeDetector** — Uses `ConsolidationState` timestamp from `state.Cluster` to skip ranking when cluster state hasn't changed (O(1) comparison, zero API calls)
- **Controller** (`controller.go`) — Orchestrates ranking, bounded labeling, and cleanup

## Configuration

### Feature Gate
```
--feature-gates=PodDeletionCostManagement=true
```

## Annotations

| Annotation | Purpose |
|-----------|---------|
| `controller.kubernetes.io/pod-deletion-cost` | Kubernetes deletion priority (lower = deleted first) |
| `karpenter.sh/managed-deletion-cost` | Sentinel: Karpenter manages this pod's cost |

## Customer Annotation Protection

Pods with an existing `pod-deletion-cost` annotation but **without** the Karpenter sentinel
are considered customer-managed and will not be modified.

## Third-Party Conflict Detection

If a third party modifies a Karpenter-managed pod's deletion cost annotation:
- Karpenter detects the value differs from what it last set
- Removes the sentinel annotation (releases management)
- Skips the pod on future reconciles
- Emits a `PodDeletionCostThirdPartyConflict` warning event

## Consolidation Priority Migration

With this controller auto-managing `pod-deletion-cost` for RS coordination, users who
previously set `pod-deletion-cost` to steer consolidation behavior should migrate to:

```yaml
annotations:
  karpenter.sh/consolidation-priority: "2147483647"  # high = expensive to disrupt
```

The consolidation scoring path (`EvictionCost`) applies this precedence:
1. `karpenter.sh/consolidation-priority` — if set, used directly
2. `controller.kubernetes.io/pod-deletion-cost` — used only if NOT auto-managed (no sentinel)
3. Default cost of 1.0

Auto-managed pods (those with `karpenter.sh/managed-deletion-cost: "true"`) have their
`pod-deletion-cost` ignored for consolidation scoring since it reflects RS coordination
ranking, not user intent about disruption cost.

## Bounded Node Labeling

Each reconcile cycle annotates at most **50 nodes**. When nodes drop out of the
top-50 set, their pod annotations are cleaned up automatically.

## Testing

```bash
go test ./pkg/controllers/pod/deletioncost/...
```

25 tests covering ranking, annotation management, change detection, third-party
conflict detection, bounded labeling, and controller reconciliation.
