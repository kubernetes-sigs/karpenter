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

The team does not have enough data to determine the right level of configuration that users will utilize. The opinionated mechanism would be responsible for defining unhealthy notes. The advantage of creating the mechanism would be to reduce the configuration burden for customers. **The feature will be gated under an alpha NodeRepair=true feature flag. This will  allow for additional feedback from customers. Additional feedback can support features that were originally considered out of scope for the Alpha stage.**

## Recommendation: CloudProvider-Defined RepairPolicies with NodePool-Configurable TolerationDuration

```
type RepairStatement struct {
    // Type of unhealthy state that is found on the node
    Type metav1.ConditionType 
    // Status condition of when a node is unhealthy
    Status metav1.ConditionStatus
}

type RepairPolicy struct {
    // ConditionType specifies the node condition to monitor
    ConditionType metav1.ConditionType
    // Status specifies the condition status that indicates unhealthy state
    Status metav1.ConditionStatus
    // TolerationDuration is the duration to wait before attempting to terminate 
    // nodes that match this repair policy. If not specified, defaults to the 
    // cloud provider's recommended duration for this condition type.
    TolerationDuration *metav1.Duration
}

type NodePoolSpec struct {
    // ... existing fields ...
    
    // Repair defines the repair policies for nodes in this NodePool
    Repair *RepairSpec
}

type RepairSpec struct {
    // Policies defines the repair policies for different node conditions.
    // These policies override the default tolerationDuration from the CloudProvider.
    Policies []RepairPolicy
    
    // DefaultTolerationDuration is the default duration to wait before repairing
    // nodes for any condition not explicitly configured in Policies.
    // If not specified, uses the CloudProvider's default durations.
    DefaultTolerationDuration *metav1.Duration
}

type CloudProvider interface {
  ...
    // RepairPolicy is for CloudProviders to define a set of unhealthy conditions for Karpenter 
    // to monitor on the node. The TolerationDuration can be overridden by NodePool configuration.
    RepairPolicy() []RepairStatement
  ...
}
```

The RepairPolicy will contain a set of statements that the Karpenter controller will use to watch node conditions. The CloudProvider defines which conditions to monitor, while the NodePool configuration specifies the TolerationDuration for each condition. On any given node, multiple node conditions may exist simultaneously. In those cases, we will choose the shortest `TolerationDuration` for a given condition. 

The resolution order for TolerationDuration is:
1. NodePool-specific RepairPolicy for the condition type
2. NodePool's DefaultTolerationDuration 
3. CloudProvider's recommended default duration

The controller workflow:
1. A diagnostic agent will add a status condition on a node 
2. Karpenter will reconcile nodes and match unhealthy conditions with repair policy statements from the CloudProvider
3. Karpenter will determine the TolerationDuration using the NodePool's repair configuration
4. Node Health controller will forcefully terminate the NodeClaim once the node has been in an unhealthy state for the determined duration

### Example

#### CloudProvider Implementation
The CloudProvider defines which conditions to monitor but no longer specifies durations:
```
func (c *CloudProvider) RepairPolicy() []cloudprovider.RepairStatement {
    return []cloudprovider.RepairStatement{
        {
            Type: "Ready",
            Status: corev1.ConditionFalse,
        },
        {
            Type: "NetworkUnavailable",
            Status: corev1.ConditionTrue,
        },
        ...
    }
}
```

#### NodePool Configuration
Users can configure repair policies in their NodePool:
```
apiVersion: karpenter.sh/v1beta1
kind: NodePool
metadata:
  name: default
spec:
  # ... other fields ...
  repair:
    defaultTolerationDuration: 30m
    policies:
    - conditionType: Ready
      status: "False" 
      tolerationDuration: 45m
    - conditionType: NetworkUnavailable
      status: "True"
      tolerationDuration: 10m
```

#### Node Repair Scenarios
In the example above, the NodePool configures different toleration durations for different conditions. Below are the cases of when we will act on nodes:

```
apiVersion: v1
kind: Node
metadata:
  ...
status:
  conditions:
    - lastHeartbeatTime: "2024-11-01T16:29:49Z"
      lastTransitionTime: "2024-11-01T15:02:48Z"
      message: no connection
      reason: Network is not available
      status: "True"
      type: NetworkUnavailable
    ...
    
- The Node here will be eligible for node repair at `2024-11-01T15:12:48Z` (10m after transition)
---
apiVersion: v1
kind: Node
metadata:
  ...
status:
  conditions:
    - lastHeartbeatTime: "2024-11-01T16:29:49Z"
      lastTransitionTime: "2024-11-01T15:02:48Z"
      message: kubelet is posting ready status  
      reason: KubeletReady
      status: "False"
      type: Ready
    ...
    
- The Node here will be eligible for node repair at `2024-11-01T15:47:48Z` (45m after transition)
```


## Forceful termination

For a first iteration approach, Karpenter will implement the force termination. Today, the graceful termination in Karpenter will attempt to wait for the pod to be fully drained on a node and all volume attachments to be deleted from a node. This raises the problem that during the graceful termination, the node can be stuck terminating when the pod eviction or volume detachment may be broken. In these cases, users will need to take manual action against the node. **For the Alpha implementation, the recommendation will be to forcefully terminate nodes. Furthermore, unhealthy nodes will not respect the customer configured terminationGracePeriod.**

## Future considerations 

There are additional features we will consider including after the initial iteration. These include:

* Disruption controls (budgets, terminationGracePeriod) for unhealthy nodes 
* Node Reboot (instead of replacement)
* Configuration surface for graceful vs forceful termination 
* Additional consideration for the availability zone resiliency
* Customer-defined node conditions for repair (beyond CloudProvider-defined conditions) 

