# Pod Deletion Cost Controller

## Summary

This RFC proposes a new feature-gated controller for Karpenter that ranks nodes by consolidation preference and propagates that ranking to pods via the `controller.kubernetes.io/pod-deletion-cost` annotation. This also introduces `karpenter.sh/disruption-cost` as the user-facing annotation for influencing consolidation priority, replacing direct use of `pod-deletion-cost` for that purpose. By aligning the ReplicaSet controller's scale-down decisions with Karpenter's consolidation targets, we measurably reduce voluntary pod disruption rate. This is tracked and validated via the pod disruption metrics being added in [kubernetes-sigs/karpenter#2892](https://github.com/kubernetes-sigs/karpenter/pull/2892). The feature is off by default and configured entirely through feature gates and CLI flags, with no CRD changes required.

## Motivation

### Disruption from consolidation

When Karpenter consolidates underutilized nodes, it disrupts running pods. For customer workloads, that disruption has a real cost: warm caches are lost, replacement pods must re-establish connections and reload state, and workloads that are slow to start (ML model loading, JVM warmup, large dataset hydration) can take minutes or longer to return to full capacity. Teams pay for this in reduced throughput during recovery and engineering complexity to handle graceful shutdown.

Karpenter already manages clusters toward lower compute cost, but it has limited ability to control how much disruption that right-sizing produces. Current disruption controls focus on preventing disruptions that exceed a given rate for a given nodepool or deployment regardless of the cost-savings merits of that action.

### Coordination gap

The root cause is a coordination gap between two independent controllers operating on the same cluster. The [ReplicaSet controller](https://kubernetes.io/docs/concepts/workloads/controllers/replicaset/) decides which pods to delete during scale-down. Karpenter's consolidation controller decides which nodes to drain and remove. These two controllers share no information about each other's intent. Without coordination, the ReplicaSet controller uses its default pod selection heuristic during scale-in: prefer pending over ready, respect pod-deletion-cost, spread terminations across nodes, prefer newer pods, then break ties randomly. With no pod-deletion-cost set, the spreading heuristic dominates, and terminations distribute roughly evenly across nodes. This increases the entropy of the cluster: most nodes end up packed at roughly the same density, and often no single node moves meaningfully closer to empty than other nodes. The result is that Karpenter's consolidation controller finds the same unfavorable state after a pod replica scale-in that it found before, with all nodes still occupied, none empty, and utilization distribution roughly unchanged but slightly lower everywhere. This matters especially for "ConsolidateWhenEmpty" NodePools, where the consolidation policy requires a node to be completely free of pods before it can be removed. If the ReplicaSet controller never concentrates deletions on a single node, that condition is rarely met, and the cluster carries more nodes than necessary.

### Why node-level ranking is the right signal

The `pod-deletion-cost` annotation is the only existing communication channel between Karpenter and the ReplicaSet controller. Karpenter doesn't consolidate pods; it consolidates nodes. When it evaluates a consolidation move, the atomic unit is a node: "can I drain this entire node and either delete it or replace it with something cheaper?" If we ranked pods independently by resource usage, age, or some pod-level heuristic, the ReplicaSet controller might delete a pod from Node A (because that pod scored low individually) while leaving all other pods on Node A intact. That doesn't help Karpenter. Node A still can't be easily consolidated because it still has pods. The "hint" was spent on a decision that doesn't move the system toward an easier-to-consolidate state.

When pods inherit their node's rank, all pods on the same node share the same deletion cost. The ReplicaSet controller's deletion probability becomes uniform within a node but ordered across nodes, exactly matching the structure of Karpenter's consolidation decisions. The practical consequence is signal alignment:

1. Karpenter ranks nodes by consolidation priority
2. Pods on those nodes get the lowest deletion costs
3. ReplicaSet scale-down removes pods from those nodes first
4. Those nodes become empty or closer to empty
5. Karpenter can consolidate them with less disruption (or they qualify for WhenEmpty consolidation)

The ranking uses Karpenter's calculated disruption cost for each node, the same heuristic used for consolidation candidate ranking. This RFC does not prescribe how disruption cost is calculated (today it is based on pod count/pod-deletion-cost, but the Balanced Consolidation proposal [#2962](https://github.com/kubernetes-sigs/karpenter/pull/2962) will introduce cost/benefit scoring). What matters is that we intentionally use the same node ranking heuristic for this signal as for consolidation decisions.

This logic extends naturally to nodes that Karpenter cannot consolidate and to nodes that are already marked for replacement. The ranking engine partitions nodes into four categories:

- **Draining:** Nodes that have the `karpenter.sh/disrupted` taint (disruption already committed) AND at least one pod blocked by a PDB with `DisruptionsAllowed=0`. These are the highest-value targets for RS scale-down assistance because the node is already committed to disruption and getting replica pods off unblocks the PDB faster. Draining nodes receive a fixed minimum int32 deletion cost. They are not sequentially ranked.
- **Drifted:** Nodes with `ConditionTypeDrifted=True` and no do-not-disrupt pods. These nodes need replacement for compliance or security reasons. Draining them first via RS scale-down means scale-down events naturally assist drift progress. Nodes are ranked by consolidation priority.
- **Disruptable:** Nodes that are not drifted and have no do-not-disrupt pods. Standard consolidation targets. Nodes are ranked by consolidation priority.
- **Not Disruptable:** Nodes that Karpenter cannot or should not consolidate: those with do-not-disrupt pods, nodes marked do-not-disrupt, or nodes in NodePools with no consolidation budget (`consolidateAfter` is nil). Deleting pods from them has zero consolidation or drift value. Managed annotations are cleared so the RS controller applies its default behavior.

Only Drifted and Disruptable nodes receive sequential negative ranks such that newly created pods without managed annotations always have a higher default deletion cost (e.g. default is 1 in Karpenter or 0 in Replicaset controller). Draining nodes get a fixed minimum value. Not Disruptable nodes have annotations removed entirely. The ReplicaSet controller removes pods from draining nodes first (unblocking in-progress operations), then drifted nodes, then normal consolidation targets, and protected nodes last.

### Why Karpenter is the right place for this

Karpenter already maintains the cluster state (`state.Cluster`) needed to rank nodes. A standalone controller would duplicate this informer cache, adding redundant API server watch load. The ranking function is the same function as consolidation candidate sorting; co-locating them prevents divergence between "how Karpenter picks consolidation targets" and "how pods are ranked for deletion." This controller extends Karpenter's node-level reasoning to influence ReplicaSet behavior, reusing the same sort order. It does not run the scheduling simulation or make consolidation decisions itself. It helps the ReplicaSet controller set up better consolidation opportunities by concentrating scale-down on the right nodes. Because the ranking lives inside Karpenter, future iterations can incorporate scheduling constraints like topology spread constraints without re-deriving signals independently.

### Evidence

Community issues and production reports consistently point to excessive disruption as a pain point:

- [#2123](https://github.com/kubernetes-sigs/karpenter/issues/2123) -- Karpenter scaling disproportionately high nodes, over-provisioning then consolidating, creating unnecessary churn
- [aws/karpenter#4356](https://github.com/aws/karpenter/issues/4356) -- Constant pod disruption from Karpenter replacing nodes with cheaper ones even when savings were marginal
- [aws/karpenter#3785](https://github.com/aws/karpenter/issues/3785) -- After peak hours, consolidation was too slow, but aggressive consolidation caused massive pod churn
- [aws/karpenter#3927](https://github.com/aws/karpenter/issues/3927) -- Production clusters with 100+ pods per node couldn't consolidate without disrupting running workloads

Benchmark experiments and simulations show meaningful decreases in disruption rate when using pod deletion cost annotations. The pod disruption tracking being added in [kubernetes-sigs/karpenter#2892](https://github.com/kubernetes-sigs/karpenter/pull/2892) will provide the metrics infrastructure to measure and validate these improvements in production. The effectiveness varies by deployment pattern: clusters with a few large deployments see the strongest benefit, while clusters with many small deployments (where each scale-down removes only 1-2 pods) see less improvement. These gains compound when combined with other proposed features such as consolidation disruption cost thresholds. This feature also pairs well with the "MostAllocated" scoring strategy for kube-scheduler, which concentrates pods onto fewer nodes and amplifies the consolidation benefit.

## Goals

- Measurably reduce voluntary pod disruption rate for clusters using Deployments/ReplicaSets, as tracked by the pod disruption metrics in [#2892](https://github.com/kubernetes-sigs/karpenter/pull/2892)
- Maintain or improve total compute cost and utilization efficiency across all benchmarks
- Minimal impact on API server load from annotation updates

### Value by consolidation policy

For ConsolidateWhenEmpty NodePools, concentrating pod deletions on specific nodes improves bin-packing by creating fully-empty nodes that qualify for removal. Without this controller, scale-down events rarely produce empty nodes. For ConsolidateWhenUnderutilized NodePools, concentrating deletions means fewer pods need to be evicted when Karpenter consolidates, reducing the total disruption per consolidation move.

## Proposal

We introduce a new feature-gated controller that automatically manages the `controller.kubernetes.io/pod-deletion-cost` annotation on pods running on Karpenter-managed nodes. The controller ranks nodes using Karpenter's disruption cost heuristic, assigns deletion cost values to pods so that Kubernetes' ReplicaSet scale-down logic preferentially removes pods from the best consolidation targets first, and partitions nodes Karpenter cannot act on separately to protect them from early eviction.

### How it works

The controller reconciles on a periodic interval, gated behind the `PodDeletionCostManagement` feature gate (the reconciler does not run when the gate is disabled). Where the data for this controller lives is an implementation detail, but the controller could reuse the existing `state.Cluster` data structures and functions to perform its ranking. On each reconcile it:

1. Collects all Karpenter-managed nodes
2. Runs change detection (compares `ConsolidationState` timestamp). If nothing changed, skips with zero API writes
3. Partitions nodes into Draining, Drifted, Disruptable, and Not Disruptable
4. Ranks Drifted and Disruptable nodes by the current Karpenter consolidation candidate ranking function. Draining nodes get a fixed minimum value. Not Disruptable nodes are excluded from ranking.
5. For each NodePool, the number of nodes eligible for annotation is limited by that NodePool's disruption budget. Since drift and consolidation can have separate budgets per NodePool, the controller respects each independently: Drifted nodes are bounded by the drift disruption budget, Disruptable nodes are bounded by the consolidation disruption budget. Only nodes Karpenter could actually act on get annotated. Across all NodePools, a hard cap of 50 nodes per cycle prevents excessive labeling when budgets are permissive.
6. For each eligible node's pods, writes the pod-deletion-cost annotation along with ownership and conflict-detection annotations (the three-annotation protocol described in Risks). Skips pods with customer-set deletion costs. Nodes that drop out of scope have their managed annotations cleaned up.

The hard cap of 50 nodes is the alpha default; we will collect feedback and adjust for beta. Implementation may use sparse numbering to reduce the number of annotation updates needed when individual nodes are added or removed from the ranking.

### API changes

No CRD changes. The feature is purely controller-side, gated behind `PodDeletionCostManagement` and configured via CLI flags / environment variables. RBAC is extended to add `update` and `patch` verbs on pods. The controller only processes pods on Karpenter-managed nodes, and the feature gate ensures the code path is dormant unless enabled. A future refinement could use server-side apply with a dedicated field manager to narrow the effective scope.

This RFC also introduces `karpenter.sh/disruption-cost` as a new user-facing annotation (see Migration section below for details and precedence rules).

### Configuration

| Option | CLI Flag | Env Var | Default | Description |
|--------|----------|---------|---------|-------------|
| Feature gate | `--feature-gates=PodDeletionCostManagement=true` | `FEATURE_GATES` | `false` | Enables the controller |

### Example: Scale-down concentrates deletions on the consolidation target

A cluster with 3 nodes runs a Deployment with 9 replicas (3 pods per node). The operator enables the controller:

```
Node A (m5.xlarge):  3 pods
Node B (m5.2xlarge): 3 pods
Node C (m5.large):   3 pods
```

All nodes have the same disruption cost, so the ranking engine breaks ties by node name. With 3 nodes, it assigns: Node A -> rank -3 (consolidation target), Node B -> -2, Node C -> -1.

**Without the controller:** Scale from 9 to 6 replicas. The ReplicaSet controller spreads deletions: 1 pod from each node. All 3 nodes still have 2 pods. Karpenter can still consolidate, but must evict pods to do so, increasing total disruption. No node moved closer to empty, so the scale-in didn't help consolidation at all.

**With the controller:** The ReplicaSet controller sees all 3 pods on Node A have the lowest deletion cost (-3). It removes all 3 from Node A. Node A is now empty. Karpenter immediately consolidates it with zero disruption. The cluster goes from 3 nodes to 2.

Additional examples (partial drain convergence, drifted node draining, disrupted + PDB-blocked priority) are in Appendix B.

### Migration: separating consolidation steering from RS coordination

Some Karpenter users currently set `controller.kubernetes.io/pod-deletion-cost` on their pods to influence which pods Karpenter prefers to evict during consolidation. With this controller also writing pod-deletion-cost for RS coordination, the same annotation now serves two purposes. To resolve this, we introduce `karpenter.sh/disruption-cost` as the user-facing annotation for steering consolidation behavior.

The consolidation scoring precedence becomes:
1. `karpenter.sh/disruption-cost` (if set by user)
2. `controller.kubernetes.io/pod-deletion-cost` (only if NOT auto-managed by this controller)
3. Default

When the deletion cost controller is enabled and manages a pod's deletion cost (sentinel annotation present), consolidation scoring ignores that pod's `pod-deletion-cost` since it reflects RS ranking, not user intent. Users who want to protect specific pods from consolidation should migrate to `karpenter.sh/disruption-cost`.

## Observability

The controller exposes the following Prometheus metrics:

- `karpenter_pod_deletion_cost_nodes_ranked` (gauge): Number of nodes ranked in the most recent cycle
- `karpenter_pod_deletion_cost_pods_updated_total` (counter, labels: result=[updated|skipped_customer|skipped_unchanged|error]): Pod annotation outcomes per cycle
- `karpenter_pod_deletion_cost_ranking_duration_seconds` (histogram): Time to compute node rankings
- `karpenter_pod_deletion_cost_annotation_duration_seconds` (histogram): Time to write pod annotations
- `karpenter_pod_deletion_cost_reconcile_skipped_total` (counter): Cycles skipped due to no state change

A high skip rate indicates stable cluster state. Zero skips combined with high annotation duration indicates write pressure. No Kubernetes events are emitted by this controller (events are more costly than metrics for a periodic reconciler).

## Rollout and Graduation

- Alpha: gate off by default, no stability guarantees. Hard cap of 50 nodes.
- Beta: gate on by default, API stable, cleanup-on-disable implemented, evaluate topology-aware ranking factors, adjust node cap based on feedback.
- GA: evaluate always on vs keeping the feature gate going forward.
- Rollback from beta: disabling the gate stops new annotation writes. Existing annotations persist on pods until those pods are replaced. Cleanup-on-disable (removing managed annotations when the gate is turned off) is a beta deliverable to make rollback clean.
- Migration requires no action on upgrade; enabling requires RBAC review and annotation audit.

## Alternatives Considered

### Rank pods directly instead of inheriting node rank

Pod-level ranking spreads ReplicaSet deletions across many nodes rather than concentrating them on the node Karpenter intends to consolidate next. In the 3-node/9-pod example, pod-level ranking could remove 1 pod from each of 3 nodes, leaving all 3 still occupied. There's a coherence problem: if two pods on the same node get very different deletion costs, the ReplicaSet controller might partially drain a node, which doesn't help Karpenter because it must still evict the remaining pod(s) to consolidate, adding disruption that the scale-in could have avoided. Node-rank inheritance ensures all pods on a target node are aligned in terms of deletion priority/probability.

### Standalone controller outside Karpenter

The ranking strategies that matter most require the same cluster state Karpenter already maintains in its state.Cluster informer cache. A standalone controller would duplicate this state and API server watch load. If the ranking logic evolves to directly consume Karpenter's consolidation scoring, that integration is trivial when they share a process. The operational cost is also higher: separate RBAC, Helm chart, release lifecycle, etc. For a feature tightly coupled to consolidation behavior, co-location is the right call, but the controller could certainly be kept independent and still work the same.

The ranking function should be factored into a shared location that the consolidation controller can also consume, establishing a single source of truth for "how much do we want to consolidate this node." This avoids drift between the deletion cost controller's ranking and consolidation candidate selection.

## Risks and Mitigations

- **Stale annotations after controller is disabled (Medium):** Pods retain annotations indefinitely when the feature gate is turned off. *Mitigation:* Annotations live on pods, which are ephemeral. New pods start clean. Staleness is bounded by pod lifetime. Cleanup-on-disable is a beta deliverable that will actively remove managed annotations when the gate is turned off.

- **Consolidation-optimized deletions may temporarily violate topology spread constraints (Low):** When the controller concentrates deletions on specific nodes, the resulting pod distribution may temporarily violate `topologySpreadConstraints` until the scheduler places replacement pods. This is the same behavior as the current spreading heuristic; neither approach guarantees constraint satisfaction during scale-down. The Kubernetes scheduler enforces topology spread when placing new pods, so any temporary imbalance is corrected on the next scheduling cycle. Operators with strict spreading requirements can leave the feature disabled. Graduation criteria: "Beta: evaluate whether topology-aware ranking factors should be added."

- **Annotation conflicts with customer-set pod deletion costs (Medium):** An operator or another controller may have already set `controller.kubernetes.io/pod-deletion-cost` on pods. If Karpenter overwrites those values, it silently breaks the operator's intended behavior. *Mitigation:* Three-annotation protocol. Karpenter sets three annotations when managing a pod: (1) `controller.kubernetes.io/pod-deletion-cost: "<rank>"` for the RS controller, (2) `karpenter.sh/managed-deletion-cost: "true"` as an ownership flag, (3) `karpenter.sh/last-assigned-deletion-cost: "<rank>"` recording what Karpenter last wrote. If a pod has a deletion cost but lacks the ownership flag, it is treated as customer-managed and skipped. If a pod has the ownership flag but its current pod-deletion-cost differs from last-assigned, a third party changed the value and Karpenter yields control (removes its annotations, skips the pod). This mechanism survives controller restarts because the expected value is persisted on the pod itself. Karpenter-managed ranks are always negative (starting at -n), so customer-set positive values naturally take precedence without special handling.

## Open Questions

1. How to incorporate other priorities into the node rankings (e.g. topology or other scheduling constraints). Planning to address this for beta.

## Appendix A: Security and Performance Implications

### Security

- **RBAC expansion:** Karpenter's ClusterRole gains `update` and `patch` on pods (cluster-wide). Minimum privilege needed for annotation management. Operators with tightly scoped RBAC should review.
- **No secrets or sensitive data:** Annotations contain only integer rank values and a boolean flag.
- **No new network access:** Communicates only with the Kubernetes API server using the existing service account.
- **Annotation injection:** A malicious actor with pod write access could set `karpenter.sh/managed-deletion-cost: "true"` to trick Karpenter into managing their deletion cost. Low severity since the attacker already needs pod write access.

### Performance

- **API server write load:** Each reconcile annotates pods on at most 50 nodes (hard cap). With an average of 30 pods/node, worst case is ~1,500 pod annotation writes per cycle. ConsolidationState-based change detection skips the cycle entirely when cluster state is unchanged (zero API writes in steady state). The change detection itself is O(1) with zero API calls.
- **Memory:** Negligible. References existing `state.StateNode` objects. Ranking data structures are O(n) and transient.
- **CPU:** O(n log n) for sorting nodes by disruption cost. Change detection is O(1) timestamp comparison.
- **Watch event amplification:** Annotation updates trigger watch events for other controllers. Bounded by the reconcile interval. Annotation changes don't affect fields Karpenter's consolidation controller uses for decisions.

## Appendix B: Additional Examples

### Partial drain converges over multiple scale-down events (9 to 7)

Same starting state: 3 nodes, 9 replicas (3 per node), Node A ranked lowest (-3).

**Scale from 9 to 7 replicas.** The ReplicaSet controller removes 2 pods, both from Node A (lowest deletion cost). State after scale-down:

```
Node A: 1 pod   (rank -3)
Node B: 3 pods  (rank -2)
Node C: 3 pods  (rank -1)
```

Node A isn't empty yet, so Karpenter can't consolidate it under a WhenEmpty policy. But Node A still has the fewest pods, so it retains the lowest rank on the next reconcile. On the next scale-down event (e.g., 7 to 5), the ReplicaSet controller again targets Node A first, removing the remaining pod. Node A becomes empty and Karpenter consolidates it. Even partial drains converge toward empty nodes over successive scale-down events because the ranking is stable.

### Drifted node drains first

A cluster with 3 nodes. Node A is drifted (pending AMI replacement):

```
Node A (m5.xlarge):  3 pods, drifted (ConditionTypeDrifted=True)
Node B (m5.2xlarge): 3 pods, normal
Node C (m5.large):   3 pods, normal
```

Ranking:

```
Drifted:
  Node A (3 pods) -> rank -3

Disruptable:
  Node B (3 pods) -> rank -2
  Node C (3 pods) -> rank -1
```

**Scale from 9 to 6 replicas.** The ReplicaSet controller removes all 3 from Node A (lowest cost). Node A is now empty. Karpenter consolidates it with zero disruption and the drifted node is replaced.

### Draining node gets highest priority

A cluster with 3 nodes. Node A has already been committed to disruption (has `karpenter.sh/disrupted` taint) but its drain is blocked because a PDB prevents evicting the last replica of a service:

```
Node A (m5.xlarge):  3 pods, draining + PDB-blocked (DisruptionsAllowed=0)
Node B (m5.xlarge):  3 pods, drifted (ConditionTypeDrifted=True)
Node C (m5.2xlarge): 3 pods, normal
```

Ranking:

```
Draining:
  Node A (3 pods) -> fixed MinInt32

Drifted:
  Node B (3 pods) -> rank -2

Disruptable:
  Node C (3 pods) -> rank -1
```

**Scale from 9 to 6 replicas.** The ReplicaSet controller removes pods from Node A first. Once a replica pod is removed, the PDB's `DisruptionsAllowed` increases, unblocking Karpenter's drain. The stalled disruption completes without additional Karpenter-initiated evictions.

Draining nodes rank above Drifted because a draining node represents a stalled operation where Karpenter has already committed resources but cannot complete. Drifted nodes also need action but aren't actively stalled.

## Appendix C: ReplicaSet Controller KEP Comparison

The [kubernetes/enhancements#5982](https://github.com/kubernetes/enhancements/issues/5982) KEP proposes adding a `ConsolidatingScaleDown` feature gate to `kube-controller-manager`. When enabled, the ReplicaSet controller's pod deletion sort order changes to prefer deleting pods on nodes with fewer total active pods (a consolidation heuristic), reversing the current spreading heuristic.

**This RFC (short-term):**
- Ships entirely within Karpenter (no upstream Kubernetes changes required)
- Works with all existing Kubernetes versions that support pod-deletion-cost
- Can be feature-gated and iterated on independently
- Tradeoff: operational complexity (annotation management, change detection, three-annotation protocol)

**KEP (on hold):**
- Eliminates the need for an external annotation-management controller
- Zero API server overhead (no annotation writes, no reconcile loop)
- Applies universally to all ReplicaSets, not just those on Karpenter-managed nodes
- Tradeoff: upstream timeline (KEP, sig-apps review, multi-release graduation)
- Also not a great place for centralizing all the different factors (autoscaling/scheduling) when picking which pods to scale-in.

## Appendix D: Future Work

### Dedicated API for disruption preferences

Multiple systems may want to influence pod deletion priority: the node autoscaler (consolidation targets), a drift controller (nodes needing replacement), a traffic shaper (replicas already being drained), or the scheduler (topology-aware candidates). Today these would all fight over a single annotation.

A dedicated API object (for example, a `PodDisruptionPreference` resource or a new field on NodeClaim) would let each system express its input independently using server-side apply with distinct field managers. A reconciler would merge these inputs into the final `pod-deletion-cost` annotation that the RS controller consumes.

### Scheduler library integration

As Karpenter upstreams into the kube-scheduler via the scheduler library, the merging of scale-in signals could happen inside the scheduler itself. The scheduler already has scheduling context (topology spread, affinity, zone distribution). Adding scale-in awareness would let it produce deletion priorities that account for both scheduling constraints and consolidation goals, without requiring an external annotation-management loop.

### Deprecation path

Once a proper API or scheduler integration exists, the annotation-management controller described in this RFC can be deprecated. The controller is designed to be replaceable: it writes standard `pod-deletion-cost` annotations that any future mechanism would also produce. No workload changes would be needed when migrating to a better signal delivery mechanism.
