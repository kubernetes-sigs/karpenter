# RFC: Optionally allow disrupting nodes without creating replacements

## Overview

This design proposes an optional setting on NodePools that allows for applying disruption actions without needing to spin up a replacement Node. Currently, Karpenter's drift resolution workflow requires a replacement Node to be spun up and available before the drifted node is terminated, which is not viable in certain static capacity scenarios. 

Fixes https://github.com/kubernetes-sigs/karpenter/issues/2905

## User stories

1. As a cluster operator, I want Karpenter to automate drift resolutions for limited/rare capacity types (ex. GPU instances, constrained ODCRs etc.) without manual intervention.
2. As a cluster operator, I want the option of applying terminate-before-replace as a drift resolution strategy for both, static and dynamic NodePools to prevent surging capacity more than my budget.
3. As a Karpenter operator, I want design decisions on API changes to be extensible to future use cases of node replacement strategies.

## Problem statement

Karpenter is used to manage automated drift resolution  safely by cluster operators when rolling out new node software (ex. new OS upgrades, Kubernetes versions etc.). Karpenter provides multiple safety controls such as Node disruption budgets and do-not-disrupt annotations on Nodes and pods. In addition, Karpenter also spins up replacement nodes before starting eviction and termination of the drifted node. However, in some scenarios, users have very limited pools of capacity to choose from - two common cases are expensive instance types (ex. GPUs) and capacity reservations (ex. Amazon ODCRs) where it is not desirable to spin up additional high-cost / limited capacity before scaling down the drifted node. 

## Proposed design

The proposed design is a fairly simple extension over the current NodePool API and disruption controller. 

### API changes 

The API introduces a new field (driftResolutionPolicy) modeled in a similar manner to consolidationPolicy. This allows optional extensibility to other drift resolution policies (example in-place upgrades: https://github.com/aws/karpenter-provider-aws/issues/8735) 

```yaml
apiVersion: karpenter.sh/v1
kind: NodePool
metadata:
  name: expensive-gpu-pool
spec:
  disruption:
    # ... existing fields
    
    # New!
    # driftResolutionPolicy: Defines what action Karpenter takes to resolve the drifted node. Two options are provided (so far):
    # 1. CreateReplacement (default, current behaviour): Karpenter spins up a replacement node before terminating the drifted one. 
    # 2. Terminate: Karpenter terminates the node without spinning up a replacement. If evicted pods are unable to be placed, Karpenter's auto-scaling will instead be used to spin up a replacement node.
    driftResolutionPolicy: CreateReplacement | Terminate # Default: CreateReplacement
      
```

### Node disruption budget changes

Today, the logic used to determine the number of disruptable nodes is the same for all disruption strategies (standard drift, static drift, consolidation etc.). It works as follows. For each NodePool, determine:

1. Number of Nodes in the NodePool (N). *Exclude* nodes that are terminating or not fully initialized. For our design, assume that Terminating nodes for the NodePool are T and initializing (but not ready) nodes in the NodePool are I. 
2. Number of Disrupting Nodes (D), identified as Nodes that are 'NotReady' or the node is marked for deletion. 
3. The node disruption budget (NDB), which is a percentage of N that is allowed to be disrupted. As long as D < (NDB * N), disruptions can proceed. 

Today, this setup works because virtually all disruption actions (aside from Empty consolidations), will spin up a Replacement node and blocks termination of the node until its up - this in turn prevents runaway scale down. However, with the introduction of the new proposed action to Terminate without creating a replacement, we run the risk of runaway deletions. To prevent this, we propose the following tweak to the algorithm 

1. Include both terminating nodes and initializing nodes in the total count of Nodes in the NodePool (i.e N, now includes T and I as part of its totals)
2. Include the terminating and initializing nodes in the NodePool in the count of 'disrupting' nodes. While these nodes are not actively 'disrupting', they should be considered as contributing to the disruption budget, in the same way 'NotReady' nodes are. 

With this change, even with strategies where a replacement node is not spun up, the NodePool is aware of unhealthy nodes (either initializing or terminating) and will count them towards it overall disruption budget. This should work for all disruption strategies without significant changes to the current algorithm.

** Release recommendation **

We recommend releasing the updates to the disruption calculations behind a feature flag along with the API updates to avoid unexpected regressions
