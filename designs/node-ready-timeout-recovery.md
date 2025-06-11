# Node Ready Timeout Recovery

## Problem Statement

Nodes can sometimes fail to reach a ready state due to various issues such as:
- Network connectivity problems during bootstrap
- Issues with cloud provider instance initialization
- Problems with container runtime or kubelet startup
- Resource constraints during node initialization

Currently, Karpenter has a node repair feature that handles nodes with specific unhealthy conditions defined by cloud providers. However, there's no mechanism to automatically recover nodes that simply fail to become ready within a reasonable timeframe. These nodes can remain in a NotReady state indefinitely, consuming cloud resources without contributing to cluster capacity.

This feature addresses the need for automatic recovery of nodes that fail to reach a ready state within a configurable timeout period.

## Goals

1. Provide automatic recovery for nodes that fail to become ready within a specified timeout
2. Make the feature configurable via feature gate
3. Ensure the feature works alongside existing node repair mechanisms
4. Follow Karpenter's established patterns for disruption and node lifecycle management

## Non-Goals

1. Handle nodes that become unhealthy after initially being ready (covered by existing node repair)
2. Provide complex recovery strategies beyond node replacement
3. Handle nodes with specific cloud provider defined conditions (covered by existing node repair)

## Proposed Solution

### Feature Gate

Add a new feature gate `NodeReadyTimeoutRecovery` that enables this functionality:

```go
type FeatureGates struct {
    NodeRepair              bool
    ReservedCapacity        bool
    SpotToSpotConsolidation bool
    NodeReadyTimeoutRecovery bool // New feature gate
}
```

### Configuration

The timeout duration will be configurable via environment variable with a reasonable default:

- **Environment Variable**: `NODE_READY_TIMEOUT`
- **Default Value**: `10m` (10 minutes)
- **Description**: Time to wait for a node to become ready before considering it for recovery

### Controller Implementation

A new controller `NodeReadyTimeoutController` will:

1. Monitor nodes managed by Karpenter
2. Track the time since node creation
3. Check if nodes are in Ready state
4. Trigger node replacement for nodes that exceed the timeout while remaining NotReady

### Recovery Logic

1. **Target Nodes**: Only nodes with `karpenter.sh/nodepool` label (Karpenter-managed)
2. **Grace Period**: Wait for the configured timeout period from node creation time
3. **Ready Check**: Verify the node's Ready condition status
4. **Recovery Action**: Delete the NodeClaim to trigger replacement via standard Karpenter mechanisms

### Integration with Existing Features

- **Node Repair**: This feature complements existing node repair by handling a different failure mode
- **Disruption Controls**: Will respect existing disruption budgets and controls
- **Health Checks**: Works alongside cloud provider health policies

## Implementation Details

### Controller Structure

```go
type Controller struct {
    clock         clock.Clock
    recorder      events.Recorder
    kubeClient    client.Client
    cloudProvider cloudprovider.CloudProvider
    timeout       time.Duration
}
```

### Recovery Decision Logic

1. Check if feature gate is enabled
2. Verify node is managed by Karpenter
3. Check if node creation time + timeout < current time
4. Verify node Ready condition is False or Unknown
5. Check disruption budgets and health thresholds
6. Delete NodeClaim to trigger replacement

### Metrics

Add metrics to track recovery actions:

```go
var NodeReadyTimeoutRecoveredTotal = opmetrics.NewPrometheusCounter(
    crmetrics.Registry,
    prometheus.CounterOpts{
        Namespace: metrics.Namespace,
        Subsystem: metrics.NodeClaimSubsystem,
        Name:      "ready_timeout_recovered_total",
        Help:      "Number of nodeclaims recovered due to ready timeout",
    },
    []string{
        metrics.NodePoolLabel,
        metrics.CapacityTypeLabel,
    },
)
```

### Events

Emit events when recovery actions are taken:

```go
func NodeReadyTimeoutRecovery(node *corev1.Node, nodeClaim *v1.NodeClaim, timeout time.Duration) events.Event {
    return events.Event{
        InvolvedObject: node,
        Type:           corev1.EventTypeWarning,
        Reason:         "NodeReadyTimeoutRecovery",
        Message:        fmt.Sprintf("Node failed to become ready within %v, triggering recovery", timeout),
    }
}
```

## Alternatives Considered

### 1. Extend Existing Node Repair

**Pros**: Reuse existing infrastructure
**Cons**: Different concern (timeout vs specific conditions), would complicate existing logic

### 2. Make Timeout Configurable per NodePool

**Pros**: More granular control
**Cons**: Adds API complexity, most users likely want cluster-wide setting

### 3. Implement Recovery Strategies

**Pros**: More sophisticated recovery options
**Cons**: Adds complexity, node replacement is the most reliable recovery method

## Testing

1. **Unit Tests**: Controller logic, timeout calculations, recovery decisions
2. **Integration Tests**: End-to-end scenarios with various node states
3. **Feature Gate Tests**: Verify feature can be enabled/disabled properly

## Security Considerations

- Feature respects existing RBAC permissions
- No new API surface exposed
- Uses existing node deletion mechanisms

## Rollout Plan

1. **Alpha**: Behind feature gate, disabled by default
2. **Beta**: Enabled by default with sufficient testing
3. **GA**: Stable feature integrated with core Karpenter

## Future Enhancements

1. Per-NodePool timeout configuration
2. Different recovery strategies (reboot vs replace)
3. Integration with cluster autoscaler patterns
4. Advanced health checks before recovery 