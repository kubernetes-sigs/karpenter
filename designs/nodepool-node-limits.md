# NodePool Node Limits in Karpenter

## Motivation

Service providers and cluster administrators should have the ability to enforce maximum node counts per `NodePool` to address several critical use cases:

* **Licensing constraints**: Respect software licensing limits (e.g., monitoring agents with per-node licensing)
* **IP address management**: With fixed IP address pools, physically prevent node creation beyond the available IP capacity
* **Service Provider SLAs**: Guarantee service level agreements by preventing unlimited node provisioning
* **Resource limits**: Respect cloud provider account limits or organizational quotas with Karpenter-level control rather than relying on infrastructure-level restrictions

## Notes

- Enforcing global maximum Node limits is not a goal of this design.
- This design does not involve nodes managed outside of Karpenter.
- The Node limits discussed in this design are slightly different from the Node limits discussed in the [Karpenter - Static Capacity](static-capacity.md) design.
    - See the [Relationship to Static Capacity Design](#relationship-to-static-capacity-design) section for more details.

## Design Options

There are three potential approaches for implementing `NodePool` node limits, each with different trade-offs:

### Option 1: Hard Limits

**Description**: Node limits are enforced as hard constraints that block both provisioning and disruption when the limit is reached. Disruption is blocked because Karpenter requires the new nodes to be schedulable and ready before the old node can be terminated, which can lead to more nodes than the limit at one state.

**Benefits**:
- Guarantees the invariant that node count never exceeds the limit
- Consistent behavior across all operations

**Drawbacks**:
- Can block disruption operations when at the limit, which may lead to suboptimal resource utilization

### Option 2: Soft Limits

**Description**: Node limits are enforced only during provisioning, allowing disruption operations to continue even when at the limit.

**Benefits**:
- Disruption functions continue to work, allowing consolidation and drift even at the limit.
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

<!-- ### Recommended Approach: Hard Limits

Based on the critical use cases (e.g., IP address management), we recommend **Option 1: Hard Limits** as the initial implementation.

TODO(maxcao13): Want to hear from the community.
-->

## Relationship to Static Capacity Design

This design builds upon the infrastructure established in the [Karpenter - Static Capacity](static-capacity.md) design, which already formally introduced node limits, but specifically for static capacity:

- `nodepool.spec.limits.nodes`: `NodePool` limits for constraining maximum node count
- `nodepool.status.nodes`: Current node count tracking per NodePool
- Validation rules for handling node limits in NodePool specs

This design utilizes those existing fields for non-static `NodePool`s where node count can be limited by a hard or soft limit.

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

### Disruption

An easy way to implement the node limit functionality into the disruption logic is by integrating it into the disruption budget calculation:

```go
// In pkg/controllers/disruption/helpers.go
func getNodePoolRemainingNodeLimit(nodePool *v1.NodePool, nodeCount int) int {
    nodePoolNodeLimit := math.MaxInt32
    // Check if there's a defined node limit, if not there is no limit (set to a very high number)
    nodePoolLimits := corev1.ResourceList(nodePool.Spec.Limits)
    if nodeLimit, ok := nodePoolLimits[resources.Node]; ok {
        nodePoolNodeLimit = int(nodeLimit.Value())
    }
    
    // This is the remaining number of allocatable nodes as defined by the node limit in the NodePool.
    remainingNodeCount := nodePoolNodeLimit - nodeCount
    return remainingNodeCount
}

// In BuildDisruptionBudgetMapping
remainingNodeLimit := getNodePoolRemainingNodeLimit(nodePool, numNodes[nodePool.Name])
// We take the minimum of the remaining budget and the remaining node limit to ensure we don't disrupt more nodes than either allows.
disruptionBudgetMapping[nodePool.Name] = lo.Min([]int{remainingDisruptionBudget, remainingNodeLimit})
```

This will allow us to enforce the node limit for dynamic `NodePool`s during disruption operations cleanly by leveraging the existing functionality for tracking disruption budget as a special case.

If the node limit is reached, we publish a special event to indicate this, distinguishing from a regular disruption budget blocking event.

E.g., special event to indicate node limit blocking:
```bash
Normal  DisruptionBlocked  NodePool/default No allowed disruptions for disruption reason Underutilized due to node hard limit
```

Note that for implementing soft limits, handling for disruption budget can be ignored. Soft node limits should allow disruption operations to continue even when at the limit.

### Behavior and migration

- If no node limit is specified in NodePool limits, no limit applies.
- Existing NodePools without node limits continue to work unchanged.
- If there were already more nodes provisioned than the limit:
  - New nodes from the `NodePool` will not be provisioned until the node count is reduced to the limit.
  - In the case of hard limits, disruption will be blocked until the node count is reduced to the limit.
  - Termination will **not** be triggered to reduce the node count down to the limit.

### Validation

#### Option 1 and 2

No extra validation is needed.

#### Option 3

- `nodepool.spec.disruption.nodeLimitMode` must be set to `Hard` or `Soft`.
- defaults to `Hard`.
