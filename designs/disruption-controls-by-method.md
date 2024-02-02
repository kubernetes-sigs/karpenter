# Disruption Controls By Reason 
## User Scenarios 
1. Users need the capability to schedule upgrades only during business hours or within more restricted time windows. Additionally, they require a system that doesn't compromise the cost savings from consolidation when upgrades are blocked due to drift.
2. High-Frequency Trading (HFT) firms require full compute capacity during specific operational hours, making it imperative to avoid scale-down requests for consolidation during these periods. However, outside these hours, scale-downs are acceptable.

## Known Requirements 
**Reason and Budget Definition:** Users should be able to define an action and a corresponding budget(s).
**Supported Reasons:** All disruption Reasons affected by the current Budgets implementation (Consolidation, Emptiness, Expiration, Drift) should be supported. We must support any cloudprovider.DriftReason in the budgets to allow control on node image upgrade vs other types of drift. 
**Default Behavior for Unspecified Reasons:** Budgets should continue to support a default behavior for all disruption actions. If an action is unspecified, it is assumed to apply to all actions. If a reason is unspecified, we apply the budget to be shared by all reasons that are unspecified. 

## API Design
### Approach A: Add a reason field to disruption Budgets 
This approach outlines a simple api change to the v1beta1 nodepool api to allow disruption budgets to specify a disruption action. 
### Proposed Spec
Add a simple field "reason" is proposed to be added to the budgets. 
```go
// Budget defines when Karpenter will restrict the
// number of Node Claims that can be terminating simultaneously.
type Budget struct {
      // Reason defines the disruption action that this particular disruption budget applies to. 
      Reasons []string `json:"action,omitempty" hash:"ignore"`
      // Nodes dictates the maximum number of NodeClaims owned by this NodePool
      // that can be terminating at once. This is calculated by counting nodes that
      // have a deletion timestamp set, or are actively being deleted by Karpenter.
      // This field is required when specifying a budget.
      // This cannot be of type intstr.IntOrString since kubebuilder doesn't support pattern
      // checking for int nodes for IntOrString nodes.
      // Ref: https://github.com/kubernetes-sigs/controller-tools/blob/55efe4be40394a288216dab63156b0a64fb82929/pkg/crd/markers/validation.go#L379-L388
      // +kubebuilder:validation:Pattern:="^((100|[0-9]{1,2})%|[0-9]+)$"
      // +kubebuilder:default:="10%"
      Nodes string `json:"nodes" hash:"ignore"`
      // Schedule specifies when a budget begins being active, following
      // the upstream cronjob syntax. If omitted, the budget is always active.
      // Timezones are not supported.
      // This field is required if Duration is set.
      // +kubebuilder:validation:Pattern:=`^(@(annually|yearly|monthly|weekly|daily|midnight|hourly))|((.+)\s(.+)\s(.+)\s(.+)\s(.+))$`
      // +optional
      Schedule *string `json:"schedule,omitempty" hash:"ignore"`
      // Duration determines how long a Budget is active since each Schedule hit.
      // Only minutes and hours are accepted, as cron does not work in seconds.
      // If omitted, the budget is always active.
      // This is required if Schedule is set.
      // This regex has an optional 0s at the end since the duration.String() always adds
      // a 0s at the end.
      // +kubebuilder:validation:Pattern=`^([0-9]+(m|h)+(0s)?)$`
      // +kubebuilder:validation:Type="string"
      // +optional
      Duration *metav1.Duration `json:"duration,omitempty" hash:"ignore"`
}


var (
  // These reasons that are inserted by the cloud provider and will be used to validate drift reasons the cloud provider wants to whitelist
  CloudProviderAllowedDriftReasons = set.New() 
)


```


##### Example
```yaml
apiVersion: karpenter.sh/v1beta1
kind: NodePool
metadata:
  name: default
spec: # This is not a complete NodePool Spec.
  disruption:
    budgets:
    - schedule: "* * * * *"
      action: [Drift]
      nodes: 10
    # For all other actions , only allow 5 nodes to be disrupted at a time
    - nodes: 5
      schedule: "* * * * *"

```

