# Zone Spreading for Static NodePools

## Motivation

A static NodePool (`spec.replicas` set) keeps a fixed number of nodes whatever the pod demand. Its NodeClaims come straight from the NodePool template and skip the scheduler, so nothing balances them across zones. The cloud provider picks the zone for each NodeClaim from the zones the requirements allow, and several NodeClaims can end up in the same zone.

[`static-capacity.md`](./static-capacity.md) left this out on purpose. Spreading automatically whenever the `topology.kubernetes.io/zone` requirement lists several values would change what that requirement means today, which is a hard constraint. The same section says an explicit, declarative option "similar to `podTopologySpreadConstraints`" would be explored later. This RFC proposes the smallest version of that option.

This is [#3583](https://github.com/kubernetes-sigs/karpenter/issues/3583). The reporter runs a datastore on static NodePools and today creates one NodePool per zone by hand.

### Use Cases

1. **Datastore across zones.** An operator needs six nodes for a datastore, two per zone, and wants Karpenter to keep that shape when nodes are replaced or the count changes.
2. **Rolling drift.** An AMI update replaces static nodes one at a time. The replacement should land in the same zone as the node it replaces, so the spread survives the roll.
3. **Scaling a static pool.** `replicas` goes from 6 to 9. The three new nodes should fill the emptiest zones, and a later scale-down should shrink the fullest zones first.

### Non-Goals

- **Rebalancing existing nodes.** The field affects only NodeClaims created or removed after it is set. Karpenter will not delete healthy nodes to fix a skew.
- **Falling back to another zone when one has no capacity.** This version waits. A `whenUnsatisfiable` option can be added later without changing anything here (see Open Questions).
- **Spreading on keys other than zone.** The list shape leaves room for capacity type and others, but only zone is accepted in this RFC.
- **Dynamic NodePools.** Their pods already carry topology spread constraints and go through the scheduler.
- **Inferring zones from the provider.** Zones come from the NodePool's own requirements.

## Proposal

Add an optional `topologySpreadConstraints` list to the NodePool spec. It is accepted only on static NodePools and only with the `StaticCapacity` feature gate on. When it is unset, behavior does not change.

### Proposed Spec

```yaml
apiVersion: karpenter.sh/v1
kind: NodePool
spec:
  replicas: 6
  topologySpreadConstraints:
  - topologyKey: topology.kubernetes.io/zone
    maxSkew: 1
  template:
    spec:
      requirements:
      - key: topology.kubernetes.io/zone
        operator: In
        values: ["us-west-2a", "us-west-2b", "us-west-2c"]
```

The fields mean what they mean for pods:
- `topologyKey` must be `topology.kubernetes.io/zone` for now.
- `maxSkew` is the largest allowed difference between the most and least populated zone. Minimum 1.

The semantics match `DoNotSchedule` for pods. If the chosen zone cannot take a node, the NodeClaim waits and is retried, and Karpenter does not place it in another zone.

Validation (CEL, next to the existing static-pool rules):
- `topologySpreadConstraints` requires `replicas`.
- It requires a `topology.kubernetes.io/zone` requirement with operator `In` and at least two values, so the domains are explicit.
- At most one constraint, and its key is the zone key.

### How It Works

**Domains.** The zones in the NodePool's `In` requirement for `topology.kubernetes.io/zone`.

**Counting.** Per-zone counts of the NodePool's NodeClaims that are not deleting, kept in the cluster state next to the per-NodePool counts the static controllers already track, so concurrent reconciles see in-flight NodeClaims. A launched NodeClaim carries the zone as a label. One that has not launched yet carries its pinned zone requirement.

**Provisioning.** For each NodeClaim the static provisioning controller is about to create, it picks the zone with the lowest count (ties broken by zone name), adds a single-value zone requirement to that NodeClaim, and increments the count. The choice reuses the domain-selection logic the scheduler already uses for pod topology spread (`TopologyGroup`), so `maxSkew` means exactly what it means for pods. The NodePool's own requirements are untouched and remain the hard constraint.

**Scale-down.** The static deprovisioning controller keeps its current order: NodeClaims that have not launched, then empty nodes, then lowest disruption cost. Among NodeClaims at the same rank it prefers one from the zone with the most NodeClaims. It never removes a non-empty or higher-cost node only to fix skew.

**Drift and terminate-first replacement.** The replacement is pinned to the zone of the NodeClaim it replaces, so a rolling replacement does not change the distribution.

**Zones that cannot launch.** A NodeClaim pinned to a zone with no capacity fails to launch and is retried under the existing launch and backoff behavior. It never moves to another zone, so the pool stays short of `replicas` until the zone recovers. Existing nodes keep running.

### Interaction with Existing Features

- **`limits.nodes`:** unchanged. Spread picks a zone, not a count.
- **Drift:** requirements drift compares the NodePool's requirements to the NodeClaim's labels, so a NodeClaim pinned to one of the allowed zones is compatible and does not drift. If a zone is later removed from the requirement, NodeClaims in that zone drift as they do today.
- **Disruption budgets and the NodePool drift backoff:** unchanged.
- **Reserved capacity:** a zone with no reserved capacity left is treated like any other zone that cannot launch.

### Observability

- A NodePool event when a NodeClaim is waiting because its zone cannot launch, naming the zone.
- No new metric in this RFC. A per-zone count gauge would follow the existing metric label rules and can be added if operators ask for it.

### Edge Cases

- **Starting from an uneven pool.** Existing NodeClaims are counted as they are. New ones go to the lowest zones first, so the pool converges as it grows or as nodes are replaced. Nothing is deleted to speed that up.
- **A zone added to the requirement.** It starts at zero and receives the next new NodeClaims.
- **A zone removed from the requirement.** Its NodeClaims drift and are replaced into the remaining zones.
- **`replicas` not divisible by the number of zones.** With `maxSkew: 1`, six nodes over four zones is 2, 2, 1, 1.
- **A replacement while its zone is full.** The replacement waits, and the old node keeps running until it can be replaced in the same zone. That is deliberate. The alternative silently breaks the spread the operator asked for.

## Alternatives Considered

### Alternative 1: Spread implicitly whenever the requirement lists several zones

This is what `static-capacity.md` considered and rejected. It would change what an existing zone requirement means for every static NodePool already running, and would set a precedent for other requirement types.

### Alternative 2: One static NodePool per zone

This works today and is what the reporter does. It repeats the NodePool configuration per zone, the operator has to split `replicas` by hand, and nothing can move capacity between zones when one has trouble.

### Alternative 3: Let the cloud provider balance zones

The provider's `Create` already chooses a zone, so it could balance. That is not portable across providers, core cannot see or enforce it, and it cannot cover scale-down or drift.

### Alternative 4: A boolean such as `zoneSpread: true`

Smaller, but it has no way to express a tolerated skew, and it would need replacing the first time someone wants one.

### Alternative 5: Put the knob on the requirement

`minValues` is already a per-requirement option, so the zone requirement could carry the spread setting itself (for example a `maxSkew` next to `values`). That removes the repeated `topologyKey` and the cross-field validation. It also changes a type shared with NodeClaim requirements and every provider that reads them, which is a much larger change for a feature limited to static NodePools. This RFC keeps the field on the NodePool spec. If maintainers prefer the requirement-level shape, the behavior above does not change.

### Alternative 6: Include `whenUnsatisfiable` and a fallback to other zones now

This would match pod topology spread completely. The fallback is the hardest part to specify and test, because it depends on offering availability and on how failed launches back off. Leaving it out costs nothing for the reported use case and can be added later as an optional field that defaults to the behavior in this RFC.

## Backward Compatibility

The field is additive, optional and alpha, and is accepted only with `StaticCapacity` on. Existing static NodePools behave as before. CEL validation rejects the field on dynamic NodePools. Removing the field restores today's behavior for NodeClaims created afterward.

## Graduation Criteria

The field ships under the `StaticCapacity` feature gate and is alpha with it. It should graduate with static capacity, after providers have reported on how a zone with no capacity behaves in practice.

## Open Questions

1. **Shape.** Is a list that mirrors `podTopologySpreadConstraints` the right shape, or would a single object be simpler for a field that takes only one key today? Or the requirement-level option in Alternative 5?
2. **Location.** NodePool spec, as proposed, or `spec.template.spec`?
3. **Fallback.** Is waiting the right default when a zone has no capacity, with `whenUnsatisfiable: ScheduleAnyway` added later if operators ask for it?
4. **Explicit zones.** Is requiring an explicit `In` zone requirement of at least two values acceptable, or should domains come from the NodeClass or provider?
5. **Counting.** Is the zone label on a launched NodeClaim a reliable source for every provider, or should core also record the zone in a status field?
