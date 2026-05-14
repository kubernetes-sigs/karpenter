# NodePool Repair Configuration RFC

## Motivation

While the existing Node Auto Repair feature provides CloudProvider-defined repair policies with fixed toleration
durations, different workloads have different sensitivities to node issues:

- **Critical, stateful applications** (e.g., databases, distributed storage) may require longer toleration durations
  to allow for manual intervention or graceful failover before node replacement. In these cases, automated repair
  could disrupt ongoing data replication or cause split-brain scenarios if triggered too quickly.
- **Stateless, fault-tolerant applications** (e.g., web frontends, batch processors) benefit from shorter tolerations
  for quicker recovery, as they can easily reschedule to healthy nodes.

The current CloudProvider-defined fixed durations cannot accommodate these different requirements within the same
cluster. Users need the ability to customize repair behavior per NodePool to match their workload characteristics.

**Important:** This design does not change the node repair mechanism to be a graceful disruption. Node repair remains
a forceful termination as described in the [Node Auto Repair design](./node-repair.md). Unhealthy nodes will not
respect customer-configured `terminationGracePeriod` or disruption budgets.

## Summary

This RFC proposes adding a new `repair` configuration block to the NodePool API, allowing users to override the
CloudProvider's default `TolerationDuration` for specific node conditions. The CloudProvider remains the source of
truth for which conditions to monitor and their default durations; the NodePool configuration only overrides the
timing.

## Proposal

### Why a Separate `repair` Stanza (Not Under `disruption`)

Node repair and disruption serve fundamentally different purposes:

- **Disruption** is a graceful lifecycle operation (consolidation, drift, expiration) that respects budgets,
  `terminationGracePeriod`, and `do-not-disrupt` annotations.
- **Repair** is a forceful response to unhealthy nodes that intentionally bypasses disruption controls, because
  an unhealthy node may be unable to gracefully drain pods or detach volumes.

Placing repair configuration under the `disruption` stanza would imply that repair operations respect disruption
controls, which they do not. A separate stanza makes this distinction clear.

### API Changes

Add a new `repair` block to the `NodePool` specification:

```go
type NodePoolSpec struct {
    // ... existing fields ...

    // Repair configures the repair policies for nodes in this NodePool.
    // When specified, NodePool-level repair policies can override the CloudProvider's
    // default TolerationDuration for specific node conditions.
    // +optional
    Repair *RepairSpec `json:"repair,omitempty"`
}

// RepairSpec defines the repair configuration for nodes in a NodePool.
type RepairSpec struct {
    // Policies defines the repair policies for different node conditions.
    // These policies override the CloudProvider's default TolerationDuration.
    // +optional
    Policies []NodePoolRepairPolicy `json:"policies,omitempty"`

    // DefaultTolerationDuration is the default duration to wait before repairing
    // nodes for any condition not explicitly configured in Policies.
    // If not specified, uses the CloudProvider's default TolerationDuration.
    // +optional
    DefaultTolerationDuration *metav1.Duration `json:"defaultTolerationDuration,omitempty"`
}

// NodePoolRepairPolicy defines a repair policy for a specific node condition.
type NodePoolRepairPolicy struct {
    // ConditionType specifies the node condition to monitor, e.g. "Ready", "DiskPressure".
    // Must match a ConditionType defined in the CloudProvider's RepairPolicies.
    ConditionType v1.NodeConditionType `json:"conditionType"`
    // ConditionStatus specifies the condition status that indicates unhealthy state,
    // e.g. "False", "True", or "Unknown".
    ConditionStatus v1.ConditionStatus `json:"conditionStatus"`
    // TolerationDuration is the duration to wait before repairing nodes matching this condition.
    // Overrides the CloudProvider's TolerationDuration for this specific condition.
    TolerationDuration metav1.Duration `json:"tolerationDuration"`
}
```

### CloudProvider Interface (Unchanged)

The CloudProvider interface remains exactly as defined in the existing Node Auto Repair implementation.
Users can only override the `TolerationDuration` for conditions that the CloudProvider already monitors:

```go
type RepairPolicy struct {
    ConditionType      corev1.NodeConditionType
    ConditionStatus    corev1.ConditionStatus
    TolerationDuration time.Duration
}

type CloudProvider interface {
    // ...
    RepairPolicies() []RepairPolicy
    // ...
}
```

Users cannot define new conditions to monitor through NodePool configuration. Only conditions defined by the
CloudProvider's `RepairPolicies()` are monitored. The NodePool configuration only controls the toleration
duration for those conditions.