In the original proposed spec, karpenter allows the user to specify up to [50 budgets](https://github.com/kubernetes-sigs/karpenter/blob/main/pkg/apis/v1beta1/nodepool.go#L96)

If there are multiple active budgets, karpenter takes the most restrictive budget. This same principle will be applied to the disruption budgets in this approach. The only difference in behavior is that each window will either apply to a single action or all actions. 

### Pros + Cons 
* 👍 No nested definitions required 
* 👍👍 Extending existing budgets api. No Breaking API Changes, completely backwards compatible  
* 👎 With action being clearly tied to budgets, and other api logic being driven by disruption Reason, we lose the chance to generalize per Reason controls 
* 👎 Makes validation of a particular disruption 

### Approach B: Defining Per Reason Controls  
Ideally, we could move all generic controls that easily map into other actions into one set of action controls, this applies to budgets and other various disruption controls that could be more generic. 
### Proposed Spec 
```go
type Disruption struct {
    All		  DisruptionSpec `json:defaults"`
    Consolidation ConsolidationSpec `json:"consolidation"`
    Drift         DriftSpec         `json:"drift"`
    Expiration    ExpirationSpec    `json:"expiration"`
    Emptiness     EmptinessSpec     `json:"emptiness"`
}

type DisruptionCommonSpec struct {
    DisruptAfter string   `json:"disruptAfter"`
    Budgets      []Budget `json:"budgets"`
}

type ConsolidationSpec struct {
    DisruptionCommonSpec
    ConsolidationPolicy string `json:"consolidationPolicy"`
}

type DriftSpec struct {
    DisruptionCommonSpec
}

type ExpirationSpec struct {
    DisruptionCommonSpec
}

type EmptinessSpec struct {
    DisruptionCommonSpec
}

type Budget struct {
    Nodes    string  `json:"nodes"`
    Schedule *string `json:"schedule,omitempty"`
    Duration *string `json:"duration,omitempty"`
    Reasons []string 
}
}

type DisruptionReason string 

const (
	All DisruptionReason = ""
	Consolidation DisruptionReason = "Consolidation" 
	Drift DisruptionReason = "Drift" 
	Expiration DisruptionReason = "Expiration" 
	Emptiness DisruptionReason = "Emptiness"
)
```
#### Example 

```yaml 
apiVersion: karpenter.sh/v1beta1
kind: NodePool
metadata:
  name: example-nodepool
spec:
  disruption:
    defaults:
      budgets: 
        - nodes: 10% 
          schedule: "0 0 1 * *"
          duration: 1h 
    consolidation:
      consolidationPolicy: WhenUnderutilized
      disruptAfter: "30m"
    drift:
      budgets:
        - nodes: "20%"
          schedule: "0 0 1 * *"
          duration: "1h"
        - nodes: "10%"
          reasons: ["NodeImageDrift"]
          schedule: "0 0 * * 0"
          duration: "2h"
        - nodes: "50%" 
          reasons: ["K8sVersionUpgrade", "NodeImageDrift"]
          schedule: "@yearly"
    expiration:
      disruptAfter: "Never"
