# Disruption Controls By Action 

## Motivation
Users want the ability to have different disruption controls for the budgets. The AKS Provider Drives NodeImageUpgrade, and K8sVersionUpgrade through Drift. Users may want to specify different disruption settings for upgrade, since upgrade is a fundementally different type of disruption in comparison to consolidation for example. Since upgrade changes the behavior of the cluster, its disruption schedule may be different from consolidation.

## Desired Behavior + Requirements 
1. User needs to be able to define an action and a budget/budgets for that action. 
2. Actions that should be supported should by any disruption actions effected by the current Budgets implementation. (Consolidation, Emptiness, Expiration, Drift) 
3. Budgets should still support the option to set a given behavior for all disruption actions. A disruption action `All` will represent behavior will be defined as a default for all actions. It should respect this budget for actions without an active disruption budget. 
4. One should be able to specify for a given disruption action a NodeDisruptionBudget. If multiple budgets are active at a given time, karpenter will take action with the most restrictive budget
## API Design
### Approach A: Add an action field to disruption Budgets 
This approach outlines a simple api change to the betav1 nodepool api. To allow disruption budgets to specify a disruption action. Thats it. 
### Proposed Spec
Add a simple field "action" is proposed to be added to the budgets. 
```go
// Budget defines when Karpenter will restrict the
// number of Node Claims that can be terminating simultaneously.
type Budget struct {
	// Action defines the disruption action that this particular disruption budget applies to. 
	Action DisruptionAction `json:"action" hash:"ignore"`
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

type DisruptionAction string 

const (
	All DisruptionAction = "All"
	Consolidation DisruptionAction = "Consolidation" 
	Drift DisruptionAction = "Drift" 
	Expiration DisruptionAction = "Expiration" 
	Emptiness DisruptionAction = "Emptiness" ? 
)
```
If no value is specified we will assume this disruption budget is `All` and default to that value and the settings will apply to all disruption actions.

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
    # Every other time for all other disruption actions, only allow 10 nodes to be deprovisioned simultaneously
    - nodes: 10
```

In the original proposed spec, karpenter allows the user to specify up to [50 budgets](https://github.com/kubernetes-sigs/karpenter/blob/main/pkg/apis/v1beta1/nodepool.go#L96)

If there are multiple active budgets, karpenter takes the most restrictive budget. This same principle will be applied to the disruption budgets in this approach. The only difference in behavior is that each window will either apply to a single action or all actions. 

### Pros + Cons 
* 👍 Simple and easy to implement 
* 👍 No nested definitions required 
* 👍👍 Extending existing budgets api. No Breaking API Changes, completely backwards compatible  
* 👎 Doesn't leave room for other controls based on action. Its tightly coupled with budgets, read more about this in second approach.

### Approach B: Defining Per Action Controls  
Ideally, we could move all generic controls that easily map into other actions into one set of action controls, this applies to budgets and other various disruption controls that could be more generic 
### Proposed Spec 
```go
type Disruption struct {
	// ConsolidationPolicy describes which nodes Karpenter can disrupt through its consolidation
	// algorithm. This policy defaults to "WhenUnderutilized" if not specified
	// +kubebuilder:default:="WhenUnderutilized"
	// +kubebuilder:validation:Enum:={WhenEmpty,WhenUnderutilized}
	// +optional
	ConsolidationPolicy ConsolidationPolicy `json:"consolidationPolicy,omitempty"`
	
	// GenericPerActionControls defines the controls for a particular DisruptionAction, these controls are meant to apply for generic controls that apply to all disruption actions.
	GenericPerActionControls map[DisruptionAction]ActionControls `json:"actionControls,omitempty"` 
}


// ActionControls defines the controls for a particular DisruptionAction, these controls are meant to apply for generic controls that apply to all disruption actions. 
type ActionControls struct {
	// DisruptAfter is the duration the controller will wait before taking disruption action on this particular DisruptionAction 
	// +kubebuilder:validation:Pattern=`^(([0-9]+(s|m|h))+)|(Never)$` 
	// +kubebuilder:validation:Type="string" 
	// +kubebuilder:validation:Schemaless 
	// +optional 
	DisruptAfter *NillableDuration `json:"disruptAfter,omitempty"`
	// Budgets is a list of Budgets. 
	// If there are multiple active budgets, Karpenter uses 
	// the most restrictive value. If left undefined, 
	// this will default to one budget with a value to 10%. 
	// +kubebuilder:validation:XValidation:message="'schedule' must be set with 'duration'",rule="!self.all(x, (has(x.schedule) && !has(x.duration)) || (!has(x.schedule) && has(x.duration)))" 
	// +kubebuilder:default:={{nodes: "10%"}} 
	// +kubebuilder:validation:MaxItems=50 
	// +optional 
	Budgets []Budget `json:"budgets,omitempty" hash:"ignore"` 
}


 // Budget defines when Karpenter will restrict the
