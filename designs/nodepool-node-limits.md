# NodePool Node Limits in Karpenter

## Motivation

Service providers and cluster administrators should have the ability to enforce maximum node counts per `NodePool` to address several important use cases:

* **Licensing constraints**: Respect software licensing limits (e.g., monitoring agents with per-node licensing)
* **IP address management**: With fixed IP address pools, physically prevent node creation beyond the available IP capacity
* **Service Provider SLAs**: Guarantee service level agreements by preventing unlimited node provisioning
* **Resource limits**: Respect cloud provider account limits or organizational quotas with Karpenter-level control rather than relying on infrastructure-level restrictions

## Notes

- Enforcing global maximum Node limits is not a goal of this design.
- This design does not involve nodes managed outside of Karpenter.
- The Node limits discussed in this design are different from the Node limits discussed in the [Karpenter - Static Capacity](static-capacity.md) design, but involve similar concepts.
    - See the [Relationship to Static Capacity Design](#relationship-to-static-capacity-design) section for more details.

## Design Options

There are three potential approaches for implementing `NodePool` node limits, each with different trade-offs:

### Option 1: Hard Limits

**Description**: Node limits are enforced as hard constraints that block both provisioning and graceful disruption (Consolidation and Drift) when the limit is reached. Graceful disruption is blocked because Karpenter requires the new nodes to be schedulable and ready before the old node can be terminated, which can lead to more nodes than the limit at one state.

**Benefits**:
- Guarantees the invariant that node count never exceeds the limit
- Consistent behavior across all operations

**Drawbacks**:
- Can block disruption operations when at the limit, which may lead to suboptimal resource utilization

### Option 2: Soft Limits

**Description**: Node limits are enforced only during provisioning, allowing graceful disruption operations to continue even when at the limit.

**Benefits**:
- Graceful disruption functions continue to work, allowing Consolidation and Drift even at the limit.
- In steady state, nodes will stay under the limit (assuming the cloud provider allows the node limit to be exceeded)

**Drawbacks**:
- Intermittent states may temporarily exceed the node limit during disruption operations
- Potential undefined behavior if Karpenter allows more nodes at once, but the cloud provider blocks disruption at the infrastructure level (e.g., due to exhaustion of IP addresses)
    - This may lead to node provisioning timeout failures, and the user running into undesired node churn if  disruption occurs.

### Option 3: Configurable Hard/Soft Node Limits

**Description**: Allow users to configure whether limits are enforced as hard or soft constraints per NodePool. This can be configured via a new field in the NodePool spec `nodepool.spec.disruption.nodeLimitMode`.
The values can be `Hard` or `Soft`, defaulting to `Hard`.

**Benefits**:
- Flexibility to choose the appropriate enforcement model per use case
- Can use hard limits for critical constraints (IP addresses) and soft limits for cost optimization

**Drawbacks**:
- Additional configuration complexity for users
- More complex implementation

### Recommended Approach: Hard Limits

Based on covering all the use cases, we recommend **Option 1: Hard Limits** as the initial implementation.

## Relationship to Static Capacity Design

This design builds upon the infrastructure established in the [Karpenter - Static Capacity](static-capacity.md) design, which already formally introduced node limits, but specifically for static capacity. This design is meant to support node limits for regular non-static `NodePool`s which scale up and down based on workload demands.

This feature borrows from the Static Capacity design:

- `nodepool.spec.limits.nodes`: The usage of a well-known resource key to define limits for constraining maximum node count per `NodePool`
- `nodepool.status.nodes`: Current node count tracking per `NodePool`
- Deprovisioning logic for node counts over the node limit

## Implementation Details

### NodePool Limits Integration

NodePool `nodes` limits have already been softly defined in the `NodePool` spec as a supported resource type, as per the [Static Capacity design](static-capacity.md). The current implementation leverages this existing structure to enforce node limits for dynamic `NodePool`s.

```go
// In pkg/apis/v1/nodepool.go
type NodePoolSpec struct {
    // ... other fields
    Limits Limits `json:"limits,omitempty"`
}

type Limits v1.ResourceList
```

```yaml
spec:
  limits:
    # Prevents Karpenter from creating new nodes once the limit is exceeded
    nodes: 10
```

### Provisioning
- Node limits are enforced in the simulated scheduling logic.
- Before creating a new `NodeClaim`, check if the `NodePool` has remaining node capacity.
    - This requires the `StateNode.Capacity()` implementation to include a `node` resource field of `1`, indicating that a single node is being requested.
    - This will allow the scheduler to correctly subtract the node capacity when updating the remaining resources for the `NodePool`.
- When node limits have been exhausted, new `NodeClaim` creation is blocked.

### Graceful Disruption (Consolidation and Drift)

Graceful disruption needs to be handled because Karpenter Consolidation and Drift require new nodes to be schedulable and ready before old nodes can be terminated.

A way to handle these situations is by integrating it into the NodePool Disruption Budget calculations:

1. For each NodePool, determine how many more nodes can be created before hitting the limit. If no limit is defined, treat it as unlimited.
2. Take the minimum of the remaining disruption budget and the remaining node limit to ensure we don't disrupt more nodes than either constraint allows.
3. Use this new limit to control how many nodes can be disrupted during operations like consolidation or drift.

This will allow us to enforce the node limit for dynamic `NodePool`s during graceful disruption operations cleanly by leveraging the existing functionality for tracking disruption budget as a special case.

If the node limit is reached, Karpenter will publish a distinct event to indicate this, distinguishing from a regular disruption budget blocking event.

E.g., distinct event to indicate node limit blocking:
```bash
Normal  DisruptionBlocked  NodePool/default No allowed disruptions for disruption reason Underutilized due to node hard limit
```

Note that for implementing soft limits, handling for disruption budget can be ignored. Soft node limits should allow all disruption operations to continue even when at the limit.

Also note that Forceful Methods (e.g., Expiration) are not affected by these changes to implement node limits, since those methods do not require node replacement.

### Deprovisioning when node count exceeds the limit

If more nodes exist in the cluster than the `NodePool` limit, Termination will be triggered to reduce the node count. We can rework the existing `getDeprovisioningCandidates` logic from the static deprovisioning controller to select which nodes to terminate in an ordered manner.

See this [section](static-capacity.md#deprovisioning-controller) in the static capacity design for more details on the deprovisioning logic.

If you decrease the node limit below the current node count, the new limit will be respected immediately and trigger the deprovisioning process. Note that `spec.limits` is a "Behavioral field", and will not be considered for Drift, but Karpenter should still react to changes that lower the limit and start terminating nodes.

### Validation

#### Option 1 and 2

No extra validation is needed.

#### Option 3

- `nodepool.spec.disruption.nodeLimitMode` must be set to `Hard` or `Soft`.
- defaults to `Hard`.
