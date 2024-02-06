# Disruption Controls By Reason 
## User Scenarios 
1. Users need the capability to schedule upgrades only during business hours or within more restricted time windows. Additionally, they require a system that doesn't compromise the cost savings from consolidation when upgrades are blocked due to drift.
2. High-Frequency Trading (HFT) firms require full compute capacity during specific operational hours, making it imperative to avoid scale-down requests for consolidation during these periods. However, outside these hours, scale-downs are acceptable.
See Less Made Up Scenarios here: 
- https://github.com/kubernetes-sigs/karpenter/issues/924 
- https://github.com/kubernetes-sigs/karpenter/issues/753#issuecomment-1790110838 

## Known Requirements 
**Reason and Budget Definition:** Users should be able to define an reason and a corresponding budget(s).
**Supported Reasons:** All disruption Reasons affected by the current Budgets implementation (Consolidation, Emptiness, Expiration, Drift) should be supported. 
**Default Behavior for Unspecified Reasons:** Budgets should continue to support a default behavior for all disruption reasons. If an reason is unspecified, it is assumed to apply to all reasons. If a reason is unspecified, we apply the budget to be shared by all reasons that are unspecified. 

## API Design
### Approach A: Add a reason field to disruption Budgets 
This approach outlines a simple api change to the v1beta1 nodepool api to allow disruption budgets to specify a list of disruption methods. 

### Proposed Spec
Add a simple field "reasons" is proposed to be added to the budgets. 
```go
// Budget defines when Karpenter will restrict the
// number of Node Claims that can be terminating simultaneously.
type Budget struct {
      // Reasons is a list of methods for disruption that apply to this budget. If Reasons is not set, this budget applies to all methods.
      // If a reason == "default", it will apply to all reasons that don't have an active budget. If a reason is set, it will only apply to that method. If multiple reasons are specified,
      // this budget will apply to all of them. If a budget is not specified for a method, the default budget will be used.
      // allowed reasons are "default", "consolidation", "expiration", "emptiness", "drift"
      // +kubebuilder:validation:MaxItems=5
      // +kubebuilder:validation:Enum:={"default","consolidation","expiration","emptiness","drift"}
      // +optional
      Reasons []string `json:"reason,omitempty" hash:"ignore"`
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
      reason: [Drift]
      nodes: 10
    # For all other reasons , only allow 5 nodes to be disrupted at a time
    - nodes: 5
      schedule: "* * * * *"

```

