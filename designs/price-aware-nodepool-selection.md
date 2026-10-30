# Price-aware nodepool selection

<!-- One-line summary of what this RFC proposes. Name it after the feature, not the solution. -->

## Motivation

<!-- Tell the story: what's the current state, why is it a problem, and who is affected?
     Link to relevant GitHub issues to ground the problem in real user requests.
     Keep it brief — if the reader doesn't understand the problem, the solution won't land. -->

Resolves: https://github.com/kubernetes-sigs/karpenter/issues/3506

A Karpenter nodepool is a powerful tool to schedule the cheapest nodes in your cluster.
However, when two nodepools have equal weights, a tie breaker kicks in and the nodepool that sits later in the alphabet is chosen. This works fine in a lot of cases, but does not cover a common usecase: choosing the cheaper nodepool.
When running Kubernetes in cloud, it is common to use spot instances to save on compute costs.
But most companies tend to go for pre-paid, dollars-per-hour commitment to on-demand compute through a contract with their Cloud provider or reseller. 
Unused commitment is already paid for. This means that the on-demand compute would be "cheaper" than spot nodes.

[Currently, Karpenter has no "native" support for "Price-Aware" provisioning ](https://github.com/kubernetes-sigs/karpenter/issues/2117). 
People have tried to solve this at the provider level.
- AWS's provider supports ODCRs and Capacity Blocks, [but not Compute Savings Plans](https://github.com/aws/karpenter-provider-aws/issues/2259). 
- Azure's provider supports Capacity Reservation Groups, but not Savings Plan for Compute.
These nodes will then be picked over `spot` instances as `reserved` instances, until capacity is reached.

But this is not general enough for other Karpenter providers, and even worse is very specific to specific APIs in each providers respective Cloud and only covers guaranteed on-demand compute, not pre-paid commitments.
Karpenter does not read billing or commitment data.

But it doesn't need to necessarily. 
Instead, a simpler approach could be adopted.
When comparing Nodepools of equal weight, lowest price should be the deciding factor between them.

Ideally, the user would be able to create two or more nodepools, of equal weight, where price is the deciding factor in what gets picked.
This would ensure price aware nodepool selection.
Combined with Node Overlays, and automation, it would be a solution to the pre-paid commitment.
This would also allow for more complex multi-nodepool setups, while still ensuring the cheapest nodes get provisioned.
And all of this without integrating with cloud specific billing APIs.

### Use Cases

1. Two equal weight nodepools, on-demand versus spot

I have two node pools. One is on-demand, called `a-on-demand`. The other is spot, called `z-spot`. 
Even with a Node Overlay on `a-on-demand`, indicating capacity in my $/h commitment, z-spot will be chosen over `a-on-demand`.
Adjusting the weights, I would force all pods that fit both nodepools onto the higher weighted one. 
My savings plan is underutilized, and my already committed and paid money is wasted.

2. Several equal-weight spot pools across instance families.

// TODO elaborate
<!-- 2-4 concrete scenarios showing how users hit this problem today.
     Each should be specific enough to validate proposed solutions against. -->

### Non-Goals


<!-- What does this RFC explicitly NOT address? Defining boundaries prevents scope creep
     during review and sets expectations for what is left for future work. -->

- Balancing between identical pools
- Cloud specific Billing and Commitment integration


## Proposal

A feature gate to enable this Price-Aware Nodepool selection.

// TODO elaborate

<!-- Describe the recommended solution. Lead with the user-facing API or behavior change,
     then explain how it works internally. Include YAML specs, Go structs, or diagrams
     as appropriate. This section should be self-contained — a reader should be able to
     understand what ships without reading Alternatives. -->

### Proposed Spec

<!-- Show the API surface: CRD YAML, Go type definitions, annotation formats, CLI examples.
     Omit if the change has no user-facing API. -->

### How It Works

// TODO figure out where to put this
Gate on against gate off (interleaved, 10 rounds each, 2,000 pods):

Equal-weight NodePools	5 workers	1 worker (default installs)
2	+3.0% (p=0.019)	+3.3% (p=0.001)
5	no difference (p=0.19)	+5.2% (p=0.002)
10	no difference (p=0.74)	no difference (p=0.17)
geomean		+2.3%


<!-- Explain the implementation at design level. Cover the controller loop, scheduling
     interactions, or algorithm changes. Not a code walkthrough — just enough that
     another contributor could implement it from this description. -->

### Interaction with Existing Features

<!-- How does this compose with consolidation, drift, disruption budgets, PDBs, etc.?
     Call out anything that changes existing behavior. -->

This might change behaviour in nodepools that have a lot of equal weight node pools,
Espescially if consolidation is frequent, and the cluster has many spot node pools with strictly defined but varying instance types.

This feature really shines when combined with Node Overlays.
Through automation like Nextdoor's Lumina and Veneer,
users can ensure that their cloud compute commitment does not go wasted.

One can also enable the Node Overlay alpha feature gate, and discount node pools to manipulate which nodepool Karpenter chooses.
If you had two equall weight nodepools, one on-demand and one spot, and added a node overlay to the on-demand one to represent available capacity, the equal-weight tie-breaker would kick in and it would choose the nodepool that sits later in the alphabet.

### Observability



<!-- What metrics, events, status conditions, or log lines help users understand
     the feature is working? Omit if not applicable. -->

// TODO look into if metrics or events are applicable here

### Edge Cases

<!-- Known corner cases and how the design handles them. A few worked examples
     are more valuable than exhaustive enumeration. -->

// TODO Look into the following: 
A pod fits only one pool.
Prices are equal.
An overlay is deleted while scheduling is running.

## Alternatives Considered

<!-- For each alternative: state what it is, how it works (briefly), and why it was
     rejected. The goal is to show the reader that obvious alternatives were evaluated,
     not to re-derive the solution. -->

### Alternative 1: Capped node pools

One can create an on-demand only nodepool, with a higher weight, and add at a cap on how many vCPUs can be used there.
The Cap can then be adjusted to represent how many vCPUs the $/h commitment can buy you.
This however, does not account for varying prices, discounts or reservations. 
Neither does it scale well across multiple clusters and accounts either.
And, it is very manual.
To quote PeterIvanov: https://github.com/aws/karpenter-provider-aws/issues/2259#issuecomment-3398172325

> The only workaround I know at the moment is: to use weighted on-demand node group for the capacity you have reserved. But it requires micro-managing every node group in many clusters, which all share the same SP budget, and the exact settings, in principle, need to be changed after every small change in SPs or in capacity usage pattern. Seeems like a brittle contraption to me.

### Alternative 2: Mixed Node-pools

.. but the Node Overlay does not change the behaviour of mixed nodepools, on-demand will always come after spot.

## Backward Compatibility

<!-- What happens to existing users? Are there migration steps? Does existing YAML
     continue to work unchanged? -->

This feature should not change any API and YAMLS will continue to work unchanged.

## Graduation Criteria

- Alpha: What makes this too risky is that it changes behaviour of how Nodepools are picked for pods. 
- Beta: People rolling this out to bigger clusters, feedback on observed behaviour.
- GA: 

<!-- Not all features need a feature gate. If the change is low-risk and backwards
     compatible, it may ship enabled by default.

     If gated, describe what's needed at each stage:
     - Alpha: What makes this too risky to enable by default?
     - Beta: What feedback or stability proof is needed before enabling by default?
     - GA: What's needed before removing the gate entirely? -->

## Open Questions

Can this ever be turned on by default? I am not sure if users will prefer this behaviour, and this might want to sit completely behind a feature gate.
But then maintaining two branches in the code is also extra work.

<!-- Unresolved decisions that need feedback or data before implementation.
     Number them so reviewers can reference them easily. -->
