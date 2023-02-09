# Deprovisioning Improvements for v1.0.0

## Background

Karpenter deprovisions nodes on [multiple signals](https://github.com/aws/karpenter-core/blob/main/pkg/controllers/deprovisioning/controller.go#L144-L161), orchestrated by the unified deprovisioning controller consisting of Expiration, Drift, Emptiness, Consolidation, Interruption for spot instances (technically not in the deprovisioning controller), and more in the future. Users currently enable deprovisioning methods through the Provisioner and the `karpenter-global-settings` ConfigMap. Once enabled, users can then toggle a portion of the behaviors at [different configuration levels](https://karpenter.sh/preview/concepts/deprovisioning/), or disable them altogether.

With the unified deprovisioning controller, all methods of deprovisioning are looped through iteratively and executed with the same process. For each deprovisioning action, Karpenter will discover all nodes that can be deprovisioned, compute a node (or nodes) to be deprovisioned, spin up any needed replacement nodes, and then finally deprovision the selected nodes.

## Pertinent Topics

### Recently Implemented - Drift

Drift is enabled in the `karpenter-global-settings` ConfigMap by a [feature gate](https://kubernetes.io/docs/reference/command-line-tools-reference/feature-gates/#overview) currently, but not for all provisioning constraints. As a completed feature, Drift must be pulled out of a feature gate, and Karpenter must support all modes of drift possible.

*Pain Point (1) - AWS Specific:* AMI drift can begin upgrading a user’s cluster without warning if a new EKS Optimized AMI is released. This can begin at any time of the day. Although this is safeguarded by ensuring replacement nodes initialize and pods schedule, Karpenter cannot ensure rescheduled pods will run flawlessly with a new AMI - this should be left to the user to validate. Solutions for this are discussed in [Native AMI Versioning](https://github.com/aws/karpenter/pull/3356).

*Pain Point (2):* In fully implementing Drift to support all modes, users may be confused about which fields on a Provisioner should make a node drifted. Karpenter’s `ProvisionerSpec` includes fields like `Weight` and `Limits` which can result in different provisioning logic, but has no bearing on deprovisioning Logic. The `ProvisionerSpec` should be re-organized to distinguish drift-able fields, in addition to clearly adding this in documentation.

### Unifying Deprovisioning

Deprovisioning is decentralized in the API while it is centralized in code. This is in contrast to provisioning, where Karpenter’s controller code base has both a provisioning and deprovisioning controller, and provisioning configuration is centralized in the `ProvisionerSpec` (*disregarding* `batchMaxDuration` and `batchIdleDuration` in the ConfigMap).

Consolidation, Expiration, and Emptiness are defined in the Provisioner. Expiration and Emptiness are defined at the top level with `ttlSecondsUntilExpired` and `ttlSecondsAfterEmpty`. Consolidation is enabled with its own subfield `consolidation.enabled: true`.

*Pain Point (3):* These fields are not defined at the same configuration level, and Drift is not defined in the Provisioner. This can create confusion on where configuration lives currently and where it should live in the future for contributors.

### Toggling Deprovisioning

Once each of the deprovisioning mechanisms are enabled, users have different [ways of disabling deprovisioning](https://karpenter.sh/preview/concepts/deprovisioning/#disabling-deprovisioning). If a user wants to block deprovisioning for a node, provisioner, or globally, users currently have four options, in no order:

1. Ensure at least one `do-not-evict` pod is scheduled to a node. (Configured at the Pod Level)
    1. *Pain Point (4):* Users must apply the `do-not-evict` annotation to at least one pod on all nodes-to-block, but then remove the annotation once each application is deprovisionable.
2. Disable deprovisioning by the controls they’re enabled with: Drift (Global), Expiration (Provisioner Level), Emptiness (Provisioner Level), and Consolidation (Provisioner Level).
    1. *Pain Point (5):* Disabling mechanisms requires editing the `ProvisionerSpec` and `karpenter-global-settings` ConfigMap to enable them again. This could have cross-team implications and requires critical RBAC permissions.
3. Use restrictive PDBs to block eviction on pods on the node. (Pod Level)
    1. *Pain Point (6):* This ensures availability at an application level, which may parallel how some teams model their provisioning. Yet, a cluster operator would need to ensure PDB owned pods are scheduled to nodes that they do not want deprovisioned, and that the PDB is restrictive enough. This is not a comprehensive method as pods can be created and deleted at any moment, allowing eviction of other pods.
4. Use the `do-not-consolidate` node annotation to disable consolidation. (Node Level)
    1. *Pain Point (7):* This does not disable any other deprovisioning mechanisms. It requires being patched out once the node can be consolidated, similar to the `do-not-evict` pod annotation.

To enable more toggling options for deprovisioning for nodes globally, owned by a Provisioner, or individually, users have asked for [Maintenance Windows](https://github.com/aws/karpenter/issues/1302) and [Provisioner Disabling](https://github.com/aws/karpenter/issues/2491). This document chooses Maintenance Windows as the solution.

*While interruption is technically considered deprovisioning, it is a forceful method, which is un-blockable once it’s enabled. This means that there are no toggling options for interruption once it’s enabled.*

## Proposed Solutions

### Maintenance Windows - Pain Points (1, 4, 5, 6, 7)

Maintenance Windows will be where users toggle deprovisioning after they’ve been enabled. Maintenance Windows will be a new Custom Resource to represent when users want to prohibit Karpenter from deprovisioning.

Maintenance Windows enable customers to say “what” deprovisioning actions and “when“ actions _*cannot*_ be taken on their Karpenter nodes. When deciding to deprovision a node, every Maintenance Window with a matching selector and schedule must allow the action (an AND semantic).

* Schedules (`[]Schedule`) - Time windows on when the Maintenance Window is active.
    * Cron (`string`) - Crontab where each hit represents the start of a schedule
    * Duration (`metav1.Duration`) - How long the schedule is valid since the last crontab hit
* Actions (`[]string`) - Deprovisioning action enums that will be blocked
* Selector (`[]v1.NodeSelectorRequirement`) - Which nodes will be blocked from being deprovisioned

```
apiVersion: karpenter.sh/v1alpha1
kind: MaintenanceWindow
metadata:
  name: nights
spec:
  schedules: // Multiple schedules for heterogenous durations
    - cron: "0 6 * * 1-5"
      duration: 1h
    - cron: "0 9 * * 1-5"
      duration: 10h
  actions: // Applies to
    - Expiration
    - Consolidation
    - Drift
    - Rebalance
  selector: # Nodes that match these
    matchExpressions: # v1.nodeSelectorTerms
    - key: topology.kubernetes.io/zone
      operator: In
      values: [ "us-west-2a" ]
```

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
```

This provides a solution to time-based deprovisioning blocking. Relying users to vend their own solution with a CronJob fails unsafe, which could result in unexpected application disruptions and node churn. Karpenter should always be able to correctly consider when to provision or deprovision nodes without relying on other independent moving parts to modify the functionality. This has the downside of being another API that users must have to learn, and another Custom Resource that Karpenter must manage.

### ProvisionerSpec API Reorganization - Pain Points (2, 3)

Each deprovisioning mechanism needs a way to both intelligently work for users that want a hands off experience and be extendable to the large pool of use cases in Karpenter. Karpenter needs to consider default behavior for the out-of-the-box experience and organize the `ProvisionerSpec` into a semantically similar way to what’s done in code.

* Model deprovisioning behavioral specs as TTL fields as Golang `metav1.duration` fields
* Move all behavioral specs to the top level (e.g. all Deprovisioning TTLs)
* Add all node template fields into the template section (e.g. providerRef, taints, startupTaints, labels, requirements, kubeletConfiguration)
    * This logical separation is discussed in [Node Templates and Drift](node-templates-and-drift.md)

```
apiVersion: karpenter.sh/v1beta1
kind: Provisioner
spec:
    template: {...}
    ttlAfterCreated: 1d       // metav1.Duration
    ttlAfterNotReady: 30m     // metav1.Duration
    ...
    (other provisioner fields, e.g. weight, limits)
```

#### 🔑 TTL Defaults

TTL defaults can be either persisted or runtime defaults. Runtime defaults will make each field a pointer (*metav1.Duration) where a deprovisioning mechanism uses a hard-coded TTL default for each deprovisioning mechanism or is disabled until a user defines it. A Persisted default will use an empty metav1.Duration{} field, and will rely on Provisioner Defaults to set the setting on the Provisioner, similar to the [OS and Architecture requirements](https://github.com/aws/karpenter/blob/main/pkg/apis/v1alpha5/provisioner.go#L45).

*Suggestion:* We should use a persisted default for the TTLs. This ensures that users can see which TTLs are being used on their Provisioners with kubectl instead of navigating the code or documentation for values. This enables all deprovisioning by default with chosen TTL values for each, requiring less work for newer users. Since deprovisioning will be always enabled, deprovisioning will need a sane way to disable deprovisioning, relying on Maintenance Windows to fill that gap.

### Complete Drift

As discussed above, Drift will be completed by pulling it out of the feature gate and enabling it by default, relying on Maintenance Windows for it to be disabled. Karpenter will also enforce all template provisioning constraints as well, which plugs in easily to the existing Drift mechanism.

Each case is validated in [Node Templates and Drift](node-templates-and-drift.md).

### Configurable ConsolidationTTL and EmptinessTTL Removal

A long-time item on the backlog is to remove emptiness since Consolidation has been fully implemented. Emptiness was the first deprovisioning method implemented, and only works for a small subset of use-cases. The core idea is to remove a node after some TTL if there are no applications running on it (excluding some [caveats](https://github.com/aws/karpenter-core/blob/main/pkg/controllers/node/emptiness.go#L87)). Consolidation not only removes nodes with no applications like Emptiness, but also re-schedules capacity elsewhere to “consolidate” applications to save on cost.

Although Consolidation can deprovision capacity in the same way that Emptiness does, they’re enabled with two different fields, where [Consolidation can be disruptive and Emptiness cannot](https://github.com/aws/karpenter/issues/2680#issuecomment-1341284018). Removing Emptiness would require making the ConsolidationTTL configurable (hard-coded as 15s currently). Since users cannot currently enable both Emptiness and Consolidation, only users of Emptiness should be affected by removing Emptiness. They’ll experience more proactive cost-reallocation, and can disable it with Maintenance Windows. Existing consolidation users will only gain more control.

To ease any potential pain, the Maintenance Windows above could also implement EmptyConsolidation and NonEmptyConsolidation as actions to delineate the two mechanisms once they’ve been merged. Some users may want automatic deprovisioning of nodes that are un-used by applications, but still have Consolidation compute better bin-packing.

### Native AMI Versioning

We can implement [Native AMI Versioning](https://github.com/aws/karpenter/pull/3356).

## Appendix

### Deprovisioning Issues

#### All Deprovisioning

1. Be more aggressive in termination with cordoning where a do-not-evict pod exists: https://github.com/aws/karpenter/issues/2988
2. Max Drain/Pod Eviction attempts: https://github.com/aws/karpenter/issues/3052
3. Custom Drain Flow: https://github.com/aws/karpenter/issues/2705
4. Node Auto Repair: https://github.com/aws/karpenter/issues/2021
5. Disable all deprovisioning by disabling a provisioner: https://github.com/aws/karpenter/issues/2491
6. General Maintenance Windows: https://github.com/aws/karpenter/issues/1302

#### Consolidation

1. Prioritize Bin-packing multiple spots instead of one large OD: https://github.com/aws/karpenter/issues/3063
2. Consolidation TTL: https://github.com/aws/karpenter/issues/3059
3. Spot → Spot Consolidation: https://github.com/aws/karpenter/issues/2741
4. Consolidate on a schedule: https://github.com/aws/karpenter/issues/2308
5. Preferences on consolidation replacement: https://github.com/aws/karpenter/issues/2486

### Example Maintenance Window Uses

1. No Consolidation, Rebalance, or Drift during critical work hours (9 AM - 5 PM) for all nodes owned by the two billing-teams in `us-west-2a`

```
apiVersion: karpenter.sh/v1alpha1
kind: MaintenanceWindow
metadata:
  name: critical
spec:
  schedules: // Multiple schedules for heterogenous durations
    - cron: "0 9 * * 1-5"
      duration: 8h
  actions: // Applies to
    - Consolidation
    - Drift
    - Rebalance
  selector: # Nodes that match these
    matchExpressions: # v1.nodeSelectorTerms
    - key: topology.kubernetes.io/zone
      operator: In
      values: [ "us-west-2a" ]
    - key: company.name/billing-label
      operator: In
      values: [ "team-a", "team-b" ]
```

2. No Drift during critical at night (5 PM -  9 AM) on weekdays and all weekend for all spot amd nodes owned by the default provisioner

```
apiVersion: karpenter.sh/v1alpha1
kind: MaintenanceWindow
metadata:
  name: nights-spot-amd
spec:
  schedules:
    - cron: "0 0 * * 1-5"
      duration: 9h
    - cron: "0 17 * * 1-5"
      duration: 8h
    - cron: "0 0 * * 6-7"
      duration: 24h
  actions:
    - Drift
  selector:
    matchExpressions:
    - key: karpenter.sh/provisioner-name
      operator: In
      values: [ "default" ]
    - key: kubernetes.io/arch
      operator: In
      values: [ "amd" ]
```
