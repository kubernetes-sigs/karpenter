# NodePool Repair Configuration RFC

## Summary

This RFC proposes adding a new `repair` configuration block to the NodePool API to give users fine-grained control over node repair behavior. This builds upon the existing node repair functionality by allowing per-condition toleration durations and workload-specific repair policies.

## Motivation

While cloud providers can define default repair policies, different workloads have different sensitivities to node issues:

- **Critical, stateful applications** may require longer toleration durations to allow for manual intervention or graceful failover
- **Stateless, fault-tolerant applications** benefit from shorter tolerations for quicker recovery

The current approach lacks the flexibility needed for users to tailor node repair behavior to their specific use cases.

## Proposal

### API Changes

Add a new `repair` block to the `NodePool` specification:

```go
type NodePoolSpec struct {
    // ... existing fields ...
    Disruption DisruptionSpec `json:"disruption"`

    // Repair configures the repair policies for nodes in this NodePool.
    // If not specified, repair policies will be disabled for this nodepool.
    Repair *RepairSpec `json:"repair,omitempty"`
}

type DisruptionSpec struct {
    // ... existing fields like consolidation, expireAfter ...
}

// RepairSpec defines the repair policies for nodes in this NodePool
type RepairSpec struct {
    // Policies defines a list of repair policies for specific node conditions. If a node has a condition
    // that matches a policy in this list, Karpenter will use the corresponding toleration duration.
    // This provides fine-grained control over repair timing for different issues.
    // These policies override the DefaultTolerationDuration.
	Policies []RepairPolicy `json:"policies,omitempty"`

    // DefaultTolerationDuration is the default duration to wait before repairing a node
    // for any condition that is not explicitly covered by a policy in the 'policies' list.
    // This acts as a fallback for any uncategorized or new node conditions.
    // If not specified, Karpenter will use the CloudProvider's default duration.
	DefaultTolerationDuration *metav1.Duration `json:"defaultTolerationDuration,omitempty"`
}

type RepairPolicy struct {
    // ConditionType is the type of the node condition, e.g. "Ready", "DiskPressure".
    ConditionType metav1.ConditionType `json:"conditionType"`
    // Toleration is the duration to wait before repairing a node for this specific condition.
    Toleration      metav1.Duration      `json:"toleration"`
}
```

### CloudProvider Interface

The CloudProvider interface remains focused on defining which conditions to monitor:

```go
type CloudProvider interface {
  ...
    // RepairPolicy is for CloudProviders to define a set of unhealthy conditions for Karpenter 
    // to monitor on the node.
    RepairPolicy() []RepairStatement
  ...
}

type RepairStatement struct {
    // Type of unhealthy state that is found on the node
    Type metav1.ConditionType 
    // Status condition of when a node is unhealthy
    Status metav1.ConditionStatus
}
```

### Resolution Order

The resolution order for `Toleration` duration is:
1. The `toleration` in a `RepairPolicy` within the NodePool's `repair.policies` for the specific condition type
2. The NodePool's `repair.defaultTolerationDuration`
3. CloudProvider's recommended default duration

### Controller Workflow

1. A diagnostic agent adds a status condition on a node
2. Karpenter reconciles nodes and matches unhealthy conditions with repair statements from the CloudProvider
3. Karpenter determines the `Toleration` using the `NodePool`'s repair configuration
4. The Node Health controller forcefully terminates the NodeClaim once the node has been in an unhealthy state for the determined duration

## Example Usage

### CloudProvider Implementation
```go
func (c *CloudProvider) RepairPolicy() []cloudprovider.RepairStatement {
    return []cloudprovider.RepairStatement{
        {
            Type: "Ready",
            Status: corev1.ConditionFalse,
        },
        {
            Type: "NetworkUnavailable",
            Status: corev1.ConditionTrue,
        },
        // ... additional conditions
    }
}
```

### NodePool Configuration
```yaml
apiVersion: karpenter.sh/v1beta1
kind: NodePool
metadata:
  name: default
spec:
  # ... other fields ...
  repair:
    defaultTolerationDuration: 30m
    policies:
    - conditionType: Ready
      toleration: 45m
    - conditionType: NetworkUnavailable
      toleration: 10m
```

### Node Repair Scenarios

**NetworkUnavailable Node:**
```yaml
apiVersion: v1
kind: Node
status:
  conditions:
    - lastTransitionTime: "2024-11-01T15:02:48Z"
      message: no connection
      reason: Network is not available
      status: "True"
      type: NetworkUnavailable
```
*Node eligible for repair at `2024-11-01T15:12:48Z` (10m after transition)*

**NotReady Node:**
```yaml
apiVersion: v1
kind: Node
status:
  conditions:
    - lastTransitionTime: "2024-11-01T15:02:48Z"
      message: kubelet is posting ready status  
      reason: KubeletReady
      status: "False"
      type: Ready
```
*Node eligible for repair at `2024-11-01T15:47:48Z` (45m after transition)*

## Implementation Considerations

### Alpha Implementation Constraints

The alpha implementation will use **forceful termination**:
- Unhealthy nodes will not respect customer-configured `terminationGracePeriod`
- Nodes will be forcefully terminated rather than gracefully drained
- This addresses cases where graceful termination may be blocked by broken pod eviction or volume detachment

### Future Enhancements

After the initial implementation, we will consider:
- Disruption controls (budgets, terminationGracePeriod) for unhealthy nodes
- Node reboot (instead of replacement)
- Configuration surface for graceful vs. forceful termination
- Availability zone resiliency considerations
- Customer-defined node conditions for repair (beyond CloudProvider-defined conditions)

## Alternative Designs Considered

### CloudProvider-Defined Fixed Durations

Initially considered having the CloudProvider define fixed toleration durations:

```go
type RepairPolicy struct {
    Type metav1.ConditionType 
    Status metav1.ConditionStatus
    TolerationDuration metav1.Duration  // Fixed duration
}
```

**Rejected because:** This approach lacks the flexibility users need to tailor repair behavior to different workload requirements. Different NodePools may need different toleration durations for the same condition type.

## Backward Compatibility

- The `repair` field is optional (`omitempty`), ensuring backward compatibility
- Existing NodePools without repair configuration will continue to work with CloudProvider defaults
- No breaking changes to existing APIs