# Disruption Controls By Reason
## TOC and Overview
1. [Overview](#overview)
2. [User Scenarios](#user-scenarios)
3. [Known Requirements](#clarifying-the-requirements-and-behavior)
   - [Q: Default or Undefined Reason Case Handling](#q-default-or-undefined-reason-case-handling)
   - [Q: Calculation of Allowed Disruptions in a Multi-Reason World](#q-calculation-of-allowed-disruptions-in-a-multi-reason-world)
   - [Q: Handling an Unspecified Default Reason](#q-handling-an-unspecified-default-reason)
   - [Q: Definition of Default in Budgets](#q-definition-of-default-in-budgets)
4. [API Design](#api-design)
   - [Approach A: Extending the Budget API to Specify a Reason](#approach-a-extending-the-budget-api-to-specify-a-reason)
      * [List Approach: Multiple Reasons per Budget](#list-approach-multiple-reasons-per-budget)
      * [Single Reason Approach: One Reason per Budget](#single-reason-approach-one-reason-per-budget)
      * [Pros and Cons for Both Approaches](#pros-and-cons-for-both-approaches)
      * [Preferred Option: List Approach](#preferred-option-list-approach)
   - [Approach B: Defining Per Reason Controls](#approach-b-defining-per-reason-controls)
      * [Pros and Cons](#pros-and-cons)
   - [API Design Conclusion: Preferred Design](#api-design-conclusion-preferred-design)

## User Scenarios 
1. Users need the capability to schedule upgrades only during business hours or within more restricted time windows. Additionally, they require a system that doesn't compromise the cost savings from consolidation when upgrades are blocked due to drift.
2. Users want to minimize workload disruptions during business hours but still want to be able to delete empty nodes throughout the day.  That is, empty can run all day, while limiting cost savings and upgrades due to drift to non-business hours only.

See Less Made Up Scenarios here: 
- https://github.com/kubernetes-sigs/karpenter/issues/924 
- https://github.com/kubernetes-sigs/karpenter/issues/753#issuecomment-1790110838
- https://github.com/kubernetes-sigs/karpenter/issues/672

## Clarifying the requirements and behavior 
**Reason and Budget Definition:** Users should be able to define an reason and a corresponding budget(s).
**Supported Reasons:** All disruption Reasons affected by the current Budgets implementation (underutilized, empty, expired, drifted) should be supported. 
**Default Behavior for Unspecified Reasons:** Budgets should continue to support a default behavior for all disruption reasons. 


### Q: How should Karpenter handle the default or undefined reason case? 
If a budget reason is unspecified like budgets[1], we will assume this budget applies to all actions that are not specified 
```yaml
budgets: 
  - nodes: 10
    reasons: [drifted, underutilized]
    schedule: "* * * * *"
  - nodes: 30 
    schedule: "* * * * *" 
```
Meaning that for any actions other than drifted + consolidation, the total amount of disrupting + unhealthy nodes has to be less than 30 for them to trigger disruption.

### Q: How should allowed disruptions be calculated in a multi-reason world? 
The calculation of allowed disruptions in a system with multiple disruption reasons has become more intricate following the introduction of Disruption Budget Reasons. Previously, the formula was straightforward:

AllowedDisruptions = minNodeCountOfAnActiveBudget - unhealthyNodes - totalDisruptingNodes.

When calculating by reason, two potential equations emerge:
1. AllowedDisruptionByReason = minNodeCountOfActiveBudget[reason] - unhealthyNodes - totalDisruptingNodes[reason]
2. AllowedDisruptionByReason = minNodeCountOfActiveBudget[reason] - unhealthyNodes - totalDisruptingNodes
The second equation is the one we should opt into.

Take this budget as an example
```yaml
budgets: 
  - nodes: 15 
    reasons: [drifted, underutilized]
    schedule: "* * * * *"
  - nodes: 10
    reasons: [drifted]
    schedule: "* * * * *"
  - nodes: 5 
    schedule: "* * * * *" 
```
First, calculate minNodeCountOfActiveBudget. Given there are two budgets for drifted, we select the lower number:

```
minNodeCountOfActiveBudget = {
  drifted: 10 
  underutilized: 15 
  Default: 5
}
```

In the first equation, tracking the number of nodes disrupted by each reason is required. For example:
```
disrupting = {
  drifted: 3 
  underutilized: 6
  Expriation: 3
  empty: 2
}
```

Assuming zero unhealthy nodes:
**First Equation**
- drifted = 10 - 3: Disruption allowed as drifted bucket isn't full.
- underutilized = 15 - 6: Disruption allowed since 15 - 6 > 0.
- Default = 5 - expired - empty = 0: No disruption allowed for reasons other than consolidation and drift.

**Second Equation**
- Total Disrupting = 3 + 6 + 5 = 14.
- drifted = 10 - 14: No disruption allowed due to drift.
- underutilized = 15 - 14: Disruption allowed for at least one node due to consolidation.
- Default: 5 - 14: No disruption allowed for other methods.

#### Decision
The second equation simplifies reasoning for cluster operators. A node can be tagged for multiple disruption reasons (e.g., empty, drifted, underutilized), making it challenging to precisely calculate in-flight disruptions. The second equation streamlines this process by providing a clearer view of the overall disruption impact.
### Q: How should we handle an unspecified reason when others are specified? 
```yaml
budgets: 
  - nodes: 10
    reasons: [drift, underutilized]
    schedule: "* * * * *"
  - nodes: 5 
    reasons: [empty] 
    schedule: "* * * * *" 
```
In the case of a budget like above, default is undefined. Should karpenter assume the user doesn't want to disrupt any other reasons? Or should we assume that if a default is unspecified, they want us to disrupt anyway?  
The intuitive options if there is no active default budget is to allow disruption of either 0 or total number of nodes(meaning unbounded disruption).
Lets choose total number of nodes, since this allows the user to also specify periods where no nodes are to be disrupted of a particular type of disruption, and makes more sense with the existing karpenter behavior today.

# API Design
## Approach A: Extending the Budget API to specify a reason 
This section contrasts two approaches for specifying disruption reasons in the v1beta1 nodepool API: a list of reasons (List Approach) and a single reason per budget (Single Reason Approach).

### List Approach: Multiple Reasons per Budget - Recommended
This approach allows specifying multiple disruption methods within a single budget entry. It is proposed to add a field Reasons to the budgets, which can include a list of reasons this budget applies to.
#### Proposed Spec
Add a simple field "reasons" is proposed to be added to the budgets. 
```go
// Budget defines when Karpenter will restrict the
// number of Node Claims that can be terminating simultaneously.
type Budget struct {
      // Reasons is a list of methods for disruption that apply to this budget. If Reasons is not set, this budget applies to all methods.
      // If a reason is set, it will only apply to that method. If multiple reasons are specified,
      // this budget will apply to all of them. If a budget is not specified for a method, the default budget will be used.
      // allowed reasons are "underutilized", "expired", "empty", "drift"
      // +kubebuilder:validation:MaxItems=5
      // +kubebuilder:validation:Enum:={"consolidation","expired","empty","drift"}
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
      // the upstream cronjob syntax. If omitted, the budget is always active. // Timezones are not supported. // This field is required if Duration is set. // +kubebuilder:validation:Pattern:=`^(@(annually|yearly|monthly|weekly|daily|midnight|hourly))|((.+)\s(.+)\s(.+)\s(.+)\s(.+))$` // +optional Schedule *string `json:"schedule,omitempty" hash:"ignore"` // Duration determines how long a Budget is active since each Schedule hit. // Only minutes and hours are accepted, as cron does not work in seconds.
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
#### Example
```yaml
apiVersion: karpenter.sh/v1beta1
kind: NodePool
metadata:
  name: default
spec: # This is not a complete NodePool Spec.
  disruption:
    budgets:
    - schedule: "* * * * *"
      reason: [drifted, underutilized]
      nodes: 10
    # For all other reasons , only allow 5 nodes to be disrupted at a time
    - nodes: 5
      schedule: "* * * * *"

```

In the original proposed spec, karpenter allows the user to specify up to [50 budgets](https://github.com/kubernetes-sigs/karpenter/blob/main/pkg/apis/v1beta1/nodepool.go#L96)
If there are multiple active budgets, karpenter takes the most restrictive budget. This same principle will be applied to the disruption budgets in this approach. The only difference in behavior is that each window will apply to list of reasons that are specifed rather than just all disruption methods. 
### Pros + Cons 
👍👍 Flexibility in Budget Allocation: Allows more flexibility in allocating budgets across multiple disruption reasons.
👍👍 Reduced Configuration Complexity: Simplifies the configuration process, especially for similar settings across multiple reasons.
👎 Potential for API Complexity: There might be confusion over whether the node count is shared between actions or if each action gets the node count individually. 

Note some pros and cons between A + B can be shared, and are in a list next to the pros + cons for Single Reason Approach 

#### Single Reason Approach: One Reason per Budget

### Proposed Spec
In this approach, each budget entry specifies a single reason for disruption.

```go
// Budget defines when Karpenter will restrict the
// number of Node Claims that can be terminating simultaneously.
type Budget struct {
      // +kubebuilder:validation:Enum:={"consolidation","expired","empty","drift"}
      // +optional
      Reason string `json:"reason,omitempty" hash:"ignore"`
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
      reason: drifted 
      nodes: 10
    # For all other reasons , only allow 5 nodes to be disrupted at a time
    - nodes: 5
      schedule: "* * * * *"

```


#### Pros and Cons
👍 Simplicity and Clarity: Offers a straightforward and clear mapping of budget to disruption reason.
👎 Increased Configuration Overhead: Requires duplicating settings for multiple reasons, increasing setup complexity.
👎 Less Flexibility: Lacks the flexibility to share a budget across multiple reasons.

#### Pros and Cons for List and Per Reason Definitions
Some of the Pros and Cons are shared by both list and single reason, as they have the same advantages and disadvantages in comparison to Per Reason Controls 
* 👍👍 Extends Existing API:  No Breaking API Changes, completely backwards compatible
* 👍 No Nesting Required: Leaves budgets at the top level of the api.
* 👎 Limited Generalization of Reason Controls: With reason being clearly tied to budgets, and other api logic being driven by disruption reason, we lose the chance to generalize per Reason controls. If we ever decide we need a place per action,  there will be some duplication for reason. 


### Preferred Option: List Approach
Given the comparison, the preferred design is the List Approach. It provides the necessary flexibility for managing multiple disruption reasons under a single budget, while reducing configuration complexity. This approach extends the existing API without introducing breaking changes and simplifies management for scenarios where multiple disruption reasons share similar constraints.

### Approach: Defining Per Reason Controls  
Ideally, we could move all generic controls that easily map into other reasons into one set of reason controls, this applies to budgets and other various disruption controls that could be more generic. 
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
```
#### Considerations 
Some of the API choices for a given reason seem to follow a similar pattern. These include ConsolidateAfter, ExpireAfter, and there are discussions about introducing a global DisruptAfter. Moreover, when discussing disruption budgets, we talk about adding behavior for each reason. It appears there is a need for disruption controls within the budgets for each reason, not just overall.

This approach aligns well with controls that apply to all existing reasons. The proposal presented here is similar to the one mentioned above in relation to the reasons we allow to be defined (underutilized, drifted, expired, empty).

This proposal is currently scoped for disruptionBudgets by reason. However, we should also consider incorporating other generic disruption controls into the PerReasonControls, even if we do not implement them immediately. Moving ConsolidateAfter and ExpireAfter into the per-reason controls is a significant migration that requires careful planning and its own dedicated design. This proposal simply demonstrates a potential model that highlights the benefits of defining controls at a per-reason level of granularity.

### Pros + Cons 
* 👍 Granular Control for Each Reason: This model aligns well with the need for specific controls for each disruption reason, providing a natural specification for further granularity. 
* 👍 Foundation for Future Extensions: Offers a framework for incorporating other generic disruption controls into the per-reason controls, extending beyond budgets.
* 👍👍 Customization for Specific Reasons: Allows for specific validations and extensions for individual reasons, offering tailored control.
* 👎👎👎 Breaking API Changes:Implementing this approach would necessitate significant changes to the current API, especially for budgets. If we decide to model DisruptAfter in a similar way we also would have to break those apis. It might make sense to break the budgets now before they have garnered large adoption as it becomes harder to make the change as time goes on.
* 👎 Complexity in Default Reason Handling: The model does not facilitate easy defaulting for unspecified ("default") disruption reasons.
* 👎 Increased Budget Complexity: The use of budgets becomes more complex as they are now nested within another field, adding a layer of intricacy to their application.

### API Design Conclusion: Preferred Design
After evaluating different approaches to extend the Karpenter API for specifying disruption reasons, the preferred design is the List Approach in Approach A. This approach offers flexibility in managing multiple disruption reasons under a single budget and reduces configuration complexity. It extends the existing API without introducing breaking changes and simplifies management for scenarios where multiple disruption reasons share similar constraints.

While the idea of per-reason controls (Approach B) provides granular control and a foundation for future extensions, it involves significant API changes and increased complexity, making it less favorable at this stage. However, this approach remains a viable option for future considerations, especially if there is a need for more tailored control over each disruption reason.

