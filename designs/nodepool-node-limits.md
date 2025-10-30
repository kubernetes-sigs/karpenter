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

There are two potential approaches for implementing `NodePool` node limits, each with different trade-offs:

### Option 1: Hard Limits

**Description**: Node limits are enforced as hard constraints that block both provisioning and graceful disruption (Consolidation and Drift) when the limit is reached. Graceful disruption is blocked because Karpenter requires the new nodes to be schedulable and ready before the old node can be terminated, which can lead to more nodes than the limit at one state.

**Benefits**:
- Guarantees the invariant that node count never exceeds the limit
- Simple and predictable behaviour for hard blocking node limits

**Drawbacks**:
- Can block disruption operations when at the limit, which may lead to suboptimal resource utilization
- Node limits will have inconsistent disruption semantics vs. other resources
- Does not provide a clear path for introducing hard for other resource limits in the future

### Option 2: Support Both Hard and Soft Limits

**Description**: Provide support for both hard and soft limit enforcement for nodes, allowing users to choose the appropriate mode based on their use case. Hard limits would block both provisioning and disruption when reached, while soft limits would only block provisioning (allowing graceful disruption to continue). This gives users the flexibility to apply strict constraints where necessary (licensing, IP exhaustion) while enabling optimization through disruption where constraints are less rigid.

**Benefits**:
- Flexibility to choose the appropriate enforcement model per use case
- Can use hard limits for critical physical constraints (IP addresses, licensing) and soft limits for cost optimization scenarios
- Allows graceful disruption to continue for resources with soft limits, enabling consolidation and drift even at capacity
- Provides a clear extensibility path for supporting hard limits on other resources in the future

**Drawbacks**:
- Additional configuration complexity for users to understand and configure
- More complex implementation and testing surface
- Requires careful API design and documentation to make the distinction between hard and soft limits clear and intuitive

### Recommended Approach

