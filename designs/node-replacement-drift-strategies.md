# RFC: Allow disrupting nodes before creating replacements for static NodePools

## Overview

This design proposes an optional setting on static NodePools that allows for applying disruption actions without needing to spin up a replacement Node. Currently, Karpenter's drift resolution workflow requires a replacement Node to be spun up and available before the drifted node is terminated, which is not viable in certain static capacity scenarios. 

Fixes https://github.com/kubernetes-sigs/karpenter/issues/2905

## User stories

1. As a cluster operator, I want Karpenter to automate drift resolutions for limited/rare capacity types (ex. GPU instances, constrained ODCRs etc.) without manual intervention.

## Problem statement

Karpenter is used to manage automated drift resolution  safely by cluster operators when rolling out new node software (ex. new OS upgrades, Kubernetes versions etc.). Karpenter provides multiple safety controls such as Node disruption budgets and do-not-disrupt annotations on Nodes and pods. In addition, Karpenter also spins up replacement nodes before starting eviction and termination of the drifted node. However, in some scenrios, users have very static pools of capacity to choose from - two common cases are expensive instance types (ex. GPUs) and capacity reservations (ex. Amazon ODCRs) where it is not desirable to spin up additional high-cost or limited capacity before scaling down the drifted node. 

## Proposed design

The proposed design is a fairly simple extension over the current NodePool API and disruption controller. 

** API changes ** 

The API introduces a new field (driftResolutionPolicy) modeled in a similar manner to consolidationPolicy. This allows optional extensibility to other drift resolution policies such as in-place upgrades (example: https://github.com/aws/karpenter-provider-aws/issues/8735) 

```yaml
apiVersion: karpenter.sh/v1
kind: NodePool
metadata:
  name: expensive-gpu-pool
spec:
  disruption:
    # ... existing fields
    
    # New field
    replacementPolicy: CreateReplacement | DoNotCreateReplacement # Default: CreateReplacement
      
```

** Design considerations ** 

One consideration is whether there is value is opening up the DoNotCreateReplacement strategy to static NodePools only vs all NodePools. 

Option 1: Static NodePools only

Pros:
* Simplifies rollout and works for current identified use cases
* Keeps changes limited to the Static drift controller logic only which is quite simple.
* Avoids the complication of the standard NodePool drift controller which doesn't always need to spin up a replacement as existing nodes in the cluster may be able to accomodate pods already. 
 
Cons:
* There are likely use-cases that could benefit from this behaviour without using static NodePools, such as dynamic NodePools with shallow reserved pools. In these cases, it may be undesriable to go over the 'max' node count of the reserved pool solely for drift resolutions

Option 2: Implement for all NodePool types (Recommended)

Pros: 
* Works for all cases that prefer avoiding spinning up node replacements
* Based on a quick read of the code, this should be a relatively easy change for all types

Cons:
* Higher risk exposure of feature rollout, especially on standard NodePools. However, we should be able to mitigate with proper feature flagging

** Implementation notes ** 

* For static drift, this should be a relatively easy check [here](https://github.com/kubernetes-sigs/karpenter/blob/9136011b9a9c1840cd0e844e2eee57716e757f15/pkg/controllers/disruption/staticdrift.go#L94-L96) to not add replacement NodeClaims in the case of the DoNotCreateReplacement strategy
* For standard NodePool drift, the change is also likely relatively easy where we can skip the [scheduling simulation and replacement check](https://github.com/kubernetes-sigs/karpenter/blob/9136011b9a9c1840cd0e844e2eee57716e757f15/pkg/controllers/disruption/drift.go#L81) entirely if DoNotCreateReplacement strategy is used