### Condition Status Handling

Node conditions have three possible statuses: `True`, `False`, and `Unknown`. The meaning depends on the
condition type:

| Condition Type     | Unhealthy Status | Meaning                        |
| ------------------ | ---------------- | ------------------------------ |
| `Ready`            | `False`          | Node is not ready              |
| `Ready`            | `Unknown`        | Node status cannot be determined |
| `DiskPressure`     | `True`           | Node has disk pressure         |
| `NetworkUnavailable` | `True`         | Node network is unavailable    |

The `ConditionStatus` field in `NodePoolRepairPolicy` must match the `ConditionStatus` in the CloudProvider's
`RepairPolicy` for the override to take effect. This ensures that the NodePool override applies to the exact
same unhealthy condition the CloudProvider monitors.

### Resolution Order

The toleration duration for a given condition is resolved in this order:

1. **NodePool condition-specific policy**: If the NodePool's `repair.policies` contains a matching
   `conditionType` and `conditionStatus`, use its `tolerationDuration`.
2. **NodePool default**: If the NodePool has `repair.defaultTolerationDuration` set, use it as a
   fallback for conditions not explicitly configured.
3. **CloudProvider default**: Use the `TolerationDuration` from the CloudProvider's `RepairPolicy`.

### Controller Workflow

1. A diagnostic agent adds a status condition on a node.
2. Karpenter reconciles nodes and matches unhealthy conditions with repair policies from the CloudProvider.
3. For each matched condition, Karpenter resolves the `TolerationDuration` using the resolution order above.
4. The Node Health controller forcefully terminates the NodeClaim once the node has been in an unhealthy
   state for the resolved duration.

## Example Usage

### CloudProvider Implementation
```go
func (c *CloudProvider) RepairPolicies() []cloudprovider.RepairPolicy {
    return []cloudprovider.RepairPolicy{
        {
            ConditionType:      corev1.NodeReady,
            ConditionStatus:    corev1.ConditionFalse,
            TolerationDuration: 10 * time.Minute,
        },
        {
            ConditionType:      corev1.NodeReady,
            ConditionStatus:    corev1.ConditionUnknown,
            TolerationDuration: 10 * time.Minute,
        },
        {
            ConditionType:      "NetworkUnavailable",
            ConditionStatus:    corev1.ConditionTrue,
            TolerationDuration: 5 * time.Minute,
        },
    }
}
```

### NodePool Configuration
```yaml
apiVersion: karpenter.sh/v1
kind: NodePool
metadata:
  name: stateful-workloads
spec:
  template:
    spec:
      nodeClassRef:
        group: karpenter.k8s.aws
        kind: EC2NodeClass
        name: default
      requirements:
        - key: kubernetes.io/arch
          operator: In
          values: ["amd64"]
  repair:
    defaultTolerationDuration: 30m
    policies:
    - conditionType: Ready
      conditionStatus: "False"
      tolerationDuration: 45m
    - conditionType: NetworkUnavailable
      conditionStatus: "True"
      tolerationDuration: 10m
```

In this example:
- A `Ready=False` condition will wait **45 minutes** (NodePool override) instead of the CloudProvider's 10 minutes.
- A `Ready=Unknown` condition will wait **30 minutes** (NodePool default) instead of the CloudProvider's 10 minutes.
- A `NetworkUnavailable=True` condition will wait **10 minutes** (NodePool override) instead of the CloudProvider's 5 minutes.

## Backward Compatibility

- The `repair` field is optional (`omitempty`), ensuring full backward compatibility.
- Existing NodePools without repair configuration continue to work with CloudProvider defaults.
- No changes to the CloudProvider interface or existing APIs.

## Alternative Designs Considered

### Placing Repair Under the Disruption Stanza

Considered adding repair configuration under `spec.disruption.repair`. Rejected because repair is fundamentally
different from disruption: it is a forceful operation that does not respect disruption budgets, termination grace
periods, or do-not-disrupt annotations. Placing it under `disruption` would mislead users into thinking these
controls apply to repair operations.

### Allowing User-Defined Conditions

Considered allowing users to define new conditions to monitor (beyond what the CloudProvider defines). Rejected
for the initial implementation because the CloudProvider is the authoritative source for which conditions indicate
hardware or infrastructure failures. Allowing arbitrary user-defined conditions could lead to unintended node
terminations. This may be revisited in future iterations based on user feedback.