// number of Node Claims that can be terminating simultaneously.
type Budget struct {
	// Nodes dictates the maximum number of NodeClaims owned by this NodePool
	// that can be terminating at once. This is calculated by counting nodes that
	// have a deletion timestamp set, or are actively being deleted by Karpenter.
	Nodes string `json:"nodes" hash:"ignore"`
	// Schedule specifies when a budget begins being active, following
	// the upstream cronjob syntax. If omitted, the budget is always active.
	// Timezones are not supported.
	Schedule *string `json:"schedule,omitempty" hash:"ignore"`
	// Duration determines how long a Budget is active since each Schedule hit.
	// Only minutes and hours are accepted, as cron does not work in seconds.
	// If omitted, the budget is always active.
	// This is required if Schedule is set.
	Duration *metav1.Duration `json:"duration,omitempty" hash:"ignore"`
} // ... Same as Existing 

type DisruptionAction string 

const (
	All DisruptionAction = "All"
	Consolidation DisruptionAction = "Consolidation" 
	Drift DisruptionAction = "Drift" 
	Expiration DisruptionAction = "Expiration" 
	Emptiness DisruptionAction = "Emptiness" ? 
)
```

THis design proposes first defining Action Controls as a very barebones structure with a place in the api  
```go
// ActionControls defines the controls for a particular DisruptionAction, these controls are meant to apply for generic controls that apply to all disruption actions. 
type ActionControls struct {
	// Budgets is a list of Budgets. 
	// If there are multiple active budgets, Karpenter uses 
	// the most restrictive value. If left undefined, 
	// this will default to one budget with a value to 10%. 
	// +kubebuilder:validation:XValidation:message="'schedule' must be set with 'duration'",rule="!self.all(x, (has(x.schedule) && !has(x.duration)) || (!has(x.schedule) && has(x.duration)))" 
	// +kubebuilder:default:={{nodes: "10%"}} 
	// +kubebuilder:validation:MaxItems=50 
	// +optional 
	Budgets []Budget `json:"budgets,omitempty" hash:"ignore"` 
}
```

#### Example 

```yaml 
apiVersion: karpenter.sh/v1beta1
kind: NodePool
metadata:
  name: example-nodepool
spec:
  disruption:
    consolidationPolicy: WhenUnderutilized
    genericPerActionControls:
      Consolidation:
        disruptAfter: "30m"
        budgets:
          - nodes: "20%"
            schedule: "0 0 1 * *"
            duration: "1h"
      Drift:
        disruptAfter: "1h"
        budgets:
          - nodes: "10%"
            schedule: "0 0 * * 0"
            duration: "2h"
      Expiration:
        disruptAfter: "Never"
```

#### Considerations 
Some of the API choices for a given action seem to follow a similar pattern. These include ConsolidateAfter, ExpireAfter, and there are discussions about introducing a global DisruptAfter. Moreover, when discussing disruption budgets, we talk about adding behavior for each action. It appears there is a need for disruption controls within the budgets for each action, not just overall.

This approach aligns well with controls that apply to all existing actions. The proposal presented here is similar to the one mentioned above in relation to the actions we allow to be defined (All, Consolidation, Drift, Expiration, Emptiness).

This proposal is currently scoped for disruptionBudgets by action. However, we should also consider incorporating other generic disruption controls into the PerActionControls, even if we do not implement them immediately. Moving ConsolidateAfter and ExpireAfter into the per-action controls is a significant migration that requires careful planning and its own dedicated design. This proposal simply demonstrates a potential model that highlights the benefits of defining controls at a per-action level of granularity.

### Pros + Cons 
* 👍 This model starts to make more sense as we continue to add general behaviors that apply to all disruption actions where users will want control on the action level.
* 👍 Provides place per action for generic controls intended to be shared across all disruption methods. While not all methods share the same disruption actions, there are already cases for this. 
* 👍 Could extend other fields beyond generic values for specific actions with validation   
*  👎👎 Breaking API Change for Budgets at least, and if we decide to model DisruptAfter we also would have to break those apis. It might make sense to break the budgets now before they have garnered large adoption as it becomes harder to make the change as time goes on.
* 👎 Doesn't allow for easy defaulting of `All` disruption actions
* 👎 Adds complexity to the use of budgets, before its high level on the disruption controls but with this design approach its nested inside another field.


### Conclusion: Preferred Design
If the goal is to provide a simple, backward-compatible solution with immediate applicability, Approach A is more suitable. It provides a straightforward way to manage disruptions without overhauling the existing system.

However, if the goal is to create a more robust, future-proof system that can accommodate a wider range of disruptions and controls, Approach B is preferable. Despite its complexity and the need for API changes, it offers a more comprehensive solution that can evolve with users' needs.
