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

## Recommendation: Simplified Repair Configuration in Disruption Settings

While a cloud provider can define a default set of repair policies and reasonable toleration durations, different workloads may have different sensitivities to node issues. A NodePool running critical, stateful applications might require a longer toleration duration to allow for manual intervention or graceful failover. Conversely, a NodePool with stateless, fault-tolerant applications can benefit from shorter tolerations for quicker recovery. Exposing per-condition toleration durations gives users the fine-grained control needed to tailor node repair behavior to their specific use cases.

To address this, we will introduce a new `repair` block in the `NodePool` API. This provides a dedicated space for repair configuration and gives users fine-grained control.

```go
type NodePoolSpec struct {
    // ... existing fields ...
    Disruption DisruptionSpec `json:"disruption"`

    // Repair configures the repair policies for nodes in this NodePool.
    // If not specified, repair policies will be disabled for this nodepool.
    Repair *RepairSpec `json:"repair,omitempty"`
}

type DisruptionSpec struct {
    // ... existing fields like consolidation, expireAfter ...
}

// RepairSpec defines the repair policies for nodes in this NodePool
type RepairSpec struct {
    // Policies defines a list of repair policies for specific node conditions. If a node has a condition
    // that matches a policy in this list, Karpenter will use the corresponding toleration duration.
    // This provides fine-grained control over repair timing for different issues.
    // These policies override the DefaultTolerationDuration.
	Policies []RepairPolicy `json:"policies,omitempty"`

    // DefaultTolerationDuration is the default duration to wait before repairing a node
    // for any condition that is not explicitly covered by a policy in the 'policies' list.
    // This acts as a fallback for any uncategorized or new node conditions.
    // If not specified, Karpenter will use the CloudProvider's default duration.
	DefaultTolerationDuration *metav1.Duration `json:"defaultTolerationDuration,omitempty"`
}

type RepairPolicy struct {
    // ConditionType is the type of the node condition, e.g. "Ready", "DiskPressure".
    ConditionType metav1.ConditionType `json:"conditionType"`
    // Toleration is the duration to wait before repairing a node for this specific condition.
    Toleration      metav1.Duration      `json:"toleration"`
}
```

type CloudProvider interface {
  ...
    // RepairPolicy is for CloudProviders to define a set of unhealthy conditions for Karpenter 
    // to monitor on the node.
    RepairPolicy() []RepairStatement
  ...
}

type RepairStatement struct {
    // Type of unhealthy state that is found on the node
    Type metav1.ConditionType 
    // Status condition of when a node is unhealthy
    Status metav1.ConditionStatus
}
```

The CloudProvider will continue to define which conditions to monitor. The user configuration in the NodePool will now be simpler, only requiring the `conditionType` to override a `tolerationDuration`.

The resolution order for `Toleration` is:
1. The `toleration` in a `RepairPolicy` within the NodePool's `repair.policies` for the specific condition type.
2. The NodePool's `repair.defaultTolerationDuration`.
3. CloudProvider's recommended default duration.

The controller workflow:
1. A diagnostic agent will add a status condition on a node.
2. Karpenter will reconcile nodes and match unhealthy conditions with repair statements from the CloudProvider.
3. Karpenter will determine the `Toleration` using the `NodePool`'s repair configuration.
4. The Node Health controller will forcefully terminate the NodeClaim once the node has been in an unhealthy state for the determined duration.

### Example

#### CloudProvider Implementation
The CloudProvider defines which conditions to monitor. This remains unchanged.
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
Users can configure repair policies in their NodePool under the `repair` block.
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
      toleration: 45m
    - conditionType: NetworkUnavailable
      toleration: 10m
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

## Alternative Designs Considered

### CloudProvider-Defined Repair Policies

The initial design considered having the cloud provider define the entire repair policy, including the toleration duration for each type of node condition. The `CloudProvider` interface would have returned a list of repair policies with specific durations.

```go
// Original proposed CloudProvider interface
type CloudProvider interface {
  ...
    // RepairPolicy would have returned policies with fixed durations
    RepairPolicy() []RepairPolicy
  ...
}

type RepairPolicy struct {
    Type metav1.ConditionType 
    Status metav1.ConditionStatus
    TolerationDuration metav1.Duration
}
```

This approach was simple and required minimal configuration from the user. However, feedback suggested that users need more control over how quickly a node is repaired, as the ideal remediation time can vary depending on the workloads running on a given NodePool. A fixed duration from the cloud provider would not offer the desired flexibility. This led to the updated recommendation where the `Toleration` is configurable within the `NodePool`.

## Forceful termination

For a first iteration approach, Karpenter will implement the force termination. Today, the graceful termination in Karpenter will attempt to wait for the pod to be fully drained on a node and all volume attachments to be deleted from a node. This raises the problem that during the graceful termination, the node can be stuck terminating when the pod eviction or volume detachment may be broken. In these cases, users will need to take manual action against the node. **For the Alpha implementation, the recommendation will be to forcefully terminate nodes. Furthermore, unhealthy nodes will not respect the customer configured terminationGracePeriod.**

## Future considerations 

There are additional features we will consider including after the initial iteration. These include:

* Disruption controls (budgets, terminationGracePeriod) for unhealthy nodes 
* Node Reboot (instead of replacement)
* Configuration surface for graceful vs forceful termination 
* Additional consideration for the availability zone resiliency
* Customer-defined node conditions for repair (beyond CloudProvider-defined conditions) 
