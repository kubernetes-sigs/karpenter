# Disruption Controls

Karpenter is including new Disruption Controls in its `v1beta1` APIs. These APIs include a new `Disruption` block in the `NodePool` that will contain all scale-down behavioral fields. Initially, this will include `Budgets`, which define a (1) parallelism of how many nodes can be deprovisioned at a time and a (2) cron schedule that determines when the parallelism applies. Disruption Controls will also include a `ConsolidateAfter` field that will allow users to affect the speed at which Karpenter will scale down underutilized nodes.

## Proposed Spec

This only includes the `Budgets` and `ConsolidateAfter` portion of the disruption spec.

```{yaml}
apiVersion: karpenter.sh/v1beta1
kind: NodePool
metadata:
  name: default
spec:
  consolidationPolicy: WhenUnderutilized || WhenEmpty
  consolidateAfter: 10m || Never # metav1.Duration
  disruption:
    budgets:
    # On Weekdays during business hours, don't do any deprovisioning.
    - crontab: "0 9 * * mon-fri"
      duration: 8h
      maxUnavailable: 0
    # Every other time, only allow 10 nodes to be deprovisioned simultaneously
    - maxUnavailable: 10
```

## Code Definition

```{go}
type Disruption struct {
    {...}
    // Budgets is a list of Budgets.
    // If there are multiple active budgets, the most restrictive is respected.
    Budgets []Budget `json:"budgets,omitempty" hash:"ignore"`
    // ConsolidateAfter is a nillable duration, parsed as a metav1.Duration.
    // Users can use "Never" to disable Consolidation.
    ConsolidateAfter *NillableDuration `json:"consolidateAfter" hash:"ignore"`
    // ConsolidationPolicy determines how Karpenter will consider nodes
    // as candidates for Consolidation.
    // WhenEmpty uses the same behavior as v1alpha5 TTLSecondsAfterEmpty
    // WhenUnderutilized uses the same behavior as v1alpha5.ConsolidationEnabled: true
    ConsolidationPolicy string `json:"consolidationPolicy" hash:"ignore"`
}
// Budget specifies periods of times where Karpenter will restrict the
// number of Node Claims that can be terminated at a time.
// Unless specified, a budget is always active.
type Budget struct {
    // MaxUnavailable dictates how many Node Claims owned by this NodePool
    // can be terminating at once.
    // This only respects and considers nodes with a DeletionTimestamp set.
    MaxUnavailable intstr.IntOrString `json:"maxUnavailable" hash:"ignore"`
    // Crontab specifies when a budget begins being active.
    // Crontab uses the same syntax as a Cronjob:
    // "Minute Hour DayOfMonth Month DayOfWeek"
    // This is required if Duration is set.
    Crontab *string `json:"crontab,omitempty" hash:"ignore"`
    // Duration determines how long a Budget is active since each Crontab hit.
    // This is required if Crontab is set.
    Duration *metav1.Duration `json:"duration,omitempty" hash:"ignore"`
}
```

## Validation/Defaults

For each `Budget`, users must set a non-negative `MaxUnavailable`. Users can disable scale down for a NodePool by setting this to `0`. Users must either omit both `Crontab` and `Duration` or set both of them, since `Crontab` and `Duration` are inherently linked. Omitting these two fields will be equivalent to an always active `Budget`. If a user defines seconds for `Duration`, Karpenter will ignore it, effectively rounding down to the nearest minute, since the smallest denomination of time in upstream Crontabs are minutes.

- An omitted `Budgets` will be defaulted to one `Budget` with `MaxUnavailable: 10%` if left undefined.
- `ConsolidationPolicy` will be defaulted to `WhenUnderutilized`, with a `consolidateAfter` value of `15s`, which is the same value for Consolidation in v1alpha5.

```
apiVersion: karpenter.sh/v1beta1
kind: NodePool
metadata:
  name: default
spec:
  disruption:
    consolidationPolicy: WhenUnderutilized
    consolidateAfter: 15s
    budgets:
    - maxUnavailable: 10%
```

### Defaulting Alternatives Explored

If Karpenter were to persist a default for the `Crontab` and `Duration` fields, it would need to pick sane values for an “always enabled” Budget. The proposal was to default `Crontab` to `* * * * *` and `Duration` to `1m`, but since `Duration` has no impact outside of the `Crontab`, users who want to set their `Crontab` but forget to set `Duration` may find this a sharp edge.

## Pros and Cons For In-line with the NodePool

Adding the `Budgets` field into the `NodePool` implicitly defines a per-NodePool grouping for the `Budgets`. In [API Alternatives Explored](#api-alternatives-explored), it was considered to make this its own CRD, where there would be a `NodeSelector` field in `Budgets`, allowing users to select a set of nodes by labels. This would allow each `Budget` to refer to nodes irrespective of the owning `NodePool`. For clusters that restrict user permissions to certain `NodePools`, making a new independent CRD for `Budgets` would allow some teams to impact behavior of other users.

* 👍 Karpenter doesn't have to create/manage another CRD
* 👍 All disruption fields are colocated, which creates a natural spot for any future disruption-based behaivoral fields to live. (e.g. `terminationGracePeriodSeconds`, `rolloutStrategy`, `evictionPolicy`)
* 👎 Introduces redundancy for cluster admins that use the same `Disruption` settings for every `NodePool`. If a user wanted to change a parallelism throughout their cluster, they’d need to persist the same value for all known resources. This means users cannot set a cluster-scoped Node Disruption Budget.

## API Alternatives Explored

### New Custom Resource

Adding a new Custom Resource would require adding an additional `Selector` Field which references a set of nodes with a label set. It would have the same fields as the `Budget` spec above.

#### Pros + Cons

* 👍 Using a selector allows a `Budget` to apply to as many or as little NodePools as desired
* 👍 This is a similar logical model as pods → PDBs, an already well-known Kubernetes concept.
* 👎 Users must reason and manage about another CR to understand when their nodes can/cannot be terminated
* 👎👎 Application developers shouldn’t be able to have the ability to create/modify this CR, since they could control disruption behaviors for other NodePools that they shouldn’t have permissions to control.
* 👎 There will be disruption fields on both the Node Pool and the new CR, potentially causing confusion for where future configuration should reside.
  * Generally, we cannot migrate `consolidateAfter` and `expireAfter` fields to this new Custom Resource, as overlapping selectors will associate multiple TTLs to the same nodes. This introduces additional complexity for users to be aware of when these fields apply (in relation to the time-based field), and requires Karpenter to implement special cases to handle these conflicts.

### New Custom Resource with Ref From Node Pool

This would create a new Custom Resource which is referenced within the `NodePool` akin to how the `Provisioner` references a `ProviderRef`. This would exclude the `Selector` field mentioned in the approach above, as each CR would be inherently linked to a `NodePool`.

#### Pros + Cons

* 👍 All disruption-based fields will live on the same spec.
* 👍 Karpenter already uses this mechanism which is done with `Provisioner → ProviderRef` / `NodePool -> NodeClass`.
* 👎 If we wanted to align all Disruption fields together, migrating the other `consolidateAfter` and `expireAfter` fields over to a new could confuse users.
