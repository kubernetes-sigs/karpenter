# RFC: Configurable Readiness TTL via Node Auto Repair

## Motivation

Karpenter currently assumes nodes will reach the **Ready** state within a fixed time. This can be problematic for workloads with lengthy startup procedures. For example:

- **GPU driver installation** during node bootstrap (drivers and CUDA libraries can take extra time)
- **Windows node initialization** (Windows AMIs often run substantial setup tasks on first boot)
- **Network plugin or CNI startup taints** (waiting for a CNI DaemonSet to remove startup taints)
- **Large container images or pre-pulled archives** that delay node readiness

Rather than creating a separate timeout mechanism, we propose leveraging Karpenter's existing **node auto repair** system to handle nodes that fail to become Ready within a configurable timeout.

## Background: Node Auto Repair

Karpenter v1.1.0+ includes a node auto repair feature that:
- Monitors nodes for unhealthy conditions (e.g., `Ready=False`, `NetworkUnavailable=True`)
- Waits for a toleration duration before taking action
- Enforces a 20% safety threshold (won't repair if >20% of nodes are already unhealthy)
- Forcefully terminates and replaces unhealthy nodes

The system is implemented in `pkg/controllers/node/health/controller.go` and uses cloud provider-defined **RepairPolicies** to determine which conditions are unhealthy and how long to tolerate them.

## Proposed Solution

### Overview

We will extend the node auto repair system to support a configurable readiness timeout by:

1. **Adding a new field** `spec.template.spec.readinessTTL` to NodePool
2. **Propagating to NodeClaims** so each NodeClaim has its own readiness timeout
3. **Creating a dynamic RepairPolicy** for nodes that remain NotReady beyond their TTL
4. **Reusing the existing health controller** to detect and replace nodes that don't become Ready in time

### Configuration

```yaml
apiVersion: karpenter.sh/v1
kind: NodePool
metadata:
  name: gpu-nodepool
spec:
  template:
    metadata: {}
    spec:
      expireAfter: 144h0m0s
      readinessTTL: 30m  # Wait 30 minutes for GPU drivers to install
      requirements:
        - key: karpenter.sh/capacity-type
          operator: In
          values: ["on-demand"]
```

**Key Points:**
- Defaults to `15m` to match current behavior
- Propagates from NodePool → NodeClaim at creation time
- Stored in `nodeClaim.spec.readinessTTL`
- Only affects initial startup; not used for drift detection

### Implementation Details

#### 1. API Changes

Add to `NodeClaimSpec` in `pkg/apis/v1/nodeclaim.go`:

```go
type NodeClaimSpec struct {
    // ... existing fields ...

    // ReadinessTTL defines how long to wait for the node to become Ready
    // before considering it unhealthy. Defaults to 15 minutes.
    // +kubebuilder:validation:Type="string"
    // +kubebuilder:validation:Pattern="^([0-9]+(\\.[0-9]+)?(ns|us|µs|ms|s|m|h))+$"
    // +kubebuilder:validation:Schemaless
    // +optional
    ReadinessTTL *metav1.Duration `json:"readinessTTL,omitempty"`
}
```

Add to `NodePoolSpec.template.spec` in `pkg/apis/v1/nodepool.go` (same field).

#### 2. NodePool Controller Changes

When creating NodeClaims, propagate `readinessTTL`:

```go
// In pkg/controllers/provisioning/scheduling/scheduler.go
nodeClaim.Spec.ReadinessTTL = nodePool.Spec.Template.Spec.ReadinessTTL
```

#### 3. Health Controller Changes

Modify `pkg/controllers/node/health/controller.go` to generate dynamic repair policies:

```go
func (c *Controller) getRepairPolicies(ctx context.Context, node *corev1.Node) []cloudprovider.RepairPolicy {
    policies := c.cloudProvider.RepairPolicies()

    // Add readiness TTL policy if configured
    nodeClaim, err := nodeutils.NodeClaimForNode(ctx, c.kubeClient, node)
    if err == nil && nodeClaim.Spec.ReadinessTTL != nil {
        // Only apply readiness TTL if node hasn't become Ready yet
        readyCondition := nodeutils.GetCondition(node, corev1.NodeReady)
        if readyCondition.Status != corev1.ConditionTrue {
            policies = append(policies, cloudprovider.RepairPolicy{
                ConditionType:       corev1.NodeReady,
                ConditionStatus:     corev1.ConditionFalse,
                TolerationDuration:  nodeClaim.Spec.ReadinessTTL.Duration,
            })
            // Also handle Unknown status
            policies = append(policies, cloudprovider.RepairPolicy{
                ConditionType:       corev1.NodeReady,
                ConditionStatus:     corev1.ConditionUnknown,
                TolerationDuration:  nodeClaim.Spec.ReadinessTTL.Duration,
            })
        }
    }

    return policies
}
```

Update `findUnhealthyConditions()` to use dynamic policies:

```go
func (c *Controller) findUnhealthyConditions(ctx context.Context, node *corev1.Node) (nc *corev1.NodeCondition, cpTerminationDuration time.Duration) {
    requeueTime := time.Time{}
    policies := c.getRepairPolicies(ctx, node)  // Use dynamic policies

    for _, policy := range policies {
        // ... rest of logic unchanged
    }
    return nc, cpTerminationDuration
}
```

#### 4. Lifecycle Controller Integration

The existing lifecycle controller already handles nodes that fail to become Ready, but with a hardcoded timeout. We'll remove that logic and rely on the health controller instead:

- Remove hardcoded readiness timeout from `pkg/controllers/nodeclaim/lifecycle/liveness.go`
- Let health controller handle all readiness timeout scenarios
- Existing metrics and events will continue to work

### Behavior

#### Normal Flow (Node Becomes Ready in Time)

1. NodeClaim created with `readinessTTL: 30m`
2. Node registers with `Ready=Unknown` at T+1m
3. Node transitions to `Ready=True` at T+25m
4. Health controller sees Ready=True, no action taken
5. Node continues normal lifecycle

#### Timeout Flow (Node Doesn't Become Ready)

1. NodeClaim created with `readinessTTL: 30m`
2. Node registers with `Ready=Unknown` at T+1m
3. At T+31m, node is still `Ready=Unknown` (exceeded TTL)
4. Health controller detects unhealthy condition
5. If <20% of nodes are unhealthy:
   - Annotates NodeClaim with termination timestamp
   - Deletes NodeClaim
   - Lifecycle controller terminates instance
   - NodePool controller provisions replacement
6. If ≥20% of nodes are unhealthy:
   - Publishes `NodeRepairBlocked` event
   - Requeues for 5 minutes

### Advantages Over Separate Timeout

1. **Reuses existing infrastructure** - No duplicate timeout logic
2. **Safety by default** - Gets 20% threshold protection automatically
3. **Consistent behavior** - Same forceful termination as other health checks
4. **Better observability** - Uses existing health metrics and events
5. **Simpler codebase** - Less code to maintain

### Backward Compatibility

- Default `readinessTTL: 15m` matches current hardcoded behavior
- Existing NodePools without `readinessTTL` continue working unchanged
- No breaking API changes

### Alternatives Considered

#### 1. Separate Liveness Timeout (Original Approach)

The original PR proposed a separate `maxNodeReadyTime` timeout in the lifecycle controller. This was rejected because:
- Duplicates health checking logic
- No safety threshold protection
- More code complexity
- Inconsistent with other health checks

#### 2. Startup Taints

Reviewer @ellistarn suggested using startup taints instead of preventing kubelet registration. However:
- Readiness TTL is still needed to detect nodes stuck in initialization
- Startup taints are complementary, not a replacement
- Users may want both: startup taints for workload scheduling + readiness TTL for node lifecycle

## Open Questions

### 1. Should readinessTTL be in NodeClass instead of NodePool?

**Pros of NodeClass:**
- GPU vs non-GPU nodes often differ by instance type/AMI (NodeClass concerns)
- Allows different timeouts for different hardware configurations

**Cons of NodeClass:**
- NodePool is already used for lifecycle settings (`expireAfter`, `terminationGracePeriod`)
- Different teams may want different timeouts for the same hardware
- Less flexible for organizational boundaries

**Decision:** Start with NodePool for consistency with other lifecycle settings. Can add NodeClass support later if needed.

### 2. What happens if readinessTTL is changed after NodeClaims are created?

Since `readinessTTL` is propagated at NodeClaim creation time (not referenced dynamically), changing the NodePool value won't affect existing NodeClaims. This is intentional and matches the behavior of other immutable NodeClaim properties.

### 3. Should readinessTTL apply after node becomes Ready once?

No. Once a node transitions to `Ready=True`, the readiness TTL has served its purpose. If a node later becomes `NotReady`, the cloud provider's standard repair policies (e.g., 30m for Ready=False) will apply, not the readiness TTL.

### 4. Interaction with existing lifecycle timeout?

We will remove the existing hardcoded readiness timeout from the lifecycle controller and rely entirely on the health controller. This centralizes all health-based replacement logic.

## Implementation Plan

1. **Phase 1: API Changes**
   - Add `readinessTTL` field to NodeClaim and NodePool specs
   - Add validation and defaults
   - Generate CRDs

2. **Phase 2: NodePool Controller**
   - Propagate `readinessTTL` from NodePool to NodeClaim

3. **Phase 3: Health Controller**
   - Add dynamic repair policy generation
   - Update `findUnhealthyConditions()` to use dynamic policies
   - Add metrics for readiness timeouts

4. **Phase 4: Lifecycle Controller Cleanup**
   - Remove hardcoded readiness timeout logic
   - Ensure health controller fully owns readiness failures

5. **Phase 5: Testing**
   - Unit tests for dynamic policy generation
   - E2E tests for readiness timeout scenarios
   - Test 20% threshold protection

6. **Phase 6: Documentation**
   - Update NodePool API docs
   - Add examples for GPU and Windows use cases
   - Document interaction with startup taints

## Success Criteria

- Users can configure `readinessTTL` per NodePool
- Nodes that don't become Ready within TTL are automatically replaced
- 20% safety threshold prevents cascading failures
- Existing NodePools without `readinessTTL` continue working unchanged
- No duplicate timeout logic in codebase
