# Karpenter PreferNoSchedule Taint

## Overview

Karpenter currently lacks comprehensive taints lifecycle management that aligns well with the deprovision lifecycle. This misalignment often leads to issues for users, as they handle the deprovisioning process in various ways, resulting in unique problems based on their use cases and associated cluster dependencies.

## Motivation

Some key scenarios have prompted the need for this feature enhancement:

1. When Karpenter evicts all pods from a selected node for disruption, the node remains active during the disruption process. Consequently, some evicted pods may get rescheduled, leading to increased pod churn, which is undesired for critical pods.

2. Karpenter, upon encountering a node with a "do-not-evict" annotation, refrains from disrupting the node even if it has expired or drifted. While this is appropriate for critical workloads, it might be beneficial to allow non-critical workloads to be scheduled on such nodes to optimize resource utilization.

## Proposal

Implementing the use of the `PreferNoSchedule` taint within Karpenter can significantly address the aforementioned issues.

### Case 1

In instances similar to the first scenario, while a `NoSchedule` taint could suffice, there might be conditions during the subsequent disruption process that don't support this disruption, leaving the node untainted and thereby preventing the scheduling of non-critical pods. By introducing a `PreferNoSchedule` taint, users can choose to tolerate this taint for non-critical pods, ensuring efficient resource utilization.

### Case 2

For the second scenario, the `PreferNoSchedule` taint is an appropriate solution. This approach prevents the scheduling of critical workloads on nodes likely to terminate following user intentions, while allowing non-critical workloads to utilize the node effectively without compromising critical workload placement.

## Implementation Details

The optimal placement of this taint would be at the start of the disruption process, where a node is identified as a candidate for disruption. This taint should then be replaced with a `NoSchedule` taint once it is confirmed that the node will be disrupted.

## Severity of the Issue

The severity of this issue has become more apparent as our project continues to evolve and adapt. It is increasingly common, either as a standalone issue or as a contributing factor to other issues, particularly in cases where nodes are not properly cordoned.
