# Explored Deprovisioning Toggling Alternatives

## **THIS IS SUPPLEMENTAL TO [DEPROVISIONING FOR V1.0.0](deprovisioning-for-v1-0-0.md)**

Karpenter Deprovisioning can be toggled at a variety of levels. Through pod annotations, node annotations, provisioner spec fields, and global settings. This document will touch on all alternatives explored for toggling Deprovisioning. The suggested solution right now is to use Maintenance Windows.

1. No changes - Self-vended solution
2. [Disabling a Provisioner](https://github.com/aws/karpenter/issues/2491)
3. [Maintenance Windows](https://github.com/aws/karpenter/issues/1302) API Choices
4. More Node/Pod Annotations

## No Karpenter Changes

If Karpenter doesn’t provide a toggling solution, users would need to implement toggling themselves. This requires a lot of work for users and is not viable for a lot of users.

To achieve a time-based toggling system, they would need to create a CronJob. This requires managing an image for the CronJob pod to run, creating/validating a script for the Job pod, ensuring that there is capacity for the Job to schedule when it needs to be created, and managing RBAC permissions for the Job Pod. If any of these fail at the expected time, the CronJob could silently fail to trigger and not toggle the provisioner settings as expected. To programmatically toggle actions again, the user may need to create another CronJob to do the converse.

## Disabling a Provisioner

Disabling a Provisioner has an [open issue](https://github.com/aws/karpenter/issues/2491) and [PR](https://github.com/aws/karpenter-core/pull/66). The PR is implemented with the intent of only blocking Provisioning logic, not Deprovisioning logic. While the issues and PRs implement toggling provisioning or deprovisioning altogether, Karpenter could implement a similar toggling in a couple of ways.

### One control to rule them all

To disable a Provisioner from consideration for provisioning and deprovisioning, Karpenter could add a `disabled: true` field. This would act the same in Karpenter as if the Provisioner did not exist.

```
spec:
    disabled: false
```

While this may have use-cases in halting all Karpenter activity, this granularity of controls is not flexible or extensible. If a user would like to ensure their pods are running, but still take advantage of cost-saving deprovisioning, the user could not use this. A future API addition would need to delineate the two, allowing a user to disable deprovisioning, provisioning, or both.

### Disable Deprovisioning

Disabling the deprovisioning logic of a provisioner allows users to halt all voluntary disruptions to their cluster, while still enabling new workloads to schedule and run.

```
spec:
    disableDeprovisioning: true
```

This works well for users who only care about if applications are interrupted, without regard for the reason. Since this is a catch-all deprovisioning block, this does not allow for more granular controls like only disabling consolidation.

### Disable Deprovisioning Selectively

Disabling deprovisioning selectively will introduce a sub-section or boolean field for each deprovisioning method. Out of the three, this is the most extensible, and fits the wide range of possible cases.

```
spec:
    consolidation:
        enabled: false
    expiration:
        ttlSecondsUntilExpired: 10000
        enabled: false
    drift:
        forceEvict: true
        enabled: false
```

*Why not this alternative:* In addition to vending boolean fields for each deprovisioning method, users could also selectively disable other binary fields that are implemented in the future, but this still programmatically requires users to implement their own solutions. Adding additional boolean specs on top of the existing quantitative specs bloats the Provisioner, and may be modeled better another way. For instance, if each of the deprovisioning methods will only have one sub-field, they may fit better at the top level. It may also make the most sense to model enabling the mechanism as including the respective `TTL`. This has the same binary property as a boolean field.

## Maintenance Windows

Maintenance Windows indicate times where applications should or should not be disrupted. A user may only want deprovisioning when they know their clusters will experience less traffic. Conversely, a user may have many disruption-tolerant workloads, but don’t want deprovisioning to occur without cluster admins monitoring the overall health of the cluster.

Maintenance Windows are commonly implemented as a crontab with filters to indicate when it should be active for. This section discusses some alternatives of what Maintenance Windows API should look like and other design choices.

### Maintenance Scope/API Surface

#### In-line with Provisioner

In-lining Maintenance with a Provisioner sets Maintenance Windows to refer to only nodes spawned by that Provisioner. For users that use a Provisioner per type of workload, or per groupings of applications, an in-line Provisioner Maintenance Window takes advantage of nodes being owned by a Provisioner.

```
spec:
    maintenanceCron: "* * * * 1-5"
    maintenanceDurationSeconds: 3600
```

The inherent downside of an in-line Provisioner is the lack of extensibility to/across Provisioners and across pods. If a cluster operator wants to stop deprovisioning across the whole cluster, they need to copy this Maintenance Window to every provisioner in the cluster to ensure no nodes are deprovisioned. In addition, if a cluster operator’s Provisioner requirements are highly flexible, being more driven at the pod-level, executing Maintenance Windows at a Provisioner level may inhibit users from taking advantage of Karpenter’s provisioning magic while enabling application-based maintenance.

#### Custom Resource

Implementing a [Custom Resource](https://kubernetes.io/docs/concepts/extend-kubernetes/api-extension/custom-resources/) means that Karpenter will have to support API Versions and vending more custom resources through the Helm Chart for installation. Creating a dedicated CR for Maintenance Windows easens placement of additional features in the future.

#### Exclusionary Windows

Users with clusters generally tolerant to disruption could use Exclusionary Windows to represent their busiest hours, specifying actions that cannot be used.

An Exclusionary Window is the inverse of a Maintenance Window and could be implemented as a boolean value in a Maintenance Window or as a separate Exclusionary Window CR. *We’ll refer to a Maintenance Window as a Deny Window, and an Exclusionary Window as an Allow Window.*

*Alternative 1:*
Merge both types of windows into one Custom Resource.

```
apiVersion: karpenter.sh/v1alpha1
kind: MaintenanceWindow
metadata:
  name: disable-everything
spec:
  // no selector, matches everything
  // no schedules, matches everything
  type: Allow | Deny
  actions:
    - Expiration // disable expiration
    - Consolidation
    - Drift
    - Rebalance
```

*Alternative 2:*
Keep both types of windows as two separate Custom Resources. Since they would have identical specs, this redundancy would be better put into a type field as above.

```
apiVersion: karpenter.sh/v1alpha1
kind: MaintenanceWindow
metadata:
  name: disable-everything
spec:
  // no selector, matches everything
  // no schedules, matches everything
  actions:
    - Expiration // disable expiration
    - Consolidation
    - Drift
    - Rebalance
---
apiVersion: karpenter.sh/v1alpha1
kind: ExclusionaryWindow
metadata:
  name: allow-everything
spec:
  // no selector, matches everything
  // no schedules, matches everything
  actions:
    - Expiration // disable expiration
    - Consolidation
    - Drift
    - Rebalance
```

Given the ability to specify an allow or deny window, this table shows that an allow window is logically redundant.

|                        | Deprovisioning Enabled? | in Allow Window? | in Deny Window? | Result             |
|------------------------|-------------------------|------------------|-----------------|--------------------|
|                        | Yes                     | Yes              | Yes             | No Deprovisioning  |
|                        | Yes                     | Yes              | No              | Yes Deprovisioning |
|                        | Yes                     | No               | Yes             | No Deprovisioning  |
|                        | Yes                     | No               | No              | Yes Deprovisioning |
|                        | No                      | Yes              | Yes             | No Deprovisioning  |
|                        | No                      | Yes              | No              | No Deprovisioning  |
|                        | No                      | No               | Yes             | No Deprovisioning  |
|                        | No                      | No               | No              | No Deprovisioning  |
| Logical Representation | A                       | B                | C               | (A && !C)          |

*Why not this alternative:* In the table, we only deprovision if the Deprovisioning is enabled, and it’s not in a deny window. Therefore, we’ll just stick with a Maintenance Window to not add configuration redundancies.

## Node and Pod Labels and Annotations

While dedicated configuration fields can help cluster operators, we may want to implement more granular node and pod labels and annotations, similar to the `do-not-evict` pod annotation and `do-not-consolidate` node annotation.

Since pods are driven/created by application teams and nodes are created by the template of the Provisioner, relying on annotations decentralizes controls on capacity away from the hands of the cluster operator.

### Toggling By Node

Annotating nodes programmatically through Karpenter can be done in the provisioner with the [annotations field](https://github.com/aws/karpenter-core/blob/main/pkg/apis/v1alpha5/provisioner.go#L35). Users can add these annotations in their Provisioner, and could drive the toggling behavior through users responsible for the nodes.

To enable `provisioner.Annotations` to drive the toggling logic, Karpenter could add the following, which could extend into any other deprovisioning logic in the future.

1. `do-not-deprovision`
   - Valuable for users that model their teams to their own nodes. If not programmatically used, teams can ensure their nodes are not disrupted, but become ultimately responsible for deprovisioning their nodes, losing all programmatic functionality.
2. `do-not-expire`
   - Not expiring a node could have security implications. Since these node-uptime requirements may be imperative, this may not be a desired annotation.
3. `do-not-drift`
   - Does not make sense since removing this annotation will technically drift it from the Provisioner, so which would be right? Should it have the annotation, or not? Even if annotations were not considered in the purview of drift, this blurs the line between node requirements and pod requirements.

### Toggling By Pod

Annotating pods programmatically is done by the application creator. Driving toggling through the user parallels the hands-off idea that Karpenter will spin up whatever needed capacity, given the workload requirements for applications.

Similar to the nodes, pod annotations could include reasons why applications should or should not be disrupted. Since deployments can be spread across nodes, Karpenter would need to check each pod that has deprovisioning blocker before it can continue. Yet, in most cases of deprovisioning logic, having pods drive consolidation or other deprovisioning methods is backwards. A pod should only be aware of whether it needs to be disrupted or not, and the node’s metadata will signal what type of deprovisioning should be possible.

*Why not this alternative:* Toggling by pods and nodes gives the reins to the application developers, where managing node deprovisioning actions should be wholly driven by the cluster operator, to assert that the state of the cluster complies with company requirements.
