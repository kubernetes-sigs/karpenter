#  Karpenter Node Auto Repair 

## Problem Statement

Karpenter users are observing that occasionally nodes can face failure mode that prevent them to be healthy in a Kubernetes clusters. These nodes stay in unhealthy state for a period of time without any actions, and in some cases forever with manual intervention. Karpenter continues to simulate pods to these nodes, but the Kubernetes scheduler won't schedule to them. This will leave pods unscheduled and cause downtime for applications. Customers have the need for karpenter to repair unhealthy nodes. The karpenter team has seen several failure mode such as disk pressure, memory pressure, networking outage etc. that can cause nodes to get into an unhealthy state Here are issues related to the topic:

* Mega Issue: https://github.com/kubernetes-sigs/karpenter/issues/750
    * Related (Unreachable): https://github.com/aws/karpenter-provider-aws/issues/2570
    * Related (Remove by taints): https://github.com/aws/karpenter-provider-aws/issues/2544
    * Related (Known resource are not registered) Fixed by v0.28.0: https://github.com/aws/karpenter-provider-aws/issues/3794
    * Related (Stuck on NotReady): https://github.com/aws/karpenter-provider-aws/issues/2439

#### Out of scope

The scope of the solution suggested bellow will be an opinionated method that will not give customer configurability. The team does not have enough data to determine the right level of configuration surface that users will utilize. The implementation will not consider budgets, API surface for repairing unhealthy nodes, or option for nodes to be rebooted instead of replaced. The solution will also not give any Karpenter based approach to enable AZ resiliency. **The feature will be gated under an alpha feature flag to allow for additional feedback from customers, and make subsequent changes in the future.**

### Option  1 (recommended) : Unhealthy condition set cloud provider interface  

```
type UnhealthyConditions struct {
    // Type of unhealthy state that is found on the node
    Type string 
    // Status condition of when a node is unhealthy
    status string
    // RepairAfter is the duration the controller will wait
    // before attempting to terminate nodes that are underutilized.
    RepairAfter time.Duration
}

type CloudProvider interface {
  ...
    // UnhealthyConditions is for CloudProviders to define a set Unhealthy condition for Karpenter 
    // to monitor on the node. Customer will need 
    UnhealthyConditions() []v1.UnhealthyConditions
  ...
}
```

The `UnhealthyConditions` will be a set of condition that the Karpenter controller will watch. The cloud provider can define compatibility with any node diagnostic agent, and track a list of node unhealthy condition types and a duration period to wait until a unhealthy state is considered a terminal: 

1. A diagnostic agent will add a status condition on to a node 
2. Karpenter will reconcile on nodes and match unhealthy node condition to cloud provider defined unhealthy condition 
3. Unhealthy condition are added to the NodeClaim
4. Disruption controller will forcefully terminate the the NodeClaim once the node has been in an unhealthy state for the duration specified by `repairAfter` of the unhealthy condition 

### Option  2: cloud provider interface  for determining node health

```
type UnhealthyReason string

type CloudProvider interface {
  ...
    // IsHealthy returns whether a NodeClaim is considered operating as expected
    IsHealthy(context.Context, *v1.NodeClaim) (UnhealthyReason, error)
  ...
}
```

`IsHealthy` cloud provide methods allows each implementation of Karpenter to determine the healthy state of a NodeClaim and return a specified `UnhealthyReason` indicating that an action on the node is required. IsHealthy is a Turing complete operation and allows the cloud providers to determine when and on what condition to act on cleaning up the node. Cloud providers will also need to consider short lived failure prior to determining if a node is unhealthy. The `IsHealthy` follows the format defined by `IsDrifted`. The reconciliation step for replacing an unhealthy node will look as such: 

1. Karpenter will reconcile on the NodeClaim and make a cloud provide call with IsHealthy
2. If the node is determined to unhealthy, a status condition will be added to the NodeClaim
3. Disruption controller will force terminate the node claim immediately 

### Option  3:  Cloud provider injected unhealthy condition 

```
type UnhealthyType string
type UnhealthyRepairAfter time.Duration

// UnhealthyConditions are condition to watch for on nodes inside of the cluster
var UnhealthyConditions = map[UnhealthyType]UnhealthyRepairAfter{
    "Ready": "30m"
    "DiskPressure": "10m"
    ...
}
```

Cloud provider hydrate a `UnhealthyConditions` map that will hold the condition to track on nodes. The map will track the condition and `repairAfter` will dictate the length of time the node will be inside the cluster before action is taken against it. Node health validation will follow as such:

1. A diagnostic agent will add a status condition on to a node 
2. Karpenter will reconcile on nodes and identify unhealthy nodes defined by `UnhealthyConditions` map
3. Unhealthy condition is added to the NodeClaim
4. Disruption controller will forcefully terminate the the NodeClaim once the node has been in an unhealthy state for the duration specified by `UnhealthyConditions` map

## Recommended Solution

The recommended approach is for cloud provider interface should expose a layer of configuration for Karpenter to monitor nodes and take action. I propose we implement option one. Cloud provider can define any set of status conditions that are tracked on the node. An interface approach allows for easy extensibility. Option 2 has the approach of being over generalized as we don’t have use-cases to show the need. All solutions outlined below will be under a feature flag to indicate that these feature are not stable. 

### Forceful termination

For a first iteration approach Karpenter will implement force termination. Today, graceful termination in Karpenter will attempt to wait for pod to be fully drained on a node and all volume attachment to be deleted from a node. This raises the problem that during graceful termination node can be stuck terminating as pod eviction or volume detachment may be broken. In these cases, users will need to take manual action against the node or if set, termination grace period will force terminate the node. For the first iteration of problem, the recommendation will be to force terminate nodes now and follow-up to allow configurability for either the cloud providers or the Karpenter users once we have sufficient data to support the need for graceful termination for unhealthy nodes. 