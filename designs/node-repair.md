# Node Auto Repair 

## Problem Statement

Nodes can experience failure modes that cause degradation to the underlying hardware, file systems, or container environments. Some of these failure modes are surfaced through the Node object such as network unavailability, disk pressure, or memory pressure, while others are not surfaced at all such as accelerator health. A Diagnostic Agent such as the [Node Problem Detector (NPD)](https://github.com/kubernetes/node-problem-detector) offers a way to surface these failures as additional status conditions on the node object.

When a status condition is surfaced through the Node, it indicates that the Node is unhealthy. Karpenter does not currently react to those conditions.

* Mega Issue: https://github.com/kubernetes-sigs/karpenter/issues/750
    * Related (Unreachable): https://github.com/aws/karpenter-provider-aws/issues/2570
    * Related (Remove by taints): https://github.com/aws/karpenter-provider-aws/issues/2544
    * Related (Known resource are not registered) Fixed by v0.28.0: https://github.com/aws/karpenter-provider-aws/issues/3794
    * Related (Stuck on NotReady): https://github.com/aws/karpenter-provider-aws/issues/2439

#### Out of scope

The alpha implementation will not consider these features:

  - Disruption Budgets
  - Customer-Defined Conditions
  - Customer-Defined Remediation Time

The team does not have enough data to determine the right level of configuration that users will utilize. The opinionated mechanism would be responsible for defining unhealthy notes. The advantage of creating the mechanism would be to reduce the configuration burden for customers. **The feature will be gated under an alpha NodeRepair=true feature flag. This will  allow for additional feedback from customers. Additional feedback can support features that were originally considered out of scope for the Alpha stage.**

## Current Implementation

The basic node repair functionality has been implemented to address unhealthy nodes. This document captures the historical context and evolution of the node repair feature.

For detailed configuration options and the current NodePool API proposal, see: [NodePool Repair Configuration RFC](./nodepool-repair-configuration.md)

## Forceful termination

For a first iteration approach, Karpenter will implement the force termination. Today, the graceful termination in Karpenter will attempt to wait for the pod to be fully drained on a node and all volume attachments to be deleted from a node. This raises the problem that during the graceful termination, the node can be stuck terminating when the pod eviction or volume detachment may be broken. In these cases, users will need to take manual action against the node. **For the Alpha implementation, the recommendation will be to forcefully terminate nodes. Furthermore, unhealthy nodes will not respect the customer configured terminationGracePeriod.**

## Future considerations 

There are additional features we will consider including after the initial iteration. These include:

* Disruption controls (budgets, terminationGracePeriod) for unhealthy nodes 
* Node Reboot (instead of replacement)
* Configuration surface for graceful vs forceful termination 
* Additional consideration for the availability zone resiliency
* Customer-defined node conditions for repair (beyond CloudProvider-defined conditions) 
