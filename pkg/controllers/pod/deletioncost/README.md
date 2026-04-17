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
- **ChangeDetector** (`changedetector.go`) — Hash-based optimization to skip unchanged state
- **Controller** (`controller.go`) — Orchestrates ranking, bounded labeling, and cleanup

## Configuration

### Feature Gate
```
--feature-gates=PodDeletionCostManagement=true
```

### Environment Variables
```bash
POD_DELETION_COST_CHANGE_DETECTION=true  # Enable hash-based change detection (default: true)
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

## Bounded Node Labeling

Each reconcile cycle annotates at most **50 nodes**. When nodes drop out of the
top-50 set, their pod annotations are cleaned up automatically.

## Testing

```bash
go test ./pkg/controllers/pod/deletioncost/...
```

25 tests covering ranking, annotation management, change detection, third-party
conflict detection, bounded labeling, and controller reconciliation.
