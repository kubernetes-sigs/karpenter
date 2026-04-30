# Impact of the Kubernetes Scheduler Score Plugin on Karpenter

## Motivation

Karpenter and `kube-scheduler` make pod-placement decisions independently, on different inputs:

- **`kube-scheduler`** decides which existing node a *Pending* pod binds to. It does this by running a Filter pass (which nodes are feasible) and a Score pass (rank the feasible nodes). The default Score configuration in upstream `kube-scheduler` uses the [`NodeResourcesFit`](https://kubernetes.io/docs/reference/scheduling/config/#scheduling-plugins) plugin in **`LeastAllocated`** mode: among feasible nodes, prefer the one with the most free CPU/memory.
- **Karpenter's provisioner** decides when a pod can't bind to any existing node. It then bin-packs all pending pods onto a hypothetical new node (or set of nodes) and submits the corresponding NodeClaim(s). This is a `MostAllocated`-style decision by construction: Karpenter prefers to fill one bigger instance over scattering pods across smaller ones.
- **Karpenter's consolidation controller** continuously evaluates nodes that could be removed or replaced with a cheaper option without leaving any pod un-schedulable. The set of disposable nodes is largely determined by how densely `kube-scheduler` packs pods onto the existing fleet.

This means the score plugin running inside `kube-scheduler` directly affects how often Karpenter's consolidation controller can find a profitable move. Issue [#1228](https://github.com/kubernetes-sigs/karpenter/issues/1228) hypothesises that this effect is non-trivial, and that operators running with the upstream defaults pay for it in disruption and cost. This doc examines the interaction and gives concrete guidance.

## Background: scoring modes

The relevant `kube-scheduler` Score plugin is `NodeResourcesFit`. Its `scoringStrategy.type` field selects one of:

| Mode | Behaviour | Result on a fleet of partially-utilized nodes |
|---|---|---|
| `LeastAllocated` (default) | Score increases as the node's allocated/allocatable ratio decreases. | Pods spread evenly. No single node tends to fully drain. |
| `MostAllocated` | Score increases as the node's allocated/allocatable ratio increases. | Pods bin-pack onto already-busy nodes. Some nodes drain naturally. |
| `RequestedToCapacityRatio` | Operator-defined function over the requested/capacity curve, with optional per-resource shaping. | Tunable; can approximate either of the above. |

Configuration is via `KubeSchedulerConfiguration` (a cluster-level config). Most managed Kubernetes offerings (EKS, GKE Autopilot, AKS) don't expose this directly — see [References](#references) for workarounds.

## How `LeastAllocated` works against Karpenter consolidation

Karpenter consolidation is triggered when a node is empty (`WhenEmpty[OrUnderutilized]`) or when its workloads can be re-packed onto a cheaper combination of nodes (`WhenUnderutilized`). In both cases, the controller has to identify nodes that are either empty or whose pods can be migrated without leaving any pending pod un-schedulable.

`LeastAllocated` actively works against the first condition. Each new pending pod prefers the *least*-loaded node, so allocations stay balanced across the fleet. With `n` nodes at average utilization `u`, you'd typically see all `n` nodes hovering near `u` rather than a subset at high utilization and the rest near zero. The "drain a node" path that consolidation depends on rarely happens organically.

A few failure modes this interaction is hypothesised to amplify (existing reports, not yet attributed to scheduler scoring with measurements):

1. **Replacement loops on lightly-loaded fleets** ([#1019](https://github.com/kubernetes-sigs/karpenter/issues/1019), [#735](https://github.com/kubernetes-sigs/karpenter/issues/735), [#1851](https://github.com/kubernetes-sigs/karpenter/issues/1851)) — rapid node churn where consolidation deletes nodes that are immediately re-provisioned. The "no node ever fully drains" effect of `LeastAllocated` is consistent with this pattern: single-node consolidation moves keep finding a slightly-cheaper packing while the actual fleet is barely changing.
2. **Disruption of dense nodes while sparse ones survive** ([#2319](https://github.com/kubernetes-sigs/karpenter/issues/2319), [aws#8868](https://github.com/aws/karpenter-provider-aws/issues/8868)) — consolidation has been reported to evict pods from highly-utilized nodes in preference to lightly-utilized ones. `LeastAllocated` would aggravate this: a node that's about to attract the next batch of pending pods can look consolidation-eligible only because no pod has landed on it yet.

`MostAllocated` would invert both: pods stack onto already-busy nodes, sparser nodes drain over time, and consolidation finds genuine empty-or-near-empty candidates. Confirming the magnitude requires the benchmark called out in [Open questions](#open-questions--future-work).

## What Karpenter does internally

Karpenter's *provisioning* path already bin-packs (the [`scheduling.Queue`](https://github.com/kubernetes-sigs/karpenter/blob/main/pkg/controllers/provisioning/scheduling/queue.go) sorts pods to maximize packing density on the new NodeClaim). The mismatch we're documenting is at runtime, after provisioning: once Karpenter has launched a node, ongoing pod placement is `kube-scheduler`'s decision, not Karpenter's.

There is no in-tree way for Karpenter to influence `kube-scheduler`'s scoring. The interaction can only be addressed at the cluster level.

## Recommendations

| Cluster type | Recommendation |
|---|---|
| **Self-managed control plane (kubeadm, kops, k3s, …)** | Set `scoringStrategy.type: MostAllocated` in `KubeSchedulerConfiguration`. This is the simplest, highest-leverage change. See the upstream [scheduler config reference](https://kubernetes.io/docs/reference/config-api/kube-scheduler-config.v1/). |
| **Managed Kubernetes that does not expose scheduler config (EKS, GKE without Autopilot custom config, AKS)** | The control-plane scheduler is not configurable. The pragmatic option is to run a *secondary* scheduler in the cluster (with `MostAllocated`) and target latency-tolerant workloads at it via `spec.schedulerName`. The [Cloud-Agnostic Approach to Bin-Packing Pods in Managed Kubernetes](https://youtu.be/3podlIoxwyI?feature=shared&t=1378) talk from KubeCon EU 2024 walks through this pattern. |
| **Karpenter on a cluster you can fully reconfigure** | Consider `RequestedToCapacityRatio` if you want a less-aggressive bin-packer than `MostAllocated` but still want consolidation-friendly behaviour. A linear shape with weight `0` at request=0% and weight `10` at request=100% is a reasonable starting point. |

Whichever mode is in use, two operational guard-rails reduce the impact of the interaction:

- Use `karpenter.sh/do-not-disrupt: true` on pods that genuinely shouldn't move, but keep the count low. Each non-disruptable pod permanently pins its node.
- Tune `disruption.consolidateAfter` rather than disabling consolidation. A longer window (e.g. 2–5 minutes) absorbs the short-term spreading effect of `LeastAllocated` without giving up consolidation entirely.

## Open questions / future work

- A reproducible benchmark would be valuable: run the same workload mix against `LeastAllocated`, `MostAllocated`, and `RequestedToCapacityRatio` on a Karpenter-managed cluster and compare consolidation move counts, evicted pod counts, and total node-hours. Issue [#1228](https://github.com/kubernetes-sigs/karpenter/issues/1228) explicitly asks for this. The KWOK provider in this repo (`kwok/`) makes it cheap to run; a reusable test rig would let any cluster operator verify the trade-off for their workload.

## References

- Issue [kubernetes-sigs/karpenter#1228](https://github.com/kubernetes-sigs/karpenter/issues/1228) — the tracking issue this doc resolves.
- [Cloud-Agnostic Approach to Bin-Packing Pods in Managed Kubernetes](https://youtu.be/3podlIoxwyI?feature=shared&t=1378) (KubeCon EU 2024) — practical walkthrough of running a secondary `MostAllocated` scheduler.
- [`KubeSchedulerConfiguration` reference](https://kubernetes.io/docs/reference/config-api/kube-scheduler-config.v1/) — upstream API docs for the relevant config object.
- [aws/containers-roadmap#1468](https://github.com/aws/containers-roadmap/issues/1468) — feature request for EKS to expose `KubeSchedulerConfiguration`.
- Related Karpenter issues: [#1019](https://github.com/kubernetes-sigs/karpenter/issues/1019), [#735](https://github.com/kubernetes-sigs/karpenter/issues/735), [#1851](https://github.com/kubernetes-sigs/karpenter/issues/1851), [#2319](https://github.com/kubernetes-sigs/karpenter/issues/2319), [aws#8868](https://github.com/aws/karpenter-provider-aws/issues/8868).