In the original proposed spec, karpenter allows the user to specify up to [50 budgets](https://github.com/kubernetes-sigs/karpenter/blob/main/pkg/apis/v1beta1/nodepool.go#L96)

If there are multiple active budgets, karpenter takes the most restrictive budget. This same principle will be applied to the disruption budgets in this approach. The only difference in behavior is that each window will apply to list of reasons that are specifed rather than just all disruption methods.

### Pros + Cons 
* 👍 No nested definitions required 
* 👍👍 Extending existing budgets api. No Breaking API Changes, completely backwards compatible  
* 👎 With reason being clearly tied to budgets, and other api logic being driven by disruption Reason, we lose the chance to generalize per Reason controls 

### Approach B: Defining Per Reason Controls  
Ideally, we could move all generic controls that easily map into other reasons into one set of reason controls, this applies to budgets and other various disruption controls that could be more generic. 
### Proposed Spec 
```go
type Disruption struct {
    Default	  DisruptionSpec `json:defaults"`
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
          schedule: "0 0 * * 0"
          duration: "2h"
        - nodes: "50%" 
          schedule: "@monthly"
    expiration:
      disruptAfter: "Never"
```
#### Considerations 
Some of the API choices for a given reason seem to follow a similar pattern. These include ConsolidateAfter, ExpireAfter, and there are discussions about introducing a global DisruptAfter. Moreover, when discussing disruption budgets, we talk about adding behavior for each reason. It appears there is a need for disruption controls within the budgets for each reason, not just overall.

This approach aligns well with controls that apply to all existing reasons. The proposal presented here is similar to the one mentioned above in relation to the reasons we allow to be defined (Default, Consolidation, Drift, Expiration, Emptiness).

This proposal is currently scoped for disruptionBudgets by reason. However, we should also consider incorporating other generic disruption controls into the PerReasonControls, even if we do not implement them immediately. Moving ConsolidateAfter and ExpireAfter into the per-reason controls is a significant migration that requires careful planning and its own dedicated design. This proposal simply demonstrates a potential model that highlights the benefits of defining controls at a per-reason level of granularity.

### Pros + Cons 
* 👍 This model starts to make more sense as we continue to add general behaviors that apply to all disruption reasons where users will want control on the reason level.
* 👍 Provides place per reason for generic controls intended to be shared across all disruption Reasons. While not all Reasons share the same disruption reasons, there are already cases for this. 
* 👍 Could extend other fields beyond generic values for specific reasons with validation  
* 👍 Allows for very natural reason specification for further granularity of specifying specific K8sVersion upgrades for example. There is no need for complicated validation in regards to what Reason types are compatible with a particular Reason. This design easily evolves into per reason controls across the board. 
* 👎👎👎 Breaking API Change for Budgets at least, and if we decide to model DisruptAfter we also would have to break those apis. It might make sense to break the budgets now before they have garnered large adoption as it becomes harder to make the change as time goes on.
* 👎 Doesn't allow for easy defaulting of `default` disruption reasons
* 👎 Adds complexity to the use of budgets, before its high level on the disruption controls but with this design approach its nested inside another field.

### API Design Conclusion: Preferred Design
If the goal is to provide a simple, backward-compatible solution with immediate applicability, Approach A is more suitable. It provides a straightforward way to manage disruptions without overhauling the existing system. Breaking API changes in Approach B are likely too disruptive to customers. Approach A allows for the flexibility we need per budget. 

## Clarifying Expected Behavior 
### Q: How should Karpenter handle the default or undefined reason case? 
The current design involves specifying a specific number of disruptable nodes per reason, which can complicate the disruption lifecycle. For example, if there's a 10-node budget for "Drift" and a separate 10-node budget for "Consolidation", but a 15-node budget for "default" determining which nodes will get disrupted becomes unclear. Would it be 10 nodes for "Drift" and 5 nodes for "Consolidation"?

We could consider treating an undefined reason as a budget for all disruption reasons except for those with explicitly defined budgets. In this scenario, if a user specifies a disruption budget like this:

```yaml
spec: # This is not a complete NodePool Spec.
  disruption:
    budgets:
    - schedule: "* * * * *"
      reason: Consolidation
      nodes: 10
    - schedule: "* * * * *"
      reason: Drift
      nodes: 10
    # For all other reasons , only allow 5 nodes to be disrupted at a time
    - nodes: 5
      schedule: "* * * * *"
```

It means that "Consolidation" and "Drift" reasons have specific budgets of 10 nodes each, while all other reasons (e.g., expiration and emptiness) share a common budget of 5 nodes. This approach simplifies the configuration but has one limitation: it may not allow the execution of other disruption reasons if a specific reason exhausts the budget. This is a problem with the existing design for disruption budgets.

There are two ways for the users to get around this behavior. 
1. If you need guaranteed disruption for a particular action, you can just specify that action in a budget.  
2. We could allow some mechanism for the users to control the ordering of the disruption actions.

#### Q: Should users be able to change the order that disruption reasons are executed in to solve this problem? 
The answer is no, this makes it harder for cluster operators to understand behavior. It also doesn't elegantly fit into karpenters per nodepool controls. Defining it in the nodepool would mean you have multiple nodepools with different orderings, which is diffcult. Karpenter today does not provide an easy way via the CRDS to define per cluster level controls.  
#### Q: Should Budget "Reasons" allow for specifying more than one? Via an array? 

```go
// Budget defines when Karpenter will restrict the
// number of Node Claims that can be terminating simultaneously.
type Budget struct {
      // Reason defines the disruption reason that this particular disruption budget applies to. 
      Reason DisruptionReason `json:"reason,omitempty" hash:"ignore"`
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
      // Reason defines the disruption reason that this particular disruption budget applies to. 
      Reason []DisruptionReason `json:"reason,omitempty" hash:"ignore"`
      Nodes string `json:"nodes" hash:"ignore"`
      Schedule *string `json:"schedule,omitempty" hash:"ignore"`
      Duration *metav1.Duration `json:"duration,omitempty" hash:"ignore"`
}
```
The second approach allows for less redundancy. Budgets themselves already have a high level of granularity, and no way to share configuration at the cluster level for disruption settings or controls. This allows for elimination of another dimension of copy paste if I want to share a budget in my nodepools.

```yaml
yaml: 
  - nodes: 10
  reasons: [Drift, Consolidation]
  schedule: "* * * * *"
  - nodes: 5 
  reasons: [Emptiness] 
  schedule: "* * * * *" 
  - nodes: 100%
  
```

The conclusion is yes. Budget reasons should be reflected as a list instead of a single string.


#### Q: How should we handle an unspecfied default reason? 
```yaml
yaml: 
  - nodes: 10
  reasons: [Drift, Consolidation]
  schedule: "* * * * *"
  - nodes: 5 
  reasons: [Emptiness] 
  schedule: "* * * * *" 
  - nodes: 100%
  
```
In the case of a budget like above, default is undefined. Should karpenter assume the user doesn't want to disrupt any other reasons? Or should we assume that if a default is unspecified, they want us to disrupt anyway?  
The intuitive options if there is no active default budget is to allow disruption of either 0 or total number of nodes(meaning unbounded disruption).
Lets choose total number of nodes, since this allows the user to also specify periods where no nodes are to be disrupted of a particular type of disruption, and makes more sense with the existing karpenter behavior today.

