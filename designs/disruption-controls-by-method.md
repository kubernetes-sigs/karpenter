# Disruption Controls By Method 
## User Scenarios 
1. Users need the capability to schedule upgrades only during business hours or within more restricted time windows. Additionally, they require a system that doesn't compromise the cost savings from consolidation when upgrades are blocked due to drift.
2. High-Frequency Trading (HFT) firms require full compute capacity during specific operational hours, making it imperative to avoid scale-down requests for consolidation during these periods. However, outside these hours, scale-downs are acceptable.

## Known Requirements and Desired Behaviors 
**Method and Budget Definition:** Users should be able to define an action and a corresponding budget(s).
**Supported Methods:** All disruption actions affected by the current Budgets implementation (Consolidation, Emptiness, Expiration, Drift) should be supported.
**Default Behavior for Unspecified Methods:** Budgets should continue to support a default behavior for all disruption actions. If an action is unspecified, it is assumed to apply to all actions.

### Q: How should Karpenter handle the default or undefined action case? 
The current design involves specifying a specific number of disruptable nodes per action, which can complicate the disruption lifecycle. For example, if there's a 10-node budget for "Drift" and a separate 10-node budget for "Consolidation," but a 15-node budget for "All" determining which nodes will get disrupted becomes unclear. Would it be 10 nodes for "Drift" and 5 nodes for "Consolidation"?

#### Unspecified Method as a Global Node Budget

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

### Q: Should Karpenter allow for more granular disruption reasons to have budgets? like for specific drift reasons for example? 
In the current setup, Karpenter provides a disruption controller with standard Method implementations. However, there's a need for more granularity in defining disruption budgets. For example, users may want to have a different schedule for Kubernetes version upgrades compared to node image upgrades.

Karpenter should provide a way to extend these more granular reasons that are children of the disruption methods. It does add a signficant number of questions into the mix. But almost deserves its own design as it opens up a multitude of questions.

## API Design
### Approach A: Add an action field to disruption Budgets 
This approach outlines a simple api change to the betav1 nodepool api to allow disruption budgets to specify a disruption action. 
### Proposed Spec
Add a simple field "action" is proposed to be added to the budgets. 
```go
// Budget defines when Karpenter will restrict the
// number of Node Claims that can be terminating simultaneously.
type Budget struct {
	// Method defines the disruption action that this particular disruption budget applies to. 
	Method DisruptionMethod `json:"action,omitempty" hash:"ignore"`
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

type DisruptionMethod string 

const (
	All DisruptionMethod = ""
	Consolidation DisruptionMethod = "Consolidation" 
	Drift DisruptionMethod = "Drift" 
	Expiration DisruptionMethod = "Expiration" 
	Emptiness DisruptionMethod = "Emptiness"
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
    # On Weekdays during business hours, do not drift nodes 
    - schedule: "0 0 1 * *"
      action: Drift
      nodes: 15
    # Every other time for all actions that are not Drift, only allow 10 nodes to be deprovisioned simultaneously
    - nodes: 10
```

In the original proposed spec, karpenter allows the user to specify up to [50 budgets](https://github.com/kubernetes-sigs/karpenter/blob/main/pkg/apis/v1beta1/nodepool.go#L96)

If there are multiple active budgets, karpenter takes the most restrictive budget. This same principle will be applied to the disruption budgets in this approach. The only difference in behavior is that each window will either apply to a single action or all actions. 

### Pros + Cons 
* 👍 Simple and easy to implement 
* 👍 No nested definitions required 
* 👍👍 Extending existing budgets api. No Breaking API Changes, completely backwards compatible  
* 👎 Doesn't leave room for other controls based on action. Its tightly coupled with budgets, read more about this in second approach.

### Approach B: Defining Per Method Controls  
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
}
}

type DisruptionMethod string 

const (
	All DisruptionMethod = ""
	Consolidation DisruptionMethod = "Consolidation" 
	Drift DisruptionMethod = "Drift" 
	Expiration DisruptionMethod = "Expiration" 
	Emptiness DisruptionMethod = "Emptiness"
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
      budgets:
	- nodes: "20%"
	  schedule: "0 0 1 * *"
	  duration: "1h"
    drift:
      disruptAfter: "1h"
      budgets:
	- nodes: "10%"
	  schedule: "0 0 * * 0"
	  duration: "2h"
    expiration:
      disruptAfter: "Never"
```

#### Considerations 
Some of the API choices for a given action seem to follow a similar pattern. These include ConsolidateAfter, ExpireAfter, and there are discussions about introducing a global DisruptAfter. Moreover, when discussing disruption budgets, we talk about adding behavior for each action. It appears there is a need for disruption controls within the budgets for each action, not just overall.

This approach aligns well with controls that apply to all existing actions. The proposal presented here is similar to the one mentioned above in relation to the actions we allow to be defined (All, Consolidation, Drift, Expiration, Emptiness).

This proposal is currently scoped for disruptionBudgets by action. However, we should also consider incorporating other generic disruption controls into the PerMethodControls, even if we do not implement them immediately. Moving ConsolidateAfter and ExpireAfter into the per-action controls is a significant migration that requires careful planning and its own dedicated design. This proposal simply demonstrates a potential model that highlights the benefits of defining controls at a per-action level of granularity.

### Pros + Cons 
* 👍 This model starts to make more sense as we continue to add general behaviors that apply to all disruption actions where users will want control on the action level.
* 👍 Provides place per action for generic controls intended to be shared across all disruption methods. While not all methods share the same disruption actions, there are already cases for this. 
* 👍 Could extend other fields beyond generic values for specific actions with validation   
*  👎👎 Breaking API Change for Budgets at least, and if we decide to model DisruptAfter we also would have to break those apis. It might make sense to break the budgets now before they have garnered large adoption as it becomes harder to make the change as time goes on.
* 👎 Doesn't allow for easy defaulting of `All` disruption actions
* 👎 Adds complexity to the use of budgets, before its high level on the disruption controls but with this design approach its nested inside another field.

### Conclusion: Preferred Design
If the goal is to provide a simple, backward-compatible solution with immediate applicability, Approach A is more suitable. It provides a straightforward way to manage disruptions without overhauling the existing system. Breaking API changes in Approach B are likely too disruptive to customers. 

