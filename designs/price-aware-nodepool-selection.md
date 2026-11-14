# Price-aware nodepool selection

<!-- One-line summary of what this RFC proposes. Name it after the feature, not the solution. -->

## Motivation

<!-- Tell the story: what's the current state, why is it a problem, and who is affected?
     Link to relevant GitHub issues to ground the problem in real user requests.
     Keep it brief — if the reader doesn't understand the problem, the solution won't land. -->

Tracking issue: https://github.com/kubernetes-sigs/karpenter/issues/3506

A Karpenter nodepool is a powerful tool to schedule the cheapest nodes in your cluster.
However, when two nodepools have equal weights, a tie breaker kicks in and the nodepool that sits later in the alphabet is chosen. 
This works fine in a lot of cases, but does not cover a common use case: choosing the cheaper nodepool.
When running Kubernetes in cloud, it is common to use spot instances to save on compute costs.
But many companies opt for pre-paid compute commitment.
Unused commitment is already paid for. This means that the on-demand compute would be "cheaper" than spot nodes.

[Currently, Karpenter has no "native" support for "Price-Aware" provisioning ](https://github.com/kubernetes-sigs/karpenter/issues/2117). 
People have tried to solve this at the provider level.
- AWS's provider supports ODCRs and Capacity Blocks, [but not Compute Savings Plans](https://github.com/aws/karpenter-provider-aws/issues/2259). 
- Azure's provider supports Capacity Reservation Groups, but not Savings Plan for Compute.
These two Karpenter providers provide some basic capability to prioritize reserved compute, until capacity is reached.

But this is not general enough for other Karpenter providers. It is very specific to specific APIs in each providers respective Cloud and only covers guaranteed on-demand compute, not billing commitments.
Karpenter does not read billing or commitment data.

But it doesn't need to necessarily. 
Instead, a simpler approach could be adopted.

Today, if you had two equal weight nodepools, one on-demand and one spot, and added a node overlay to the on-demand one to represent available capacity, the equal-weight tie-breaker would kick in and it would choose the nodepool that sits later in the alphabet.
Another option is, when comparing Nodepools of equal weight, lowest price being the deciding factor between them.

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
Node Pools are ordered by weight, but in the case of equal weight, the tie-breaker becomes pricing.
Price, in this case, would be the cheapest the cheapest available, compatible offering of the first capacity type, including Node Overlay adjustments.
Capacity type ordering is preserved, and in the case of equal prices, fall back to the reverse alphabetical order, as it works today.

Enabling the gate replaces no existing nodes.
This will only apply to new NodeClaims.

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

It is important that the gate does not move running pods and doesn't trigger a disruption on its own.
This feature will only act on new Node Claims, so consolidation is the only path that revisits a pod's placement __based on price__.
So the pricing-sensitive rollout will be gradual relative to your consolidation configurations.

This might change behaviour in nodepools that have a lot of equal weight node pools,
Especially if consolidation is frequent, and the cluster has many spot node pools with strictly defined but varying instance types.

This feature really shines when combined with Node Overlays.
Through automation like Nextdoor's Lumina and Veneer,
users can ensure that their cloud compute commitment does not go wasted.

However, the automation across many clusters and accounts will always lag behind the compute commitment usage.
That is of course the nature of APIs and interacting systems like this, but worth mentioning.


### Observability

<!-- What metrics, events, status conditions, or log lines help users understand
     the feature is working? Omit if not applicable. -->

All we need is two things.

First, a PriceTieBreakSkipped event, as described in Edge Cases, in the case of a mixed node pool having the same weight as non-mixed node pools.
This event notifies that the edge case was hit and the process fell back to reverse alphabet evaluation.
During consolidation, Karpenter runs simulations. 
This would trigger the event to be published. 
That can either be accepted, or this event removed alltogether.
The third option would be to route a flag to indicate if the run is a simulation or not, but that is undesireable complexity.

Second, a **debug** log line. Each time the Price Aware gate chooses the cheapest node pool, it logs it. 
The fields logged are Pod, namespace/name, and Node pool, including its calculated `price` (price + overlays)

### Edge Cases

<!-- Known corner cases and how the design handles them. A few worked examples
     are more valuable than exhaustive enumeration. -->

// TODO Look into the following: 
- A pod fits only one pool: it goes to that pool. A cheaper pool that can't fit the pod is ruled out by its price bound without being tried. (2.5 CPU pod: a-big-test m7g.xlarge at $0.1059 beat z-small-test at $0.0001.)
- Equal prices: Fallback to name order
- An overlay is deleted mid-scheduling: each scheduling run fixes its prices when it starts, so the change applies from the next run. NodeClaims already chosen keep their pool. In the test, a deletion 1 second after creating 4 pods meant all 4 went to spot in one consistent run, with no errors.


# A mixed pool where on-demand is cheaper than spot
This won't happen much, unless with Node Overlays enabled.
But in this case, it differs between providers, since AWS has the capacity type "reserved", and Azure does not, for example.
The gate overprices such a pool and can pick an equal-eight pool that is in fact more expensive.


Phase	| Pools	| Launched	| Real price
--	| --	| --    | --
1	| z-mixed-test and a-spot-test, equal weight	| a-spot-test, m7g.xlarge spot	| $0.001
2	| z-mixed-test alone	| z-mixed-test, m7g.xlarge on-demand	| $0.0001

Phase 1: a-spot-test won even though it loses the name tie-break, so the gate chose it on price. The gate rated the mixed pool at its cheapest spot offering (~$0.03, above $0.001), not the $0.0001 it actually launches at. The log shows created nodeclaim … NodePool: a-spot-test.
Phase 2: with no competitor, the same mixed pool launched on-demand. That confirms $0.0001 was its real price.

Therefore I recommend one capacity type per NodePool when using the gate. 
This is also specifically the setup my use-case needs.

To handle this, the easiest path is to fallback to the reverse alphabet and break out of the price evaluation.
An event is published, `PriceTieBreakSkipped`.

## Alternatives Considered

<!-- For each alternative: state what it is, how it works (briefly), and why it was
     rejected. The goal is to show the reader that obvious alternatives were evaluated,
     not to re-derive the solution. -->

### Alternative 1: Capped node pools

One can create an on-demand only nodepool, with a higher weight, and add a cap on how many vCPUs can be used there.
The Cap can then be adjusted to represent how many vCPUs the $/h commitment can buy you.
This however, does not account for varying prices, discounts or reservations. 
Neither does it scale well across multiple clusters and accounts either.
And, it is very manual.
To quote PeterIvanov: https://github.com/aws/karpenter-provider-aws/issues/2259#issuecomment-3398172325

> The only workaround I know at the moment is: to use weighted on-demand node group for the capacity you have reserved. But it requires micro-managing every node group in many clusters, which all share the same SP budget, and the exact settings, in principle, need to be changed after every small change in SPs or in capacity usage pattern. Seeems like a brittle contraption to me.

### Alternative 2: Mixed Node-pools

Mixed node pools are on a per-provider basis. [AWS has one version](https://github.com/aws/karpenter-provider-aws/blob/466225b902058933fa8a43e659dd775939a49464/pkg/providers/instance/filter/filter.go#L392-L409), [Azure has another](https://github.com/Azure/karpenter-provider-azure/blob/c9cbb3c481c762e7bfd2cad60c32965b50088e2a/pkg/providers/allocationstrategy/stages/offering_rank.go#L84-L110).
Mixed node pools on AWS choose nodes in the following order: [reserved, spot, on-demand](https://github.com/aws/karpenter-provider-aws/blob/466225b902058933fa8a43e659dd775939a49464/pkg/providers/instance/instance.go#L829).
Spot will always beat out on-demand. **Unless an on-demand is cheaper!** So this __could__ work for some people, utilizing Node Overlays with heavy downpricing.
But not for people who [set `minValues`](https://github.com/aws/karpenter-provider-aws/blob/466225b902058933fa8a43e659dd775939a49464/pkg/providers/instance/filter/filter.go#L407) on their Node Pools, a use case exemplified both [AWS's best practice documentation](https://github.com/aws/aws-emr-containers-best-practices/blame/05077e225b15f3e626d0351d43e088b382826f67/content/performance/docs/karpenter.md#L149) as well as [AWS's own provider documentation](https://github.com/aws/karpenter-provider-aws/blob/466225b902058933fa8a43e659dd775939a49464/website/content/en/docs/concepts/nodepools.md?plain=1#L305-L367)

On top of that, a mixed pool has one disruption budget, one consolidation policy, one expireAfter, one CPU limit.
That is not ideal when mixing on-demand and spot.
So this alternative is rejected on the basis that it doesn't quite cover all the bases.

### Alternative 3: Capacity Reservations sized to Savings Plans

One solution in AWS would be to maintain ODCRs directly in relation to Savings Plans.
In AWS Savings Plans are organization wide, and can include every single instance type.
ODCRs are fixed any count of a single instance type in a single AZ.
Maintaining these ODCRs between instances and all AZs is insurmountable, and brings us right back to the issue with maintaining the capped node pools manually, only worse.


## Backward Compatibility

<!-- What happens to existing users? Are there migration steps? Does existing YAML
     continue to work unchanged? -->

This feature should not change any API and YAMLS will continue to work unchanged.
There is no active migration on the part of the user.

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

# Who should determine a NodePool's launch price?

1. Core takes the cheapest offering across allowed capacity types. That is right for AWS without minValues and for Azure, and wrong for AWS with minValues.
2. A CloudProvider hook that returns the expected launch offering or price. It's exact for every provider, but it adds to the provider interface.
3. Keep today's order and document the limitation. It's simple, and correct for single-capacity-type pools.


# Can this ever be turned on by default? 
I am not sure if users will prefer this behaviour, and this might want to sit completely behind a feature gate.
But then maintaining two branches in the code is also extra work.

<!-- Unresolved decisions that need feedback or data before implementation.
     Number them so reviewers can reference them easily. -->
