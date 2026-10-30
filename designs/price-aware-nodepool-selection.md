# Price-aware nodepool selection

<!-- One-line summary of what this RFC proposes. Name it after the feature, not the solution. -->

## Motivation

<!-- Tell the story: what's the current state, why is it a problem, and who is affected?
     Link to relevant GitHub issues to ground the problem in real user requests.
     Keep it brief — if the reader doesn't understand the problem, the solution won't land. -->


A Karpenter nodepool is a powerful tool to schedule the cheapest nodes in your cluster.
However, when two nodepools have equal weights, an tie breaker kicks in and the nodepool that sits later in the alphabet is chosen. This works fine in a lot of cases, but does not cover a common usecase: choosing the cheaper nodepool.
When running Kubernetes in cloud, it is common to use spot instances to save on compute costs.
But most companies tend go for pre-paid, dollars-per-hour commitment to on-demand compute through a contract with their Cloud provider or reseller. This means that the on-demand compute is cheaper, up to a point, than spot nodes.

Currently, Karpenter has no "native" support for comparing reserved on-demand usage against spot prices types. Individual providers support some notion of reserved instances however. 
AWS's provider supports ODCRs and Capacity Blocks, but not Compute Savings Plans. 
Azure's provider supports Capacity Reservation Groups, but not Savings Plan for Compute.
GCP's communitu maintained provider does not seem to have any real support either.

There are ways around this, depending on use case. Reserved instances, like ODCRs, will be prioritized in mixed nodepools, over spot,  until capacity is met. But spot will still be chosen over on-demand.
And this does not cover Compute Savings Plans, as stated earlier.

In that use case, one can create an on-demand only nodepool, with a higher weight, and add at a cap on how many vCPUs can be used there. The Cap can then be made represent how many vCPUs the $/h commitment can buy you. This however, does not account for varying prices, discounts or reservations. 
Neither does it scale well across multiple clusters and accounts either.

One can also enable the Node Overlay alpha feature gate, and discount node pools to manipulate which nodepool Karpenter chooses.
But the Node Overlay does not change the behaviour of mixed nodepools, on-demand will always come after spot.
If you had two equall weight nodepools, one on-demand and one spot, and added a node overlay to the on-demand one to represent available capacity, the equal-weight tie-breaker would kick in and it would choose the nodepool that sits later in the alphabet.

This is where my motivation comes in.

Ideally, the user would be able to create two or more nodepools, of equal weight, where price is the deciding factor in what gets picked.
This would ensure price aware nodepool selection. and combined with Node Overlays, on-demand pools with CPU caps, or automation integrated with cloud provider APIs, can be added to dynamically steer workloads to nodepools based on your own organizations' usage and availability.


### Use Cases

1. Two equal weight nodepools, on-demand versus spot

I have two node pools. One is on-demand, called `a-on-demand`. The other is spot, called `z-spot`. 
Even with a Node Overlay on `a-on-demand`, indicating capacity in my $/h commitment, z-spot will be chosen over `a-on-demand`.
Adjusting the weights, I would force all pods that fit both nodepools onto the higher weighted one. 
My savings plan is underutilized, and my already committed and paid money is wasted.

2. 

<!-- 2-4 concrete scenarios showing how users hit this problem today.
     Each should be specific enough to validate proposed solutions against. -->

### Non-Goals

- Balancing between identical pools
<!-- What does this RFC explicitly NOT address? Defining boundaries prevents scope creep
     during review and sets expectations for what is left for future work. -->

## Proposal

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

### Observability

<!-- What metrics, events, status conditions, or log lines help users understand
     the feature is working? Omit if not applicable. -->

### Edge Cases

<!-- Known corner cases and how the design handles them. A few worked examples
     are more valuable than exhaustive enumeration. -->

## Alternatives Considered

<!-- For each alternative: state what it is, how it works (briefly), and why it was
     rejected. The goal is to show the reader that obvious alternatives were evaluated,
     not to re-derive the solution. -->

### Alternative 1: ...

### Alternative 2: ...

## Backward Compatibility

<!-- What happens to existing users? Are there migration steps? Does existing YAML
     continue to work unchanged? -->

## Graduation Criteria

<!-- Not all features need a feature gate. If the change is low-risk and backwards
     compatible, it may ship enabled by default.

     If gated, describe what's needed at each stage:
     - Alpha: What makes this too risky to enable by default?
     - Beta: What feedback or stability proof is needed before enabling by default?
     - GA: What's needed before removing the gate entirely? -->

## Open Questions

<!-- Unresolved decisions that need feedback or data before implementation.
     Number them so reviewers can reference them easily. -->