Based on covering all the use cases outlined in the [Motivation](#motivation) section, we recommend **Option 2: Support Both Hard and Soft Limits**.

While Option 1 (hard limits only) would meet the strict requirements, Option 2 provides valuable flexibility for users who want different enforcement behaviors for different scenarios. This also provides a clear path for introducing hard limits for other resources in a future enhancement.

## Other Considered Solutions

### Soft Limits Only

**Description**: Node limits would be enforced only during provisioning, allowing graceful disruption operations to continue even when at the limit.

**Benefits**:
- Graceful disruption functions continue to work, allowing Consolidation and Drift even at the limit.
- In steady state, nodes will stay under the limit (assuming the cloud provider allows the node limit to be exceeded)

**Drawbacks**:
- Intermittent states may temporarily exceed the node limit during disruption operations
- Potential undefined behavior if Karpenter allows more nodes at once, but the cloud provider blocks disruption at the infrastructure level (e.g., due to exhaustion of IP addresses)
    - This may lead to node provisioning timeout failures, and the user running into undesired node churn if disruption occurs.

This approach does not meet the requirements outlined in the [Motivation](#motivation) section. While it represents the most straightforward extension of Karpenter's existing limits mechanism, the ability to temporarily exceed node limits during disruption operations directly conflicts with strict use cases such as licensing constraints, and limited IP addresses.

For these reasons, this approach alone does not satisfy the stated goals of this RFC.

## Relationship to Static Capacity Design

This design builds upon the infrastructure established in the [Karpenter - Static Capacity](static-capacity.md) design, which already formally introduced node limits, but specifically for static capacity. This design is meant to support node limits for regular non-static `NodePool`s which scale up and down based on workload demands.

This feature borrows from the Static Capacity design:

- `nodepool.spec.limits.nodes`: The usage of a well-known resource key to define soft limits for constraining maximum node count per `NodePool`
- `nodepool.status.nodes`: Current node count tracking per `NodePool`
- Deprovisioning logic for node counts over the node limit

## Implementation Details

### NodePool Limits API Integration

NodePool `nodes` limits have already been defined in the `NodePool` spec as a supported resource type, as per the [Static Capacity design](static-capacity.md). However, the current semantics of the `spec.limits` field enforces **soft limits**.

```go
// In pkg/apis/v1/nodepool.go
type NodePoolSpec struct {
    // ... other fields
    Limits Limits `json:"limits,omitempty"`
}

type Limits v1.ResourceList
```

```yaml
# NodePool example
spec:
  limits:
    cpu: "100"
    memory: "500Gi"
    nodes: 10  # Currently soft - blocks provisioning only
```

This is challenge where if we implement node limits as a hard limit (blocking both provisioning and disruption) in `spec.limits.nodes`, then `nodes` would behave fundamentally differently than other resources (`cpu`, `memory`, etc.) in the same field.

Additionally, users may already be using `spec.limits.nodes` with the expectation of soft limit behavior, so changing the semantics would be a breaking change. We need to introduce some sort of new field to support hard limits.

#### API Design Options

Two approaches have been considered for supporting hard limits:

**Option A: Separate `hardLimits` field**

```yaml
spec:
  limits:          # Soft limits (existing behavior)
    cpu: "100"
    memory: "500Gi"
  hardLimits:      # New field for hard limits
    nodes: 10
```

**Benefits:**
- Backwards compatible - existing `spec.limits` continues working unchanged

**Drawbacks:**
- Two separate fields for conceptually similar functionality
- Requires clear documentation that the existing `spec.limits` fields are soft limits

**Option B: Nested soft/hard structure**

```yaml
spec:
  limits:
    soft:
      cpu: "100"
      memory: "500Gi"
    hard:
      nodes: 10
    cpu: 10  # Top-level for backwards compatibility
```

**Benefits:**
- Keeps related limit functionality under one `limits` field

**Drawbacks:**
- Backwards compatibility strategy of mixing nested and top-level syntax may be confusing

#### Recommended Approach

We recommend **Option A** (separate `hardLimits` field):

**Rationale:**
- Existing `spec.limits` behavior is unchanged, no backwards compatibility strategy is required
- Can easily add other resources (GPUs, specific instance types) to `hardLimits` when needed

**Validation:**
- A resource is allowed to be specified in both `limits` and `hardLimits`, but hard limits will take precedence over soft limits when both are present
- For this RFC, only `nodes` is supported in `hardLimits`

**Drift:**
- This field will be considered a behavioral field, identical to the existing `spec.limits` field.

### Provisioning

Both hard and soft node limits block provisioning of new nodes.

- Node limits are enforced in the simulated scheduling logic.
- Before creating a new `NodeClaim`, check if the `NodePool` has remaining node capacity.
    - This requires the `StateNode.Capacity()` implementation to include a `node` resource field of `1`, indicating that a single node is being requested.
    - This will allow the scheduler to correctly subtract the node capacity when updating the remaining resources for the `NodePool`.
- When node limits have been exhausted, new `NodeClaim` creation is blocked.

### Graceful Disruption (Consolidation and Drift)

Graceful disruption needs special handling because Karpenter Consolidation and Drift require new nodes to be schedulable and ready before old nodes can be terminated.

**For hard limits (`spec.hardLimits.nodes`):**

Hard limits block graceful disruption when the limit is reached. This is integrated into the NodePool Disruption Budget calculations:

1. For each NodePool, determine how many more nodes can be created before hitting the hard limit. If no hard limit is defined, treat it as unlimited.
2. Take the minimum of the remaining disruption budget and the remaining node limit to ensure we don't disrupt more nodes than either constraint allows.
3. Use this new limit to control how many nodes can be disrupted during operations like consolidation or drift.

This will allow us to enforce hard node limits for dynamic `NodePool`s during graceful disruption operations cleanly by leveraging the existing functionality for tracking disruption budget as a special case.

If the hard node limit is reached, Karpenter will publish a distinct event to indicate this, distinguishing from a regular disruption budget blocking event:

```bash
Normal  DisruptionBlocked  NodePool/default No allowed disruptions for disruption reason Underutilized due to node hard limit
```

**For soft limits (`spec.limits.nodes`):**

Soft limits do NOT block graceful disruption. Disruption operations can proceed even when at or above the soft limit, allowing temporary exceedance during node replacement.

**Note:** Forceful Methods (e.g., Expiration) are not affected by these changes to implement node limits, since those methods do not require node replacement.

### Deprovisioning when node count exceeds the limit

If more nodes exist in the cluster than the `NodePool` limit (soft or hard), Termination will be triggered to reduce the node count. We can rework the existing `getDeprovisioningCandidates` logic from the static deprovisioning controller to select which nodes to terminate in an ordered manner.

See this [section](static-capacity.md#deprovisioning-controller) in the static capacity design for more details on the deprovisioning logic.

If you decrease the node limit below the current node count, the new limit will be respected immediately and trigger the deprovisioning process to terminate excess nodes.

**Note**: Both `spec.limits` and `spec.hardLimits` are "Behavioral fields", which means changes to these fields do not trigger `Drift` on existing nodes. However, Karpenter will still react and enforce the new limit by terminating nodes through deprovisioning.

## Future Considerations

### Extension to other resources

The natural extension of this RFC is to support hard limits for other resources beyond nodes. For example, users may want to enforce hard limits on GPUs (`nvidia.com/gpu`), specific instance types, or other scarce resources. The `spec.hardLimits` field is designed to accommodate this expansion, allowing future RFCs to add support for additional resource types with the same hard limit enforcement semantics.

### Relationship to "terminateBeforeLaunch"

A future enhancement that has been discussed is a `terminateBeforeLaunch` mode for disruption, where Karpenter terminates the old node before launching the replacement (instead of the current launch-then-terminate). While this could help with hard limits by avoiding temporary exceedance, it comes with tradeoffs:

- **Capacity risk**: No guarantee that replacement capacity will be available after terminating the old node (InsufficientCapacityError)
- **Pod availability**: Evicted pods remain pending for longer periods
- **Consolidation quality**: Cannot guarantee optimal capacity selection

For these reasons, having configurable soft vs hard limits remains valuable regardless of whether `terminateBeforeLaunch` is added in the future.
