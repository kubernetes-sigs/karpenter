# **Node Resources Fit Scoring Awareness**

## Summary

Let Karpenter's scheduling simulation try existing nodes in the order kube-scheduler's `NodeResourcesFit` scoring strategy (`LeastAllocated` or `MostAllocated`) ranks them, so that the node Karpenter predicts a pod lands on is the node kube-scheduler actually binds it to. The setting is a new `nodeResourcesFit.scoringStrategy` field on the existing `SCHEDULER_CONFIG` value introduced by [Default Pod Topology Spread Awareness](default-topology-spread-awareness.md). When it's unset, behavior is unchanged.

## Motivation

When Karpenter simulates scheduling, during provisioning and during every consolidation and drift decision, it places each pod on the **first** existing node the pod fits on. Existing nodes are ordered with initialized nodes first and then by node name (`sortExistingNodes` in `pkg/controllers/provisioning/scheduling/scheduler.go`). Name order has nothing to do with how kube-scheduler picks a node.

kube-scheduler scores every feasible node and binds the pod to the highest scoring one. For resource fit, its `NodeResourcesFit` plugin uses `LeastAllocated` by default, which prefers the emptiest node, or `MostAllocated` when an operator configures bin-packing. EKS now exposes this setting for managed control planes, so more clusters run `MostAllocated`.

Because the two orders differ, Karpenter's simulation and the real placement diverge:

- **Consolidation plans fall apart.** Karpenter decides a node can be removed because its pods fit on, for example, `node-a` and `node-b`. After the eviction, kube-scheduler puts them on the nodes its strategy prefers. Those can be nodes Karpenter planned to consolidate next, or the pods may not all fit, so a replacement node is launched. Both cause pod churn that the decision didn't account for ([#1228](https://github.com/kubernetes-sigs/karpenter/issues/1228)).
- **Provisioning predictions are off.** With several pending pods, Karpenter assigns them to existing nodes in name order and launches capacity only for the rest. kube-scheduler places them differently, so a pod Karpenter expected to fit can stay `Pending` until a later provisioning loop.
- **CapacityBuffers land in the wrong place.** Buffer virtual pods are placed with the same simulation, so headroom is reserved on different nodes than the ones the real scheduler fills ([#3196](https://github.com/kubernetes-sigs/karpenter/issues/3196)).

### Use Cases

1. **Bin-packing clusters.** An operator runs kube-scheduler with `MostAllocated` so pods stack onto full nodes and other nodes drain empty. They want Karpenter's consolidation to assume the same stacking.
2. **Spreading clusters.** An operator keeps the default `LeastAllocated` and runs CapacityBuffers or consolidation-heavy NodePools. They want Karpenter to predict that pods spread out, not pile onto the alphabetically first node.

### Non-Goals

- **`RequestedToCapacityRatio`.** It's rejected at startup. It can be added as another type later.
- **Other scoring plugins.** `NodeResourcesBalancedAllocation`, `InterPodAffinity`, `PodTopologySpread` scoring, `TaintToleration`, `ImageLocality` and the plugin weights that blend them are not mirrored. Resource fit is the score that most directly decides packing, and it's the one operators configure.
- **New NodeClaims.** The order in which pods are added to NodeClaims Karpenter is about to launch is unchanged. kube-scheduler can't see those nodes yet, so there's nothing to mirror, and changing it would change instance selection.

## Proposal

Add an optional `nodeResourcesFit` section to `SchedulerConfiguration`. When it contains a `scoringStrategy`, Karpenter scores every existing node with the same formula kube-scheduler uses and tries nodes in descending score order instead of name order.

### Proposed Spec

```yaml
# SCHEDULER_CONFIG / --scheduler-config
nodeResourcesFit:
  scoringStrategy:
    type: MostAllocated        # or LeastAllocated
    resources:                 # optional; defaults to cpu and memory with weight 1, as in kube-scheduler
      - name: cpu
        weight: 1
      - name: memory
        weight: 1
```

The fragment is the same shape as `profiles[].pluginConfig[name: NodeResourcesFit].args.scoringStrategy` in a `KubeSchedulerConfiguration`, so it can be copied across. It can be combined with `podTopologySpread` in the same document.

```go
type SchedulerConfiguration struct {
    PodTopologySpread *PodTopologySpreadConfig `json:"podTopologySpread,omitempty"`
    NodeResourcesFit  *NodeResourcesFitConfig  `json:"nodeResourcesFit,omitempty"`
}

type NodeResourcesFitConfig struct {
    ScoringStrategy *ScoringStrategy `json:"scoringStrategy,omitempty"`
}

type ScoringStrategy struct {
    Type      ScoringStrategyType `json:"type"` // LeastAllocated | MostAllocated
    Resources []ResourceSpec      `json:"resources,omitempty"`
}

type ResourceSpec struct {
    Name   string `json:"name"`
    Weight int64  `json:"weight,omitempty"`
}
```

Validation and defaulting mirror kube-scheduler's `ValidateNodeResourcesFitArgs` and `SetDefaults_NodeResourcesFitArgs`. The type must be `LeastAllocated` or `MostAllocated`, and a weight must be in `[0, 100]`. An empty resource list becomes cpu and memory with weight 1, and a weight of 0 becomes 1. An invalid value fails operator startup, like the rest of `SCHEDULER_CONFIG`.

### How It Works

- **Score.** For each scored resource, `requested` is the node's allocatable minus what the scheduler still considers available, so it includes bound pods, pods already simulated onto the node and expected daemonset pods. The score is `requested * 100 / allocatable` for `MostAllocated` and `(allocatable - requested) * 100 / allocatable` for `LeastAllocated`, combined as a weighted average with kube-scheduler's integer arithmetic. CPU is counted in millicores, other resources in their base unit.
- **Order.** Nodes are sorted once, when the scheduler is built: initialized nodes first (unchanged), then by descending score, then by name for ties.
- **Re-rank.** After a pod is simulated onto a node, only that node is re-scored and moved to its new position. That costs O(n) per placement instead of a full sort. kube-scheduler scores each pod against current state, so without this step a batch of pods would all be tried against a stale order.
- **Placement is still first fit.** `CanAdd` is still evaluated in order and the first node the pod fits on wins. Walking nodes in descending score order means the first feasible node is the highest scoring feasible node, without having to evaluate every node for every pod.

### Interaction with Existing Features

- **Consolidation and drift** use the same `NewScheduler` and pick up the ordering automatically. Nodes under `consolidateAfter` are still skipped as candidate destinations exactly as before.
- **Initialized-first ordering** is preserved: score only orders nodes within the initialized and uninitialized groups.
- **Default topology spread constraints** are independent. Topology filtering still decides which nodes a pod can go on, and score only decides the order in which they're tried.
- **CapacityBuffers** use the same simulation, so buffer pods are placed according to the configured strategy.

### Observability

When the strategy is set, the operator logs it once at startup, next to the existing log line for default topology spread constraints. Placement decisions themselves are unchanged in shape: pods are nominated to, or reported as fitting on, existing nodes as they are today.

### Edge Cases

- **Score without the pod.** kube-scheduler scores a node with the incoming pod's requests included, while Karpenter scores without them, so one ordering serves every pod. On nodes of the same size the two orders are identical. On nodes of different sizes, a large pod can rank two nodes in a different order than kube-scheduler would.
- **Extended resources.** kube-scheduler scores extended and other scalar resources only for pods that request them. Because the order doesn't depend on the pod, only cpu, memory and ephemeral-storage are scored, and other configured resources are ignored.
- **Pods without requests.** kube-scheduler counts a pod with no cpu or memory request as 100m / 200Mi when scoring. Karpenter uses actual requests, so nodes full of request-less pods score lower than they would in kube-scheduler.
- **Multiple scheduler profiles.** Like `podTopologySpread`, this mirrors one cluster-wide strategy. Clusters whose profiles use different strategies should configure the one most of their pods use.

## Alternatives Considered

### Alternative 1: Score every feasible node per pod

Evaluate `CanAdd` on every existing node for every pod and pick the highest score, including the pod's own requests. This would match kube-scheduler exactly for resource fit. It was rejected because `CanAdd` is the expensive part of the simulation (requirements, topology, volumes, DRA), and today the loop stops at the first node that fits. Sorting keeps that early exit.

### Alternative 2: A dedicated flag

Add something like `--placement-strategy=MostAllocated|LeastAllocated`. It was rejected because `SCHEDULER_CONFIG` was introduced specifically so scheduler-mirroring settings grow as fields rather than as new flags, and because a flag can't carry resource weights in the scheduler's own shape.

### Alternative 3: Default to a strategy

Default to `LeastAllocated` (kube-scheduler's default) or `MostAllocated`. It was rejected because either default changes placement for every existing user on upgrade. Leaving it unset keeps today's name order, and the operator states what their kube-scheduler actually runs.
