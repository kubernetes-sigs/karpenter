# Pod Deletion Cost Controller

## Summary

This RFC proposes a new feature-gated controller for Karpenter that ranks nodes by consolidation preference and propagates that ranking to pods via the `controller.kubernetes.io/pod-deletion-cost` annotation. By aligning the ReplicaSet controller's scale-down decisions with Karpenter's consolidation targets, we reduce voluntary pod disruption rates by 30-80% in benchmarks while maintaining or improving compute cost. The feature is off by default and configured entirely through feature gates and CLI flags, with no CRD changes required.

## Motivation

When Karpenter consolidates underutilized nodes, it disrupts running pods. For customer workloads, that disruption has a real cost: in-flight requests may fail, warm caches are lost, and replacement pods must re-establish connections and reload state. Depending on the workload, this recovery process can take seconds to minutes (or even hours in some cases). Teams pay for this in terms of latency, availability, and engineering complexity to handle it gracefully.

Karpenter already manages clusters toward lower compute cost, but it has limited ability to control how much disruption that right-sizing produces. Current disruption controls focus on preventing disruptions that exceed a given rate for a given nodepool or deployment regardless of the cost-savings merits of that action.

The root cause is a coordination gap between two independent controllers operating on the same cluster. The ReplicaSet controller decides which pods to delete during scale-down. Karpenter's consolidation controller decides which nodes to drain and remove. These two controllers share no information about each other's intent. Without coordination, the ReplicaSet controller uses its default pod selection heuristic during scale-in: prefer pending over ready, respect pod-deletion-cost, spread terminations across nodes, prefer newer pods, then break ties randomly. With no pod-deletion-cost set, the spreading heuristic dominates, and terminations distribute roughly evenly across nodes. This increases the entropy of the cluster: most nodes end up packed at roughly the same density, and often no single node moves meaningfully closer to empty than other nodes. The result is that Karpenter's consolidation controller finds the same unfavorable state after a pod replica scale-in that it found before, with all nodes still occupied, none empty, and utilization distribution roughly unchanged but slightly lower everywhere. This matters especially for ConsolidateWhenEmpty NodePools, where the consolidation policy requires a node to be completely free of pods before it can be removed. If the ReplicaSet controller never concentrates deletions on a single node, that condition is rarely met, and the cluster carries more nodes than necessary.

### Why node-level ranking is the right signal

The `pod-deletion-cost` annotation is the only existing communication channel between Karpenter and the ReplicaSet controller. Karpenter doesn't consolidate pods; it consolidates nodes. When it evaluates a consolidation move, the atomic unit is a node: "can I drain this entire node and either delete it or replace it with something cheaper?" If we ranked pods independently by resource usage, age, or some pod-level heuristic, the ReplicaSet controller might delete a pod from Node A (because that pod scored low individually) while leaving all other pods on Node A intact. That doesn't help Karpenter. Node A still can't be easily consolidated because it still has pods. The "hint" was spent on a decision that doesn't move the system toward an easier-to-consolidate state.

When pods inherit their node's rank, all pods on the same node share the same deletion cost. The ReplicaSet controller's deletion probability becomes uniform within a node but ordered across nodes, exactly matching the structure of Karpenter's consolidation decisions. The practical consequence is a positive feedback loop:

1. Karpenter ranks nodes by consolidation priority
2. Pods on those nodes get the lowest deletion costs
3. ReplicaSet scale-down removes pods from those nodes first
4. Those nodes become empty or closer to empty
5. Karpenter can consolidate them with less disruption (or they qualify for WhenEmpty consolidation)

This logic extends naturally to nodes that Karpenter cannot consolidate. Some pods carry the `karpenter.sh/do-not-disrupt` annotation, which tells Karpenter to leave their node alone entirely. If the ReplicaSet controller deletes pods from one of these nodes during scale-down, that deletion has zero consolidation value because Karpenter can't act on the node regardless. The ranking engine accounts for this by partitioning nodes into two groups before ranking: Group A contains nodes with no do-not-disrupt pods (normal, consolidate-able nodes), and Group B contains nodes with at least one do-not-disrupt pod (protected nodes). Group A always receives lower deletion costs than Group B, so the ReplicaSet controller removes pods from nodes Karpenter can consolidate first.

### Evidence

Community issues and production reports consistently point to excessive disruption as a pain point:

- [#2123](https://github.com/kubernetes-sigs/karpenter/issues/2123) -- Karpenter scaling disproportionately high nodes, over-provisioning then consolidating, creating unnecessary churn
- [aws/karpenter#4356](https://github.com/aws/karpenter/issues/4356) -- Constant pod disruption from Karpenter replacing nodes with cheaper ones even when savings were marginal
- [aws/karpenter#3785](https://github.com/aws/karpenter/issues/3785) -- After peak hours, consolidation was too slow, but aggressive consolidation caused massive pod churn
- [aws/karpenter#3927](https://github.com/aws/karpenter/issues/3927) -- Production clusters with 100+ pods per node couldn't consolidate without disrupting running workloads
- BMW's Karpenter migration across 375+ EKS clusters explicitly called out "excessive pod disruption" as a challenge
- re:Invent 2025 COP208 talk emphasized the need for PDB guardrails and consolidation delays because without them, Karpenter will "go crazy and interrupt your containers" during short-term usage dips

Benchmark experiments and simulations show 30-80% decreases in disruption rate when using pod deletion cost annotations. These gains compound when combined with other proposed features such as dynamically limiting node size and consolidation disruption cost thresholds. This feature also pairs with the "MostAllocated" scoring strategy for kube-scheduler.

## Goals

- Reduce voluntary pod disruption rate for clusters using Deployments/ReplicaSets by 30-80% in benchmarks
- Maintain or improve total compute cost and utilization efficiency across all benchmarks
- Minimal impact on API server load from annotation updates

### Related Proposals that can reduce the pod disruption rate

- Most important: Consolidation cost thresholding -- see draft RFC
- Limiting large node sizes for Karpenter -- internal draft RFC in progress

## Proposal

We introduce a new feature-gated controller (`pod.deletioncost`) that automatically manages the `controller.kubernetes.io/pod-deletion-cost` annotation on pods running on Karpenter-managed nodes. The controller ranks nodes using a configurable strategy, assigns sequential deletion cost values to pods so that Kubernetes' ReplicaSet scale-down logic preferentially removes pods from the "least valuable" nodes first, and partitions nodes hosting do-not-disrupt pods separately to protect them from early eviction.

### Recommended ranking strategy

We recommend ranking nodes the same way Karpenter ranks consolidation candidates: by pod count (disruption cost). This maximizes the mutual information between the hint we send and the action Karpenter will take next, because the ranking signal and the consolidation signal are literally the same function. As Karpenter's consolidation candidate ranking evolves in the future, the pod deletion cost ranking should follow those updates to maintain alignment.

The controller also supports alternative strategies (random, node-size-based, unallocated-vCPU-per-pod) for experimentation and for clusters where the default doesn't fit. But for the general case, mirroring Karpenter's own consolidation ranking is the right default.

### How it works

All new code lives in `pkg/controllers/pod/deletioncost/`. A singleton reconciler runs every 60 seconds. On each tick it:

1. Checks the `PodDeletionCostManagement` feature gate
2. Collects all Karpenter-managed nodes from `state.Cluster`
3. Runs change detection (SHA-256 hash of node/pod state). If nothing changed, skips with zero API writes
4. Partitions nodes into Group A (no do-not-disrupt pods) and Group B (has do-not-disrupt pods)
5. Ranks each group independently using the configured strategy
6. Assigns sequential integer ranks starting at -1000 for Group A, continuing for Group B (this will need to be adjusted for clusters with >1000 nodes)
7. Sets `controller.kubernetes.io/pod-deletion-cost` and `karpenter.sh/managed-deletion-cost: "true"` on each pod, skipping pods with customer-set deletion costs (two-annotation protocol)

```
┌─────────────────────────────────────────────────────────────────┐
│                 Pod Deletion Cost Controller                     │
│                   (reconciles every 60s)                         │
├─────────────────────────────────────────────────────────────────┤
│                                                                 │
│  Feature Gate ──► Enabled?                                      │
│                     │ No → return                               │
│                     │ Yes ↓                                     │
│  State Cluster ──► Collect Nodes                                │
│                     │                                           │
│  Change Detector ─► Changed?                                    │
│                     │ No → skip (emit metric) → return          │
│                     │ Yes ↓                                     │
│  ┌──────────────────────────────────────┐                       │
│  │        Ranking Engine                │                       │
│  │  1. Partition by do-not-disrupt      │                       │
│  │  2. Rank Group A (normal)            │                       │
│  │  3. Rank Group B (protected)         │                       │
│  │  4. Assign sequential ranks          │                       │
│  │     A: -1000, -999, ...              │                       │
│  │     B: continues after A             │                       │
│  └──────────────────┬───────────────────┘                       │
│                     ↓                                           │
│  ┌──────────────────────────────────────┐                       │
│  │      Annotation Manager              │                       │
│  │  For each node's pods:               │                       │
│  │  - Skip customer-managed pods        │                       │
│  │  - Set pod-deletion-cost: <rank>     │                       │
│  │  - Set managed-deletion-cost         │                       │
│  └──────────────────┬───────────────────┘                       │
│                     ↓                                           │
│  Emit metrics + events                                          │
│                                                                 │
└─────────────────────────────────────────────────────────────────┘
```

### Ranking strategies

- **Pod count / disruption cost (recommended)**: Mirrors Karpenter's own consolidation candidate ranking. Nodes with fewer pods get lower deletion costs, making them the first targets for ReplicaSet scale-down. We plan to make this the default.
- **UnallocatedVCPUPerPodCost**: Sort by (unallocated CPU / pod count) descending. Nodes with more wasted CPU per pod are deleted first.
- **LargestToSmallest / SmallestToLargest**: Sort by normalized capacity (CPU cores + memory GB, where 1 core ≈ 1 GB).
- **Random**: Fisher-Yates shuffle, low compute cost, gives a good baseline strategy to compare against for experiments.

All sorting-based strategies use `sort.SliceStable` with deterministic tie-breaking by node name.

### API changes

No CRD changes. The feature is purely controller-side, gated behind `PodDeletionCostManagement` and configured via CLI flags / environment variables. RBAC is extended to add `update` and `patch` verbs on pods.

### Configuration

| Option | CLI Flag | Env Var | Default | Description |
|--------|----------|---------|---------|-------------|
| Feature gate | `--feature-gates=PodDeletionCostManagement=true` | `FEATURE_GATES` | `false` | Enables the controller |
| Ranking strategy | `--pod-deletion-cost-ranking-strategy` | `POD_DELETION_COST_RANKING_STRATEGY` | `Random` | One of: `Random`, `LargestToSmallest`, `SmallestToLargest`, `UnallocatedVCPUPerPodCost` |
| Change detection | `--pod-deletion-cost-change-detection` | `POD_DELETION_COST_CHANGE_DETECTION` | `true` | Skip ranking when cluster state hasn't changed |

### Example: Scale-down concentrates deletions on the consolidation target

A cluster with 3 nodes runs a Deployment with 9 replicas (3 pods per node). The operator enables the controller with `UnallocatedVCPUPerPodCost`:

```
Node A (m5.xlarge):  4 vCPU, 2.5 unallocated, 3 pods  →  0.83 vCPU/pod
Node B (m5.2xlarge): 8 vCPU, 6.0 unallocated, 3 pods  →  2.00 vCPU/pod
Node C (m5.large):   2 vCPU, 0.2 unallocated, 3 pods  →  0.07 vCPU/pod
```

Node B has the most wasted CPU per pod. The ranking engine assigns: Node B → rank -1000 (consolidation target), Node A → -999, Node C → -998 (tightest packing, protect).

**Without the controller:** Scale from 9 to 6 replicas. The ReplicaSet controller spreads deletions: 1 pod from each node. All 3 nodes still have 2 pods. Karpenter can't consolidate any of them.

**With the controller:** The ReplicaSet controller sees all 3 pods on Node B have the lowest deletion cost (-1000). It removes all 3 from Node B. Node B is now empty. Karpenter immediately consolidates it with zero disruption. The cluster goes from 3 nodes to 2.

## Alternatives Considered

### Rank pods directly instead of inheriting node rank

Pod-level ranking spreads ReplicaSet deletions across many nodes rather than concentrating them on the node Karpenter intends to consolidate next. In the 3-node/9-pod example, pod-level ranking would likely remove 1 pod from each of 3 nodes, leaving all 3 still occupied. There's also a coherence problem: if two pods on the same node get very different deletion costs, the ReplicaSet controller might partially drain a node, which doesn't help Karpenter because it still can't consolidate it without disrupting the remaining pods. Node-rank inheritance ensures all pods on a target node are aligned in terms of deletion priority/probability.

### Standalone controller outside Karpenter

The ranking strategies that matter most require the same cluster state Karpenter already maintains in its `state.Cluster` informer cache. A standalone controller would duplicate this state and API server watch load. More importantly, it can't access Karpenter's internal consolidation priority. If the ranking logic evolves to directly consume Karpenter's consolidation scoring, that integration is trivial when they share a process. The operational cost is also higher: separate RBAC, Helm chart, release lifecycle, and failure domain. For a feature tightly coupled to consolidation behavior, co-location is the right call.

### TODO: Changing the replicaset controller behavior directly

TODO: Came up on 3/20 in a convo with James. This note is to remind me to address this possibility here.

### Do nothing

The ReplicaSet controller's scale-down decisions and Karpenter's consolidation decisions remain completely uncoordinated. Scale-down scatters deletions uniformly across nodes. No single node moves closer to empty. For ConsolidateWhenEmpty NodePools, the cluster carries more nodes than necessary. For clusters with do-not-disrupt workloads, the ReplicaSet controller may delete pods from nodes Karpenter can't act on anyway. Over time, this manifests as higher infrastructure cost and slower convergence to optimal cluster size.

## Risks and Mitigations

- **Annotation conflicts with customer-set pod deletion costs (Medium):** An operator or another controller may have already set `controller.kubernetes.io/pod-deletion-cost` on pods. If Karpenter overwrites those values, it silently breaks the operator's intended behavior. *Mitigation:* Two-annotation protocol. Karpenter also sets `karpenter.sh/managed-deletion-cost: "true"` when it manages a pod. If a pod has a deletion cost but lacks the management annotation, it's treated as customer-managed and skipped entirely. Existing customer annotations are never overwritten.

- **Ranking oscillation causing excessive API server writes (Medium):** Unstable rankings could update annotations on many pods every 60-second cycle, creating sustained write load and watch event amplification. *Mitigation:* Three layers: (1) change detector skips ranking when cluster state is unchanged (zero API writes in steady state), (2) `sort.SliceStable` with deterministic tie-breaking by node name prevents rank flipping, (3) 60-second reconcile interval bounds write frequency.

- **Stale annotations after controller is disabled (Low):** Pods retain annotations indefinitely when the feature gate is turned off. *Mitigation:* Annotations live on pods, which are ephemeral. New pods start clean. Staleness is bounded by pod lifetime. Cleanup on disable is a known gap deferred to beta.

- **Pod update RBAC escalation (Medium):** Karpenter's ClusterRole gains `update` and `patch` on pods, broader than the actual access pattern. *Mitigation:* The controller only processes pods on Karpenter-managed nodes via `node.Pods()` / `cluster.Nodes()`. The feature gate ensures the code path is dormant unless explicitly enabled. A future refinement could use server-side apply with a dedicated field manager.

### Backward compatibility

The feature gate defaults to `false`. When disabled, the controller is not registered, no annotations are written, no metrics are emitted, and all existing behavior is unchanged. When enabled, the controller only adds annotations. It never removes customer annotations, modifies pod specs, or changes node state.

## Open Questions

1. **Is Karpenter the right place for this?** [For SIG-Autoscaling / Karpenter maintainers] Would we be better off making this a standalone controller or trying to make changes directly to the ReplicaSet controller? The Alternatives section argues for co-location based on shared state and tight coupling to consolidation intent, but this deserves explicit discussion.

2. **What do we think about the expanded RBAC permissions?** [For Karpenter maintainers / security reviewers] The PR adds `update` and `patch` on pods to Karpenter's ClusterRole. This is a meaningful privilege escalation. Should we explore server-side apply with a dedicated field manager to narrow the effective scope?

3. **How can we further minimize API server load from annotation updates?** [For Karpenter maintainers / SIG-Scalability] The current design relies on change detection and a 60-second reconcile interval. Additional ideas: diffing current vs. desired annotation values before writing, batching updates, or using server-side apply to reduce conflict retries.

## Appendix A: Detailed Component Descriptions

**Controller (controller.go):** A singleton reconciler that runs every 60 seconds. On each tick it checks the `PodDeletionCostManagement` feature gate, gathers all Karpenter-managed nodes from the cluster state, optionally runs change detection, ranks nodes, and updates pod annotations.

**RankingEngine (ranking.go):** Ranks nodes using one of four pluggable strategies. Before ranking, it partitions nodes into two groups: Group A (no do-not-disrupt pods) and Group B (has at least one do-not-disrupt pod). Each group is ranked independently. Group A gets lower ranks (starting at -1000) and Group B gets higher ranks (continuing sequentially after Group A).

**AnnotationManager (annotation.go):** Iterates over pods on each ranked node and sets `controller.kubernetes.io/pod-deletion-cost` to the node's rank value. Also sets `karpenter.sh/managed-deletion-cost: "true"`. Pods with customer-set deletion costs (no management annotation) are skipped. Handles NotFound and Conflict errors gracefully.

**ChangeDetector (changedetector.go):** Computes SHA-256 hashes of node names/timestamps/pod-counts and pod-to-node assignments. If neither hash changed since the last reconcile, ranking is skipped entirely. O(n) optimization that avoids O(n log n) ranking when the cluster is stable.

## Appendix B: Additional Example -- Do-Not-Disrupt Partitioning

A cluster with 4 nodes. Node D runs a pod with `karpenter.sh/do-not-disrupt: "true"` (a long-running ML training job). The operator uses `LargestToSmallest`:

```
Node E (m5.4xlarge): 16 vCPU, no do-not-disrupt pods
Node F (m5.xlarge):   4 vCPU, no do-not-disrupt pods
Node G (m5.2xlarge):  8 vCPU, no do-not-disrupt pods
Node D (m5.4xlarge): 16 vCPU, has do-not-disrupt pod (ML training)
```

Partitioning and ranking:

```
Group A (normal), sorted largest-first:
  Node E (16 vCPU) → rank -1000
  Node G (8 vCPU)  → rank -999
  Node F (4 vCPU)  → rank -998

Group B (do-not-disrupt), sorted largest-first:
  Node D (16 vCPU) → rank -997  (always above all Group A nodes)
```

**Without the controller:** The ReplicaSet controller might delete pods from Node D, but Karpenter can never consolidate it. Those deletions are wasted.

**With the controller:** Pods on Node E have rank -1000 (lowest). The ReplicaSet controller removes pods from Node E first. Karpenter consolidates Node E while the ML training job on Node D is never touched.

## Appendix C: Security and Performance Implications

### Security

- **RBAC expansion:** Karpenter's ClusterRole gains `update` and `patch` on pods (cluster-wide). Minimum privilege needed for annotation management. Operators with tightly scoped RBAC should review.
- **No secrets or sensitive data:** Annotations contain only integer rank values and a boolean flag.
- **No new network access:** Communicates only with the Kubernetes API server using the existing service account.
- **Annotation injection:** A malicious actor with pod write access could set `karpenter.sh/managed-deletion-cost: "true"` to trick Karpenter into managing their deletion cost. Low severity since the attacker already needs pod write access.

### Performance

- **API server write load:** Each reconcile can update up to N pods. With change detection, writes only occur when state changes. Worst case (1,000 nodes, 50 pods/node, constant changes): ~833 pod updates/sec. Mitigated by change detection and potential future annotation value diffing.
- **Memory:** Negligible. References existing `state.StateNode` objects. Ranking data structures are O(n) and transient. Change detector stores 64 bytes.
- **CPU:** O(n log n) for sorting strategies, O(n) for random. Hash computation is O(n × pods_per_node).
- **Watch event amplification:** Annotation updates trigger watch events for other controllers. Bounded by the 60-second reconcile interval. Annotation changes don't affect fields Karpenter's consolidation controller uses for decisions.

## Appendix D: Metrics and Observability

### Prometheus metrics

| Metric | Type | Labels | Description |
|--------|------|--------|-------------|
| `karpenter_pod_deletion_cost_nodes_ranked_total` | Counter | `strategy` | Cumulative nodes ranked across all reconciles |
| `karpenter_pod_deletion_cost_pods_updated_total` | Counter | `result` (`success`, `skipped_customer_managed`, `error`) | Pod annotation update outcomes |
| `karpenter_pod_deletion_cost_ranking_duration_seconds` | Histogram | `strategy` | Time spent computing rankings per reconcile |
| `karpenter_pod_deletion_cost_annotation_duration_seconds` | Histogram | (none) | Time spent updating annotations per reconcile |
| `karpenter_pod_deletion_cost_skipped_no_changes_total` | Counter | (none) | Reconcile cycles skipped (no changes detected) |

### Kubernetes events

| Event | Type | Reason | When |
|-------|------|--------|------|
| Ranking completed | Normal | `PodDeletionCostRankingCompleted` | After successful ranking |
| Update failed | Warning | `PodDeletionCostUpdateFailed` | Pod annotation update failure |
| Feature disabled | Warning | `PodDeletionCostDisabled` | Fatal error causes reconcile skip |

### Structured logging

- `V(1)`: "completed node ranking", "pod deletion cost annotation update completed", "no changes detected, skipping"
- `Error`: "failed to rank nodes", "failed to update pod deletion costs", "failed to list pods on node"

## Appendix E: Testing and Validation

### Unit tests

**Ranking Engine:** Strategy ordering correctness, deterministic tie-breaking by node name, do-not-disrupt partitioning (Group B always above Group A), edge cases (empty input, single node, mixed groups), normalized capacity calculation, zero-pod sentinel values, unknown strategy fallback to random.

**Annotation Manager:** `shouldUpdatePod` logic (no annotations → update; customer-managed → skip; Karpenter-managed → update), correct annotation values, NotFound/Conflict error handling, nil annotations map initialization, metrics counters.

**Change Detector:** First call returns changed, identical state returns unchanged, node/pod additions detected, deterministic hash regardless of iteration order.

**Controller:** Feature gate disabled → no-op, no nodes → no errors, change detection skip → metric incremented, errors → event published and requeue.

### Integration tests

- **End-to-end annotation flow:** Provision 3 nodes, deploy workload, verify annotations match ranking strategy
- **Scale-down alignment:** Scale down, verify pods removed from lowest-cost node, verify Karpenter consolidates it
- **Customer annotation protection:** Pre-set deletion cost without management annotation, verify it's untouched after reconcile
- **Do-not-disrupt partitioning:** Verify protected nodes get higher costs, scale-down avoids them
- **Feature gate toggle:** Enable/disable, verify controller starts/stops, existing annotations persist
- **Multi-NodePool:** Verify global ranking across all Karpenter-managed nodes

### Edge cases

Zero nodes, zero pods on a node, system-only pods, pod deleted between ranking and annotation, concurrent pod updates, identical-capacity nodes, single-node cluster, node transitioning to do-not-disrupt, very large clusters (1000+ nodes), zero allocatable CPU, unrecognized strategy string.

## Appendix F: Migration and Rollout Plan

### Rollout phases

**Alpha (current PR):** Feature gate defaults to `false`. Default strategy is `Random`. Change detection enabled. No stability guarantees.

**Beta:** Feature gate still defaults to `false` but API is stable. Default strategy may change to consolidation-aligned (pod count). Add annotation cleanup on disable. Add annotation value diffing to skip no-op writes. Consider dry-run mode.

**GA:** Feature gate defaults to `true`. Random strategy deprecated in favor of consolidation-aware default.

### Migration steps

Existing users need no action on upgrade (feature gate defaults off). To enable:

1. Review RBAC (new `update`/`patch` on pods)
2. Audit existing `controller.kubernetes.io/pod-deletion-cost` annotations
3. Enable: `--feature-gates=PodDeletionCostManagement=true`
4. Optionally set ranking strategy
5. Monitor `karpenter_pod_deletion_cost_*` metrics

### Rollback

**Disable feature gate:** Set `PodDeletionCostManagement=false`, restart Karpenter. Existing annotations become stale but disappear as pods are recycled.

**Remove annotations manually:**

```bash
kubectl get pods -A -o json | jq -r '.items[] |
  select(.metadata.annotations["karpenter.sh/managed-deletion-cost"]
  == "true") | "\(.metadata.namespace) \(.metadata.name)"' | while
  read ns name; do
    kubectl annotate pod -n "$ns" "$name" \
      controller.kubernetes.io/pod-deletion-cost- \
      karpenter.sh/managed-deletion-cost-
done
```

**Rollback signals:** High `pods_updated_total{result="error"}`, API server latency correlated with reconcile interval, unexpected scale-down behavior, growing `ranking_duration_seconds`.

### Documentation

New: feature guide, configuration reference, metrics reference, troubleshooting guide.

Updates: feature gates table, consolidation docs, RBAC docs, Helm chart values, disruption/do-not-disrupt docs, changelog.

## Appendix: Non-Goals

- Direct changes to consolidation behavior or performance. All improvements here are indirect, resulting from the ReplicaSet controller's changed pod selection ordering.
- Any changes to existing Karpenter behavior or features outside of the new pod deletion cost labelling.
