# Pod Deletion Cost Controller

## Summary

This RFC proposes a new feature-gated controller for Karpenter that ranks nodes by consolidation preference and propagates that ranking to pods via the `controller.kubernetes.io/pod-deletion-cost` annotation. By aligning the ReplicaSet controller's scale-down decisions with Karpenter's consolidation targets, we measurably reduce voluntary pod disruption rate. This is tracked and validated via the pod disruption metrics being added in [kubernetes-sigs/karpenter#2892](https://github.com/kubernetes-sigs/karpenter/pull/2892). The feature is off by default and configured entirely through feature gates and CLI flags, with no CRD changes required.

## Motivation

### Disruption from consolidation

When Karpenter consolidates underutilized nodes, it disrupts running pods. For customer workloads, that disruption has a real cost: warm caches are lost, replacement pods must re-establish connections and reload state, and workloads that are slow to start (ML model loading, JVM warmup, large dataset hydration) can take minutes or longer to return to full capacity. Teams pay for this in reduced throughput during recovery and engineering complexity to handle graceful shutdown.

Karpenter already manages clusters toward lower compute cost, but it has limited ability to control how much disruption that right-sizing produces. Current disruption controls focus on preventing disruptions that exceed a given rate for a given nodepool or deployment regardless of the cost-savings merits of that action.

### Coordination gap

The root cause is a coordination gap between two independent controllers operating on the same cluster. The ReplicaSet controller decides which pods to delete during scale-down. Karpenter's consolidation controller decides which nodes to drain and remove. These two controllers share no information about each other's intent. Without coordination, the ReplicaSet controller uses its default pod selection heuristic during scale-in: prefer pending over ready, respect pod-deletion-cost, spread terminations across nodes, prefer newer pods, then break ties randomly. With no pod-deletion-cost set, the spreading heuristic dominates, and terminations distribute roughly evenly across nodes. This increases the entropy of the cluster: most nodes end up packed at roughly the same density, and often no single node moves meaningfully closer to empty than other nodes. The result is that Karpenter's consolidation controller finds the same unfavorable state after a pod replica scale-in that it found before, with all nodes still occupied, none empty, and utilization distribution roughly unchanged but slightly lower everywhere. This matters especially for “ConsolidateWhenEmpty” NodePools, where the consolidation policy requires a node to be completely free of pods before it can be removed. If the ReplicaSet controller never concentrates deletions on a single node, that condition is rarely met, and the cluster carries more nodes than necessary.

### Why node-level ranking is the right signal

The `pod-deletion-cost` annotation is the only existing communication channel between Karpenter and the ReplicaSet controller. Karpenter doesn't consolidate pods; it consolidates nodes. When it evaluates a consolidation move, the atomic unit is a node: "can I drain this entire node and either delete it or replace it with something cheaper?" If we ranked pods independently by resource usage, age, or some pod-level heuristic, the ReplicaSet controller might delete a pod from Node A (because that pod scored low individually) while leaving all other pods on Node A intact. That doesn't help Karpenter. Node A still can't be easily consolidated because it still has pods. The "hint" was spent on a decision that doesn't move the system toward an easier-to-consolidate state.

When pods inherit their node's rank, all pods on the same node share the same deletion cost. The ReplicaSet controller's deletion probability becomes uniform within a node but ordered across nodes, exactly matching the structure of Karpenter's consolidation decisions. The practical consequence is signal alignment:

1. Karpenter ranks nodes by consolidation priority
2. Pods on those nodes get the lowest deletion costs
3. ReplicaSet scale-down removes pods from those nodes first
4. Those nodes become empty or closer to empty
5. Karpenter can consolidate them with less disruption (or they qualify for WhenEmpty consolidation)

This logic extends naturally to nodes that Karpenter cannot consolidate and to nodes that are already marked for replacement. The ranking engine partitions nodes into three groups before ranking:

- **Group A (Drifted):** Nodes with `ConditionTypeDrifted=True` and no do-not-disrupt pods. These nodes need replacement anyway for compliance or security reasons. Draining them first via RS scale-down means scale-down events naturally assist drift progress. Sorted by pod count ascending (fewest pods = lowest cost = drained first).
- **Group B (Normal):** Nodes that are not drifted and have no do-not-disrupt pods. Standard consolidation targets. Sorted by pod count ascending.
- **Group C (Do-not-disrupt):** Nodes with at least one `karpenter.sh/do-not-disrupt` pod. Karpenter cannot act on these nodes regardless, and they are also protected from drift. Deleting pods from them has zero consolidation or drift value. Sorted by pod count ascending.

Group A always receives the lowest deletion costs, Group B the middle range, and Group C the highest, so the ReplicaSet controller removes pods from drifted nodes first, then normal consolidation targets, and protected nodes last.

### Evidence

Community issues and production reports consistently point to excessive disruption as a pain point:

- [#2123](https://github.com/kubernetes-sigs/karpenter/issues/2123) -- Karpenter scaling disproportionately high nodes, over-provisioning then consolidating, creating unnecessary churn
- [aws/karpenter#4356](https://github.com/aws/karpenter/issues/4356) -- Constant pod disruption from Karpenter replacing nodes with cheaper ones even when savings were marginal
- [aws/karpenter#3785](https://github.com/aws/karpenter/issues/3785) -- After peak hours, consolidation was too slow, but aggressive consolidation caused massive pod churn
- [aws/karpenter#3927](https://github.com/aws/karpenter/issues/3927) -- Production clusters with 100+ pods per node couldn't consolidate without disrupting running workloads
- BMW's Karpenter migration across 375+ EKS clusters explicitly called out "excessive pod disruption" as a challenge

Benchmark experiments and simulations show meaningful decreases in disruption rate when using pod deletion cost annotations. The pod disruption tracking being added in [kubernetes-sigs/karpenter#2892](https://github.com/kubernetes-sigs/karpenter/pull/2892) will provide the metrics infrastructure to measure and validate these improvements in production. The effectiveness varies by deployment pattern: clusters with a few large deployments see the strongest benefit, while clusters with many small deployments (where each scale-down removes only 1-2 pods) see less improvement. These gains compound when combined with other proposed features such as consolidation disruption cost thresholds. This feature also pairs well with the "MostAllocated" scoring strategy for kube-scheduler, which concentrates pods onto fewer nodes and amplifies the consolidation benefit.

## Goals

- Measurably reduce voluntary pod disruption rate for clusters using Deployments/ReplicaSets, as tracked by the pod disruption metrics in [#2892](https://github.com/kubernetes-sigs/karpenter/pull/2892)
- Maintain or improve total compute cost and utilization efficiency across all benchmarks
- Minimal impact on API server load from annotation updates

### Value by consolidation policy

The benefit of this feature varies by consolidation policy. For `ConsolidateWhenEmpty` NodePools, the value is direct: concentrating pod deletions on a single node is the only way to create the fully-empty condition that triggers consolidation. Without this controller, scale-down events rarely produce empty nodes. For `ConsolidateWhenUnderutilized` NodePools, the value is indirect but meaningful: concentrating deletions reduces the number of pods Karpenter must evict when it consolidates, lowering the total disruption cost of each consolidation move even though the consolidation would eventually happen regardless.

### Related Proposals that can reduce the pod disruption rate

- Most important: Consolidation cost thresholding -- see draft RFC

## Proposal

We introduce a new feature-gated controller (`pod.deletioncost`) that automatically manages the `controller.kubernetes.io/pod-deletion-cost` annotation on pods running on Karpenter-managed nodes. The controller ranks nodes by pod count (mirroring Karpenter's own consolidation candidate sorting), assigns sequential deletion cost values to pods so that Kubernetes' ReplicaSet scale-down logic preferentially removes pods from the "least valuable" nodes first, and partitions nodes hosting do-not-disrupt pods separately to protect them from early eviction.

### Ranking strategy

The controller ranks nodes the same way Karpenter ranks consolidation candidates: by pod count (disruption cost). Within this ranking, drifted nodes (`ConditionTypeDrifted=True`) are placed in the lowest-cost tier, ahead of non-drifted nodes. Since drifted nodes need replacement anyway, directing RS deletions toward them first means scale-down events naturally assist drift progress without additional disruptions. This maximizes the mutual information between the hint we send and the action Karpenter will take next, because the ranking signal and the consolidation signal are literally the same function. As Karpenter's consolidation candidate sorting evolves, we plan to follow it here to stay aligned.

### How it works

All new code lives in `pkg/controllers/pod/deletioncost/`. A singleton reconciler runs every 60 seconds. The 60-second interval balances annotation freshness against API server write load — fast enough to react to scale events within one HPA evaluation period, slow enough to avoid amplifying watch events during steady state. On each tick it:

1. Checks the `PodDeletionCostManagement` feature gate
2. Collects all Karpenter-managed nodes from `state.Cluster`
3. Runs change detection (SHA-256 hash of node/pod state). If nothing changed, skips with zero API writes
4. Partitions nodes into Group A (drifted, no do-not-disrupt pods), Group B (not drifted, no do-not-disrupt pods), and Group C (has do-not-disrupt pods)
5. Ranks each group independently by pod count ascending
6. Assigns sequential integer ranks starting at -n (where n is the total number of Karpenter-managed nodes) for Group A, continuing for Group B, then Group C
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
│  │  1. Partition: drifted / normal /    │                       │
│  │     do-not-disrupt                   │                       │
│  │  2. Rank Group A (drifted)           │                       │
│  │  3. Rank Group B (normal)            │                       │
│  │  4. Rank Group C (do-not-disrupt)    │                       │
│  │  5. Assign sequential ranks          │                       │
│  │     A: -n, -(n-1), ...              │                       │
│  │     B: continues after A            │                       │
│  │     C: continues after B            │                       │
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

### API changes

No CRD changes. The feature is purely controller-side, gated behind `PodDeletionCostManagement` and configured via CLI flags / environment variables. RBAC is extended to add `update` and `patch` verbs on pods.

### Configuration

| Option | CLI Flag | Env Var | Default | Description |
|--------|----------|---------|---------|-------------|
| Feature gate | `--feature-gates=PodDeletionCostManagement=true` | `FEATURE_GATES` | `false` | Enables the controller |
| Change detection | `--pod-deletion-cost-change-detection` | `POD_DELETION_COST_CHANGE_DETECTION` | `true` | Skip ranking when cluster state hasn't changed |

### Example: Scale-down concentrates deletions on the consolidation target

A cluster with 3 nodes runs a Deployment with 9 replicas (3 pods per node). The operator enables the controller:

```
Node A (m5.xlarge):  3 pods
Node B (m5.2xlarge): 3 pods
Node C (m5.large):   3 pods
```

All nodes have the same pod count, so the ranking engine breaks ties by node name. With 3 nodes, it assigns: Node A → rank -3 (consolidation target), Node B → -2, Node C → -1.

**Without the controller:** Scale from 9 to 6 replicas. The ReplicaSet controller spreads deletions: 1 pod from each node. All 3 nodes still have 2 pods. Karpenter can still consolidate, but must evict pods to do so — increasing total disruption. No node moved closer to empty, so the scale-in didn't help consolidation at all.

**With the controller:** The ReplicaSet controller sees all 3 pods on Node A have the lowest deletion cost (-3). It removes all 3 from Node A. Node A is now empty. Karpenter immediately consolidates it with zero disruption. The cluster goes from 3 nodes to 2.

### Example: Partial drain converges over multiple scale-down events (9→7)

Same starting state: 3 nodes, 9 replicas (3 per node), Node A ranked lowest (-3).

**Scale from 9 to 7 replicas.** The ReplicaSet controller removes 2 pods, both from Node A (lowest deletion cost). State after scale-down:

```
Node A: 1 pod   (rank -3)
Node B: 3 pods  (rank -2)
Node C: 3 pods  (rank -1)
```

Node A isn't empty yet, so Karpenter can't consolidate it under a WhenEmpty policy. But Node A still has the fewest pods, so it retains the lowest rank on the next reconcile. On the next scale-down event (e.g., 7→5), the ReplicaSet controller again targets Node A first, removing the remaining pod. Node A becomes empty and Karpenter consolidates it. The key insight: even partial drains converge toward empty nodes over successive scale-down events because the ranking is stable.

### Example: Drifted node drains first via three-tier ranking

A cluster with 3 nodes. Node A is drifted (pending AMI replacement):

```
Node A (m5.xlarge):  3 pods, drifted (ConditionTypeDrifted=True)
Node B (m5.2xlarge): 3 pods, normal
Node C (m5.large):   3 pods, normal
```

Three-tier ranking:

```
Group A (drifted):
  Node A (3 pods) → rank -3

Group B (normal), sorted by pod count ascending:
  Node B (3 pods) → rank -2
  Node C (3 pods) → rank -1
```

**Scale from 9 to 6 replicas.** The ReplicaSet controller sees all 3 pods on Node A have the lowest deletion cost (-3). It removes all 3 from Node A. Node A is now empty. Karpenter consolidates it with zero disruption — and the drifted node is replaced, achieving both cost savings and drift progress in a single scale-down event.

**Without three-tier ranking:** Node A would compete equally with Nodes B and C for deletions. The drifted node might not drain first, delaying drift compliance while providing no additional consolidation benefit.

## Alternatives Considered

### Rank pods directly instead of inheriting node rank

Pod-level ranking spreads ReplicaSet deletions across many nodes rather than concentrating them on the node Karpenter intends to consolidate next. In the 3-node/9-pod example, pod-level ranking could remove 1 pod from each of 3 nodes, leaving all 3 still occupied. There's a coherence problem: if two pods on the same node get very different deletion costs, the ReplicaSet controller might partially drain a node, which doesn't help Karpenter because it must still evict the remaining pod(s) to consolidate, adding disruption that the scale-in could have avoided. Node-rank inheritance ensures all pods on a target node are aligned in terms of deletion priority/probability.

### Standalone controller outside Karpenter

The ranking strategies that matter most require the same cluster state Karpenter already maintains in its state.Cluster informer cache. A standalone controller would duplicate this state and API server watch load. If the ranking logic evolves to directly consume Karpenter's consolidation scoring, that integration is trivial when they share a process. The operational cost is also higher: separate RBAC, Helm chart, release lifecycle, etc. For a feature tightly coupled to consolidation behavior, co-location is the right call, but the controller could certainly be kept independent and still work the same.

### Changing the ReplicaSet Controller Behavior Directly

An alternative (and complementary) approach is to modify the ReplicaSet controller's pod deletion algorithm itself, rather than influencing it indirectly through annotations. This is being pursued in parallel as a Kubernetes enhancement: [KEP: ReplicaSet Consolidation-Aware Scale-In Strategy (kubernetes/enhancements#5982)](https://github.com/kubernetes/enhancements/issues/5982).

#### Short-Term: Pod Deletion Cost Labels (This RFC)

The pod-deletion-cost approach proposed in this RFC is the near-term deliverable. It works with the existing ReplicaSet controller by using the `controller.kubernetes.io/pod-deletion-cost` annotation — a communication channel that already exists in Kubernetes (KEP-2255). This approach:

- Ships entirely within Karpenter (no upstream Kubernetes changes required)
- Works with all existing Kubernetes versions that support pod-deletion-cost
- Can be feature-gated and iterated on independently
- Provides measurable disruption reduction as validated by the pod disruption metrics in [#2892](https://github.com/kubernetes-sigs/karpenter/pull/2892)

The tradeoff is operational complexity: Karpenter must continuously maintain annotations, handle change detection, and manage the two-annotation protocol to avoid conflicts with customer-set values. It also adds API server write load proportional to the number of managed pods.

#### Long-Term: Direct ReplicaSet Controller Change

The [kubernetes/enhancements#5982](https://github.com/kubernetes/enhancements/issues/5982) KEP proposes adding a `ConsolidatingScaleDown` feature gate to `kube-controller-manager`. When enabled, the ReplicaSet controller's pod deletion sort order changes to prefer deleting pods on nodes with *fewer* total active pods (a consolidation heuristic), reversing the current spreading heuristic. It also respects do-not-disrupt signals to deprioritize pods on protected nodes.

This approach:

- Eliminates the need for an external annotation-management controller
- Has zero API server overhead (no annotation writes, no reconcile loop)
- Applies universally to all ReplicaSets, not just those on Karpenter-managed nodes
- Is complementary to KEP-2255 — `pod-deletion-cost` annotations take precedence in the existing sort order, so both mechanisms coexist cleanly

The tradeoff is timeline: upstream Kubernetes changes require a KEP, sig-apps review, and multi-release graduation (alpha → beta → GA). The design is still in the proposal stage.

#### Relationship Between the Two Approaches

These approaches are complementary, not competing. Either one alone delivers the core benefit (consolidation-aware scale-in). Having both doesn't conflict — the ReplicaSet controller's sort order already checks `pod-deletion-cost` before its built-in heuristics, so if Karpenter sets annotations AND the RS controller has consolidation-aware sorting, the annotations simply reinforce the same preference.

The recommended path is:

1. **Ship the pod-deletion-cost controller now** (this RFC) — immediate value, no upstream dependency
2. **Pursue the RS controller KEP in parallel** — broader impact, zero-overhead solution
3. **Deprecate the annotation controller** once the RS controller change reaches GA and the minimum supported Kubernetes version includes it

### Do nothing

The ReplicaSet controller's scale-down decisions and Karpenter's consolidation decisions remain completely uncoordinated. Scale-down scatters deletions uniformly across nodes. No single node moves closer to empty. For ConsolidateWhenEmpty NodePools, the cluster carries more nodes than necessary. For clusters with do-not-disrupt workloads, the ReplicaSet controller may delete pods from nodes Karpenter can't act on anyway. Over time, this manifests as higher infrastructure cost and slower convergence to optimal cluster size.

## Risks and Mitigations

- **Annotation conflicts with customer-set pod deletion costs (Medium):** An operator or another controller may have already set `controller.kubernetes.io/pod-deletion-cost` on pods. If Karpenter overwrites those values, it silently breaks the operator's intended behavior. *Mitigation:* Two-annotation protocol. Karpenter also sets `karpenter.sh/managed-deletion-cost: "true"` when it manages a pod. If a pod has a deletion cost but lacks the management annotation, it's treated as customer-managed and skipped entirely. Existing customer annotations are never overwritten. Additionally, Karpenter-managed ranks are always negative (starting at -n), so customer-set positive values naturally take precedence in the RS controller's sort order without any special handling.

- **Ranking oscillation causing excessive API server writes (Medium):** Unstable rankings could update annotations on many pods every 60-second cycle, creating sustained write load and watch event amplification. *Mitigation:* Three layers: (1) change detector skips ranking when cluster state is unchanged (zero API writes in steady state), (2) `sort.SliceStable` with deterministic tie-breaking by node name prevents rank flipping, (3) 60-second reconcile interval bounds write frequency.

- **Stale annotations after controller is disabled (Low):** Pods retain annotations indefinitely when the feature gate is turned off. *Mitigation:* Annotations live on pods, which are ephemeral. New pods start clean. Staleness is bounded by pod lifetime. Cleanup on disable is a known gap deferred to beta.

- **Pod update RBAC escalation (Medium):** Karpenter's ClusterRole gains `update` and `patch` on pods, broader than the actual access pattern. *Mitigation:* The controller only processes pods on Karpenter-managed nodes via `node.Pods()` / `cluster.Nodes()`. The feature gate ensures the code path is dormant unless explicitly enabled. A future refinement could use server-side apply with a dedicated field manager.

- **Consolidation-optimized deletions may conflict with topology spread constraints (Low):** When the controller concentrates deletions on specific nodes, the resulting pod distribution may temporarily violate `topologySpreadConstraints` until the scheduler places replacement pods. *Mitigation:* This is the same behavior as the current spreading heuristic — neither approach guarantees constraint satisfaction during scale-down. The Kubernetes scheduler enforces topology spread when placing new pods, so any temporary imbalance is corrected on the next scheduling cycle. Operators with strict spreading requirements can leave the feature disabled.

- **Active-scaling fan-out (Medium):** During active scaling events (e.g., HPA-driven burst), many nodes may change pod count simultaneously, causing the controller to re-rank and update annotations on a large fraction of pods in a single reconcile cycle. For a 500-node cluster averaging 30 pods/node, a full re-rank writes ~15,000 pod annotations in one 60-second window (~250 writes/sec). *Mitigation:* Change detection prevents re-ranking when state is stable. During active scaling, the writes are bounded by the reconcile interval (at most once per 60s). The API server write load is comparable to a large Deployment rollout. Clusters with >1,000 nodes should monitor `karpenter_pod_deletion_cost_annotation_duration_seconds` and consider disabling the feature if annotation latency exceeds acceptable thresholds.

### Placement and Spreading Constraints

A natural concern is whether optimizing for consolidation (concentrating pod deletions on specific nodes) conflicts with topology spread constraints, pod affinity/anti-affinity rules, or zone distribution requirements. This section addresses that concern directly.

#### The current RS scale-down logic doesn't address placement concerns either

The existing ReplicaSet controller scale-down heuristic is: prefer pending pods over running, respect `pod-deletion-cost`, spread terminations across nodes (prefer nodes with more colocated replicas), prefer newer pods, then break ties randomly. This spreading heuristic does not consider topology spread constraints, pod affinity/anti-affinity, or zone distribution. It optimizes for a single goal — even distribution of deletions across nodes — and ignores all scheduling constraints.

The proposed change is not regressing from a placement-aware system. It is offering an alternative heuristic that optimizes for a different goal (lower pod disruption and better consolidation) while being equally unaware of scheduling constraints.

#### This proposal is an alternative heuristic, not a scheduling-aware system

The proposal does not attempt to link scheduling logic to RS scale-in decisions. It offers a different tiebreaker for the ReplicaSet controller's pod deletion sort:

- **Current heuristic**: prefer pods on nodes with more colocated replicas (spreading)
- **Proposed heuristic**: prefer pods on underutilized/consolidation-target nodes (consolidation)

Both are simple heuristics that don't reason about scheduling constraints. The difference is which operational goal they optimize for. The current heuristic optimizes for even distribution; the proposed heuristic optimizes for enabling node consolidation.

#### Balancing all concerns is a fundamentally harder problem

If we wanted RS scale-in to simultaneously consider topology spread constraints, pod affinity/anti-affinity, zone distribution, AND consolidation impact, we would need to scalarize and weight all of these concerns — essentially building a mini-scheduler for scale-down. That is a fundamentally different (and much more complex) proposal that would require:

- Reading and interpreting all scheduling constraints from pod specs
- Evaluating constraint satisfaction across candidate deletion sets
- Defining weights or priorities across competing objectives
- Handling constraint conflicts (e.g., "spread evenly across zones" vs. "consolidate onto fewer nodes")

This level of sophistication is out of scope for this RFC and would be a separate KEP entirely.

#### Current and proposed behaviors are symmetric in their simplicity

| Aspect | Current (Spreading) | Proposed (Consolidation) |
|--------|---------------------|--------------------------|
| Optimizes for | Even distribution across nodes | Enabling node removal |
| Considers scheduling constraints | No | No |
| Considers topology spread | No | No |
| Considers pod affinity | No | No |
| Considers zone distribution | No | No |
| Heuristic complexity | Simple (count colocated replicas) | Simple (node consolidation rank) |

Neither approach is "correct" in all cases — they represent different tradeoffs for different operational priorities. Clusters that prioritize even distribution can leave the feature disabled. Clusters that prioritize cost efficiency and reduced disruption can enable it.

#### A future scalarized approach could subsume both

If the community wants to build a weighted multi-objective scale-down scorer, both the current spreading heuristic and the proposed consolidation heuristic could become weighted factors in that system. The `ConsolidatingScaleDown` KEP ([kubernetes/enhancements#5982](https://github.com/kubernetes/enhancements/issues/5982)) is a step in this direction — it adds consolidation as an alternative heuristic behind a feature gate, leaving room for future work that combines multiple objectives. But that future work is a separate effort and should not block shipping a pragmatic improvement today.

### Backward compatibility

The feature gate defaults to `false`. When disabled, the controller is not registered, no annotations are written, no metrics are emitted, and all existing behavior is unchanged. When enabled, the controller only adds annotations. It never removes customer annotations, modifies pod specs, or changes node state.

## Open Questions

1. **Is Karpenter the right place for this?** [For SIG-Autoscaling / Karpenter maintainers] Would we be better off making this a standalone controller or trying to make changes directly to the ReplicaSet controller? The Alternatives section argues for co-location based on shared state and tight coupling to consolidation intent, but this deserves explicit discussion.

2. **What do we think about the expanded RBAC permissions?** [For Karpenter maintainers / security reviewers] The PR adds `update` and `patch` on pods to Karpenter's ClusterRole. This is a meaningful privilege escalation. Should we explore server-side apply with a dedicated field manager to narrow the effective scope?

3. **How can we further minimize API server load from annotation updates?** [For Karpenter maintainers / SIG-Scalability] The current design relies on change detection and a 60-second reconcile interval. Additional ideas: diffing current vs. desired annotation values before writing, batching updates, or using server-side apply to reduce conflict retries.

## Future Work

This RFC uses `pod-deletion-cost` annotations as the communication channel between Karpenter and the ReplicaSet controller. Annotations work today with every Kubernetes version and require no upstream changes, but they have known limitations: last-writer-wins semantics, no built-in support for multiple contributors, and the timing constraint that annotations must be set before scale-down starts.

We see annotations as the v1 mechanism, not the end state. Several directions could improve on this foundation:

### Dedicated API for disruption preferences

Multiple systems may want to influence pod deletion priority: the node autoscaler (consolidation targets), a drift controller (nodes needing replacement), a traffic shaper (replicas already being drained), or the scheduler (topology-aware candidates). Today these would all fight over a single annotation.

A dedicated API object (for example, a `PodDisruptionPreference` resource or a new field on NodeClaim) would let each system express its input independently using server-side apply with distinct field managers. A reconciler would merge these inputs into the final `pod-deletion-cost` annotation that the RS controller consumes. This cleanly separates "who has an opinion" from "what the RS controller sees."

### Scheduler library integration

As Karpenter upstreams into the kube-scheduler via the scheduler library, the merging of scale-in signals could happen inside the scheduler itself. The scheduler already has scheduling context (topology spread, affinity, zone distribution). Adding scale-in awareness would let it produce deletion priorities that account for both scheduling constraints and consolidation goals, without requiring an external annotation-management loop.

### Deprecation path

Once a proper API or scheduler integration exists, the annotation-management controller described in this RFC can be deprecated. The controller is designed to be replaceable: it writes standard `pod-deletion-cost` annotations that any future mechanism would also produce. No workload changes would be needed when migrating to a better signal delivery mechanism.

## Appendix A: Detailed Component Descriptions

**Controller (controller.go):** A singleton reconciler that runs every 60 seconds. On each tick it checks the `PodDeletionCostManagement` feature gate, gathers all Karpenter-managed nodes from the cluster state, optionally runs change detection, ranks nodes, and updates pod annotations.

**RankingEngine (ranking.go):** Ranks nodes by pod count (mirroring Karpenter's consolidation candidate sorting). Before ranking, it partitions nodes into three groups: Group A (drifted, no do-not-disrupt pods), Group B (not drifted, no do-not-disrupt pods), and Group C (has at least one do-not-disrupt pod). Each group is ranked independently by pod count ascending. Group A gets the lowest ranks, Group B the middle range, and Group C the highest (continuing sequentially). Uses `sort.SliceStable` with deterministic tie-breaking by node name. Drift status is read from `ConditionTypeDrifted` on the node's `StateNode`.

**AnnotationManager (annotation.go):** Iterates over pods on each ranked node and sets `controller.kubernetes.io/pod-deletion-cost` to the node's rank value. Also sets `karpenter.sh/managed-deletion-cost: "true"`. Pods with customer-set deletion costs (no management annotation) are skipped. Handles NotFound and Conflict errors gracefully.

**ChangeDetector (changedetector.go):** Computes SHA-256 hashes of node names/timestamps/pod-counts and pod-to-node assignments. If neither hash changed since the last reconcile, ranking is skipped entirely. O(n) optimization that avoids O(n log n) ranking when the cluster is stable.

## Appendix B: Additional Example -- Do-Not-Disrupt Partitioning

A cluster with 4 nodes. Node D runs a pod with `karpenter.sh/do-not-disrupt: "true"` (a long-running ML training job):

```
Node E (m5.4xlarge): 2 pods, no do-not-disrupt pods
Node F (m5.xlarge):  5 pods, no do-not-disrupt pods
Node G (m5.2xlarge): 3 pods, no do-not-disrupt pods
Node D (m5.4xlarge): 4 pods, has do-not-disrupt pod (ML training)
```

Suppose Node G is also drifted (`ConditionTypeDrifted=True`). Partitioning and ranking:

```
Group A (drifted, no do-not-disrupt), sorted by pod count ascending:
  Node G (3 pods) → rank -4 (drifted consolidation target)

Group B (normal, not drifted, no do-not-disrupt), sorted by pod count ascending:
  Node E (2 pods) → rank -3
  Node F (5 pods) → rank -2

Group C (do-not-disrupt), sorted by pod count ascending:
  Node D (4 pods) → rank -1 (always above all Group A and B nodes)
```

The ReplicaSet controller removes pods from drifted Node G first (rank -4), then normal Node E (rank -3). Node D's ML training job is never touched. Drifted Node G drains before non-drifted Node E despite Node E having fewer pods, because drift priority takes precedence over pod count across groups.

**Without the controller:** The ReplicaSet controller might delete pods from Node D, but Karpenter can never consolidate it. Those deletions are wasted.

**With the controller:** Pods on drifted Node G have rank -4 (lowest). The ReplicaSet controller removes pods from Node G first, assisting drift progress. Karpenter consolidates Node G while the ML training job on Node D is never touched.

## Appendix C: Security and Performance Implications

### Security

- **RBAC expansion:** Karpenter's ClusterRole gains `update` and `patch` on pods (cluster-wide). Minimum privilege needed for annotation management. Operators with tightly scoped RBAC should review.
- **No secrets or sensitive data:** Annotations contain only integer rank values and a boolean flag.
- **No new network access:** Communicates only with the Kubernetes API server using the existing service account.
- **Annotation injection:** A malicious actor with pod write access could set `karpenter.sh/managed-deletion-cost: "true"` to trick Karpenter into managing their deletion cost. Low severity since the attacker already needs pod write access.

### Performance

- **API server write load:** Each reconcile can update up to N pods. With change detection, writes only occur when state changes. Worst case (1,000 nodes, 50 pods/node, constant changes): ~833 pod updates/sec. Mitigated by change detection and potential future annotation value diffing.
- **Memory:** Negligible. References existing `state.StateNode` objects. Ranking data structures are O(n) and transient. Change detector stores 64 bytes.
- **CPU:** O(n log n) for sorting by pod count. Hash computation is O(n × pods_per_node).
- **Watch event amplification:** Annotation updates trigger watch events for other controllers. Bounded by the 60-second reconcile interval. Annotation changes don't affect fields Karpenter's consolidation controller uses for decisions.

## Appendix D: Metrics, Testing, and Rollout

Detailed metrics tables (Prometheus metrics, Kubernetes events, structured logging levels), test plans (unit test cases, integration test scenarios, edge case lists), and rollback scripts will be included in the implementation PR. The key observability and rollout decisions are:

- Prometheus metrics cover nodes ranked, pods updated (by result), ranking duration, annotation duration, and skipped-no-changes count
- Kubernetes events emitted for ranking completion, update failures, and feature-disabled states
- Rollout follows alpha (gate off, no stability guarantees) → beta (gate off, API stable, add cleanup-on-disable) → GA (gate on by default)
- Rollback is safe: disable the feature gate and existing annotations become stale, bounded by pod lifetime
- Migration requires no action on upgrade; enabling requires RBAC review and annotation audit

## Appendix: Non-Goals

- Direct changes to consolidation behavior or performance. All improvements here are indirect, resulting from the ReplicaSet controller's changed pod selection ordering.
- Any changes to existing Karpenter behavior or features outside of the new pod deletion cost labelling.