```

#### Reasons
Rather than Reasons being defined at the budget level, we could add an additonal layer of abstraction. Then for each budget, we apply a reason or All/Undefined. If reason isn't specifed in a budget we take the same behavior in terms of fallback for all on these Reason types.

This design allows for simplification of reason as its very easy to directly define a relationship between a given disruption Reason and its sub-action since the disruption Reason is explicitly declared. Reasons aren't going to be solved by this doc, but this goes to show how this api design leaves a clear place for customer behavior per action.

#### Considerations 
Some of the API choices for a given action seem to follow a similar pattern. These include ConsolidateAfter, ExpireAfter, and there are discussions about introducing a global DisruptAfter. Moreover, when discussing disruption budgets, we talk about adding behavior for each action. It appears there is a need for disruption controls within the budgets for each action, not just overall.

This approach aligns well with controls that apply to all existing actions. The proposal presented here is similar to the one mentioned above in relation to the actions we allow to be defined (All, Consolidation, Drift, Expiration, Emptiness).

This proposal is currently scoped for disruptionBudgets by action. However, we should also consider incorporating other generic disruption controls into the PerReasonControls, even if we do not implement them immediately. Moving ConsolidateAfter and ExpireAfter into the per-action controls is a significant migration that requires careful planning and its own dedicated design. This proposal simply demonstrates a potential model that highlights the benefits of defining controls at a per-action level of granularity.

### Pros + Cons 
* 👍 This model starts to make more sense as we continue to add general behaviors that apply to all disruption actions where users will want control on the action level.
* 👍 Provides place per action for generic controls intended to be shared across all disruption Reasons. While not all Reasons share the same disruption actions, there are already cases for this. 
* 👍 Could extend other fields beyond generic values for specific actions with validation  
* 👍 Allows for very natural reason specification for further granularity of specifying specific K8sVersion upgrades for example. There is no need for complicated validation in regards to what Reason types are compatible with a particular Reason. This design easily evolves into per action controls across the board. 
*  👎👎 Breaking API Change for Budgets at least, and if we decide to model DisruptAfter we also would have to break those apis. It might make sense to break the budgets now before they have garnered large adoption as it becomes harder to make the change as time goes on.
* 👎 Doesn't allow for easy defaulting of `All` disruption actions
* 👎 Adds complexity to the use of budgets, before its high level on the disruption controls but with this design approach its nested inside another field.

### API Design Conclusion: Preferred Design
If the goal is to provide a simple, backward-compatible solution with immediate applicability, Approach A is more suitable. It provides a straightforward way to manage disruptions without overhauling the existing system. Breaking API changes in Approach B are likely too disruptive to customers. 

## Clarifying Behavioral Questions 
### Q: How should Karpenter handle the default or undefined action case? 
The current design involves specifying a specific number of disruptable nodes per action, which can complicate the disruption lifecycle. For example, if there's a 10-node budget for "Drift" and a separate 10-node budget for "Consolidation", but a 15-node budget for "All" determining which nodes will get disrupted becomes unclear. Would it be 10 nodes for "Drift" and 5 nodes for "Consolidation"?

We could consider treating an undefined action as a budget for all disruption actions except for those with explicitly defined budgets. In this scenario, if a user specifies a disruption budget like this:

```yaml
spec: # This is not a complete NodePool Spec.
  disruption:
    budgets:
    - schedule: "* * * * *"
      action: Consolidation
      nodes: 10
    - schedule: "* * * * *"
      action: Drift
      nodes: 10
    # For all other actions , only allow 5 nodes to be disrupted at a time
    - nodes: 5
      schedule: "* * * * *"
