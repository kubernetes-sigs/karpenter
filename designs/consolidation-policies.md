# Consolidation Policies

## Problem Statement

Karpenter provides 3 forms of consolidation: single-node, multi-node, and emptiness consolidation. Single-node replaces expensive nodes with cheaper nodes that satisfy the workloads requirements. Multi-node replaces multiple nodes with a single, larger node that can satisfy workload requirements. Emptiness removes nodes that have workloads that can fit onto other nodes to reduce costs.

Customers want to have control over the types of consolidation that can occur within a nodepool. Some nodepools may only be used for job type workloads (crons, jobs, spark, etc) where single-node consolidation is dangerous and wastes execution when it disrupts pods. Other nodepools may be used mostly by long-running daemons, where daemonset costs are more significant and multi-node consolidation to binpack into larger nodes would be preferred.

Karpenter's consolidation is an all or nothing offering today. Nodepools can disable consolidation and let only empty nodes be removed, or customers can opt into all forms of consolidation.

## Design

### Option 1: Refactor and Expand Consolidation Policies

Karpenter already presents the binary consolidation options. The `NodePool` resource has a disruption field that exposes the `consolidationPolicy` field. This field is an enum with two supported values: `WhenEmpty` and `WhenUnderutilized`.

The proposal is to add two new consolidation policy enum values:

* `WhenCheaper`
* `WhenUnderutilizedOrCheaper`

And change the semantics of the existing consolidation policy enum value:

* `WhenUnderutilized`

The semantics of each of the enum values matches their names, making it straightforward to explain to customers or users. `WhenUnderutilizedOrCheaper` becomes the new default. `WhenUnderutilizedOrCheaper` is the same as `WhenUnderutilized` previously. `WhenUnderutilized` semantics changes to only allow emptiness or multi-node consolidation to occur for a nodepool. `WhenCheaper` only allows emptiness or single-node consolidation to occur for a nodepool.

#### Pros/Cons

* 👍 Simple configuration for the `NodePool` crd
* 👍 Simple implementation for the various consolidation controllers.
* 👎 If Karpenter adds consolidation modes with new purposes, more `consolidationPolicy` enum values will be needed to make them toggleable.

### Option 2: Add Consolidation Configuration to NodePools

Karpenter's `NodePool` resource could expose fine-grained consolidation controls to allow configuring each controller, as well as controller-specific subfields (e.g multi-node consolidation's `maxParallel` setting). 

A possible `NodePool` partial resource might look like this:
```yaml
 spec:
  disruption:
    consolidation:
      singleNode:
        enabled: true
      multiNode:
        enabled: true
        maxParallel: 100
      emptiness:
        enabled: true
        emptyThreshold: "30%"
```
#### Pros/Cons

* 👍 Fine-grained controls for the various consolidation controllers.
* 👎 Complex configuration for the `NodePool` crd
* 👎 Complex implementation for the various consolidation controllers.
* 👎 Limits Karpenter's ability to change / remove existing consolidation controllers, as config will be tightly coupled.