```

It means that "Consolidation" and "Drift" actions have specific budgets of 10 nodes each, while all other actions (e.g., expiration and emptiness) share a common budget of 5 nodes. This approach simplifies the configuration but has one limitation: it may not allow the execution of other disruption actions if a specific action exhausts the budget. This is a problem with the existing design for disruption budgets.

There are two ways for the users to get around this behavior. 
1. If you need gaurenteed disruption for a particular action, you can just specify that action in a budget.  
2. We could allow some mechanism for the users to control the ordering of the disruption actions.


#### Q: Should users be able to change the order that disruption actions are executed in to solve this problem? 
The answer is no, this makes it harder for cluster operators to understand behavior. It also doesn't elegantly fit into karpenters per nodepool controls. Defining it in the nodepool would mean you have multiple nodepools with different orderings, which is diffcult. Karpenter today does not provide an easy way via the CRDS to define per cluster level controls.  
#### Q: Should Budget "Reasons" allow for specifying more than one? Via an array? 

```go
// Budget defines when Karpenter will restrict the
// number of Node Claims that can be terminating simultaneously.
type Budget struct {
      // Reason defines the disruption action that this particular disruption budget applies to. 
      Reason DisruptionReason `json:"action,omitempty" hash:"ignore"`
      Nodes string `json:"nodes" hash:"ignore"`
      Schedule *string `json:"schedule,omitempty" hash:"ignore"`
      Duration *metav1.Duration `json:"duration,omitempty" hash:"ignore"`
}
```

vs
```go
// Budget defines when Karpenter will restrict the
// number of Node Claims that can be terminating simultaneously.
type Budget struct {
      // Reason defines the disruption action that this particular disruption budget applies to. 
      Reason []DisruptionReason `json:"action,omitempty" hash:"ignore"`
      Nodes string `json:"nodes" hash:"ignore"`
      Schedule *string `json:"schedule,omitempty" hash:"ignore"`
      Duration *metav1.Duration `json:"duration,omitempty" hash:"ignore"`
}
```


```yaml
yaml: 
  - nodes: 10
  reasons: [Drift, Consolidation, K8sVersionUpgrade]
  schedule: "* * * * *"
  - nodes: 5 
  reasons: [K8sVersionUpgrade] 
  schedule: "* * * * *" 
```
  


#### Q: How should karpenter track node deletion by reason
To answer this question we can first answer, how does the disruption budgets implementation track node deletion today? 

```go
func BuildDisruptionBudgets(ctx context.Context, cluster *state.Cluster, clk clock.Clock, kubeClient client.Client) (map[string]int, error) {
	nodePoolList := &v1beta1.NodePoolList{}
	if err := kubeClient.List(ctx, nodePoolList); err != nil {
		return nil, fmt.Errorf("listing node pools, %w", err)
	}
	numNodes := map[string]int{}
	deleting := map[string]int{}
	disruptionBudgetMapping := map[string]int{}
	// We need to get all the nodes in the cluster
	// Get each current active number of nodes per nodePool
	// Get the max disruptions for each nodePool
	// Get the number of deleting nodes for each of those nodePools
	// Find the difference to know how much left we can disrupt
	nodes := cluster.Nodes()
	for _, node := range nodes {
		// We only consider nodes that we own and are initialized towards the total.
		// If a node is launched/registered, but not initialized, pods aren't scheduled
		// to the node, and these are treated as unhealthy until they're cleaned up.
		// This prevents odd roundup cases with percentages where replacement nodes that
		// aren't initialized could be counted towards the total, resulting in more disruptions
		// to active nodes than desired, where Karpenter should wait for these nodes to be
		// healthy before continuing.
		if !node.Managed() || !node.Initialized() {
			continue
		}
		nodePool := node.Labels()[v1beta1.NodePoolLabelKey]
		if node.MarkedForDeletion() {
			deleting[nodePool]++
		}
		numNodes[nodePool]++
	}

	for i := range nodePoolList.Items {
		nodePool := nodePoolList.Items[i]
		disruptions := nodePool.MustGetAllowedDisruptions(ctx, clk, numNodes[nodePool.Name])
		// Subtract the allowed number of disruptions from the number of already deleting nodes.
		// Floor the value since the number of deleting nodes can exceed the number of allowed disruptions.
		// Allowing this value to be negative breaks assumptions in the code used to calculate how
		// many nodes can be disrupted.
		disruptionBudgetMapping[nodePool.Name] = lo.Clamp(disruptions-deleting[nodePool.Name], 0, math.MaxInt32)
	}
	return disruptionBudgetMapping, nil
}
```
The disruption budgets used by karpenter today will check the minimum allowed disruptions specified in all the budgets, minus the number of nodes currently undergoing disruption.

This implementation will have to change, and karpenter cluster state will have to become aware of the reason for node deletion.

```go
func (in *StateNode) MarkedForDeletion() bool {
	// The Node is marked for deletion if:
	//  1. The Node has MarkedForDeletion set
	//  2. The Node has a NodeClaim counterpart and is actively deleting
	//  3. The Node has no NodeClaim counterpart and is actively deleting
	return in.markedForDeletion ||
		(in.NodeClaim != nil && !in.NodeClaim.DeletionTimestamp.IsZero()) ||
		(in.Node != nil && in.NodeClaim == nil && !in.Node.DeletionTimestamp.IsZero())
}
```
Rather than this function that simply looks for a nodeclaims/nodes deletion timestamp, we will need to include some marking on the nodes indicating why they were deleted.
We can use the `DisruptionReason` to determine why a given nodeclaim was disrupted, then track in cluster state the current number of nodeclaims that are in a deleting state.

```go
type NodeClaimStatus struct {
	...
	// DisruptionReason represents the reason why the node was disrupted. This can be Consolidation, Drift, Expiration, Emptiness, and the cloudprovider drift reasons
	// in the format of "Reason:reason".
	DisruptionReason string `json:"disruptionDetails,omitempty"`
	...
}
```


## Observability and Supportability 
One major aspect to budgets that is missing is a proper monitoring story. The monitoring story can be broken into the following categories 
1. Metrics 
2. NodePoolStatus 
3. NodeClaimConditions
4. Events 

### Metrics
- **Metric**: `karpenter_nodepool_active_budgets`
- **Description**: Tracks the number of active budgets for a given NodePool.
- **Labels**:
  - `nodepool`: Identifies the NodePool.
  - `Reason`: Specifies the disruption Reason (e.g., Consolidation, Drift, Emptiness, Expiration).
- **Type**: Gauge
- **Value**: Number of active budgets for each combination of Reason and reason in the NodePool.
- EstimatedCardinality: X

- **Metric**: `karpenter_nodepool_disrupted_nodes`
- **Description**: Counts the number of nodes that have been disrupted for a particular action within a specific budget window.
- **Labels**:
  - `nodepool`: Identifies the NodePool.
  - `Reason`: Specifies the disruption Reason.
- **Type**: Counter
- **Value**: Cumulative count of disrupted nodes for each combination of Reason and reason in the NodePool.
- EstimatedCardinality: X

#### NodePool Status 
```yaml
apiVersion: karpenter.sh/v1beta1
kind: NodePool
metadata:
  annotations:
    karpenter.sh/nodepool-hash: "18334584923024176540"
    kubectl.kubernetes.io/last-applied-configuration: |
  creationTimestamp: "2024-01-25T06:59:08Z"
  generation: 2
  name: default
  resourceVersion: "721558"
  uid: f09b662b-eb65-4495-81ae-e407a8a6da5a
spec:
  disruption:
    budgets:
    - nodes: 10%
      Reason: "Drift" 

    consolidationPolicy: WhenUnderutilized
    expireAfter: 720h
  limits:
    cpu: 1000

status:
  disruption:
    activeBudgets:
      - Reason: "Drift"
        nodes: "5" # Current number of nodes disrupted under this budget
        remainingBudget: "45"
    totalDisruptedNodes: "20" # Total number of nodes disrupted across all Reasons
    lastDisruptionTime: "2024-01-25T10:00:00Z" # Timestamp of the last disruption
  resources:
    nodes: 500 # Useful when trying to understand percentage values in the nodes declaration of a budget
```

#### NodeClaimConditions
We want to communicate when 
```yaml 
kind: NodeClaim
status:
  conditions:
    - type: "DisruptionBudgetRestricted"
      status: "True|False"
      reason: "BudgetExceeded"
      message: "Disruption restricted due to budget limitations."
      lastTransitionTime: "2024-01-25T10:00:00Z"
```


#### Events 

- **Event**: `EnteringDisruptionWindow`
- **Description**: Triggered when a NodePool enters a disruption window as defined by the active budgets.

- **Event**: `ExitingDisruptionWindow`
- **Description**: Occurs when a NodePool exits a disruption window.

- **Event**: `BudgetExceeded`
- **Description**: Fired when the disruption actions exceed the specified budget for a NodePool
