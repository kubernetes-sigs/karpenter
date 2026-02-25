# Capacity Buffer API Support

## Background

Karpenter currently operates as a dynamic cluster autoscaler, provisioning nodes just-in-time based on pending pod demand. However, several important use cases require maintaining pre-provisioned spare capacity:

1. **Performance-critical applications** where just-in-time provisioning latency is unacceptable
2. **Burst workloads** that need immediate scheduling for CI/CD, batch jobs, or event-driven applications  
3. **High-availability services** that require buffer capacity to handle traffic spikes or node failures
4. **Intent-driven capacity configuration** allowing users to declaratively express "I want X amount of buffer capacity"

Currently, users attempt to achieve buffer capacity through workarounds:
- Deploying "balloon pods" or placeholder workloads to maintain minimum capacity
- Using pause containers with resource requests to reserve capacity
- Over-provisioning through static NodePools or manual capacity management

The Kubernetes SIG Autoscaling has standardized a CapacityBuffer API (`autoscaling.x-k8s.io/v1alpha1`) to declare spare capacity/headroom in clusters. Cluster Autoscaler implemented this API in version 1.34.0 (September 2024), providing a vendor-agnostic way to express capacity requirements.

## User Experience

Users want to declaratively specify buffer capacity that Karpenter should maintain, similar to how they specify pod requirements. The experience should be:

```yaml
# User creates a CapacityBuffer resource
apiVersion: autoscaling.x-k8s.io/v1alpha1
kind: CapacityBuffer
metadata:
  name: web-app-buffer
  namespace: production
spec:
  # Reference existing workload to derive capacity requirements
  scalableRef:
    apiGroup: apps
    kind: Deployment
    name: web-app
  replicas: 5  # Maintain capacity for 5 additional replicas
```

**Expected behavior:**
- Karpenter provisions and maintains capacity as if 5 additional web-app pods were scheduled
- New pods from the web-app deployment can be scheduled immediately on this pre-provisioned capacity
- Buffer capacity is preserved during consolidation (nodes aren't removed if they're providing buffer)
- When real pods consume the buffer, Karpenter provisions additional capacity to restore it

**This is similar to existing Karpenter features:**
- Like StaticNodePools maintain fixed node counts, CapacityBuffers maintain fixed spare capacity
- Like pod requirements drive provisioning decisions, buffer requirements also influence scheduling
- Like disruption budgets control disruption, buffers participate in consolidation decisions

## Proposal

Extend Karpenter to support the standard CapacityBuffer API by treating buffer capacity as virtual pods that participate in scheduling and disruption decisions.

**Goal:** Karpenter respects configured CapacityBuffers, maintaining additional capacity as if they were pods scheduled in the cluster.

### API Specification

Karpenter will support the standard CapacityBuffer API:

```yaml
# Option 1: Reference existing workload
apiVersion: autoscaling.x-k8s.io/v1alpha1
kind: CapacityBuffer
metadata:
  name: web-app-buffer
  namespace: production
spec:
  scalableRef:
    apiGroup: apps
    kind: Deployment
    name: web-app
  replicas: 5  # Buffer for 5 additional replicas
---
# Option 2: Reference pod template  
apiVersion: autoscaling.x-k8s.io/v1alpha1
kind: CapacityBuffer
metadata:
  name: template-buffer
  namespace: production
spec:
  podTemplateRef:
    name: buffer-template
  replicas: 3
---
# Option 3: Percentage-based buffer
apiVersion: autoscaling.x-k8s.io/v1alpha1
kind: CapacityBuffer
metadata:
  name: database-buffer
  namespace: production
spec:
  scalableRef:
    apiGroup: apps
    kind: StatefulSet
    name: database
  percentage: 20  # 20% additional capacity
```

**Supported Reference Types**:
- `apps/v1/Deployment`
- `apps/v1/ReplicaSet` 
- `apps/v1/StatefulSet`
- `batch/v1/Job`
- `core/v1/ReplicationController`
- `core/v1/PodTemplate` (via PodTemplateRef)
- Any resource with scale subresource (via ScaleObjectPodResolver)

**Important Validation Rules** (from implementation):
1. **XOR constraint**: Exactly one of `podTemplateRef` or `scalableRef` must be specified
   - Validation: `!(has(self.podTemplateRef) && has(self.scalableRef))`
2. **PodTemplateRef requirement**: If `podTemplateRef` is set, either `replicas` or `limits` must also be set
   - Validation: `!has(self.podTemplateRef) || has(self.replicas) || has(self.limits)`
3. **Field types**: `replicas` and `percentage` are `int32` (not `int`)
4. **Minimum values**: Both `replicas` and `percentage` have `+kubebuilder:validation:Minimum=0`

### Virtual Pod Generation Workflows

The buffer controller generates virtual pods in response to these events:

**1. Initial CapacityBuffer Creation**
- User creates CapacityBuffer resource
- Controller validates and translates spec
- Generates initial set of virtual pods
- Updates status to ReadyForProvisioning

**2. CapacityBuffer Spec Update**
- User modifies replicas, percentage, or references
- Controller detects spec change
- Regenerates virtual pods with new configuration
- Updates status with new replica count

**3. Referenced Workload Scale (for percentage-based buffers)**
- Referenced deployment/statefulset scales
- Controller detects scale event
- Recalculates buffer size based on new percentage
- Regenerates virtual pods to match

**4. PodTemplate Generation Change**
- Referenced workload updates pod template
- Controller detects template change
- Regenerates virtual pods with new template
- Updates podTemplateGeneration in status

**Virtual Pod Properties:**
```yaml
metadata:
  labels:
    karpenter.sh/capacity-type: buffer
    autoscaling.x-k8s.io/buffer: <buffer-name>
    autoscaling.x-k8s.io/buffer-namespace: <buffer-namespace>
spec:
  # Derived from scalableRef or podTemplateRef
  # No priority modification - Karpenter provisions for all pods equally
  # kube-scheduler handles actual pod placement based on priority
```

## Design Decisions

### Replica Calculation Strategy

When both `replicas` and `percentage` are specified in a CapacityBuffer, we must decide which value to use for buffer capacity.

**Option A: Use minimum of both values (Cluster Autoscaler approach)**
```yaml
spec:
  replicas: 10      # Absolute cap
  percentage: 20    # 20% of current deployment
# Result: min(10, ceiling(20% of deployment replicas))
```

**Pros**:
- Prevents unexpected cost from over-provisioning
- `percentage` acts as dynamic scaling, `replicas` as hard cap
- Compatible with Cluster Autoscaler behavior for seamless migration
- Intuitive cost control: "scale with workload, but never exceed N replicas"

**Cons**:
- May not meet minimum capacity expectations in some scenarios
- Requires users to understand min() semantics

**Option B: Use maximum of both values**
```yaml
# Result: max(10, ceiling(20% of deployment replicas))
```

**Pros**:
- Ensures minimum buffer capacity is always maintained
- More intuitive for "at least N replicas" use cases

**Cons**:
- Can lead to unexpected cost when percentage exceeds replicas
- Diverges from Cluster Autoscaler, complicating migration
- Less predictable cost control

**Decision**: ✅ **Option A (minimum)**

**Rationale**:
1. **Compatibility**: Matches Cluster Autoscaler's implementation, enabling users to migrate between autoscalers without configuration changes
2. **Cost Control**: Prevents over-provisioning surprises. Users can set `replicas` as a budget limit while allowing `percentage` to scale dynamically
3. **Common Use Case**: "Scale buffer with workload (percentage), but cap at maximum cost (replicas)" is more common than "ensure at least N replicas regardless of workload size"

**Example Scenario**:
```yaml
# Production deployment with 50 replicas
# Buffer: 20% capacity with 10 replica maximum
spec:
  scalableRef:
    kind: Deployment
    name: web-app
  percentage: 20  # 20% of 50 = 10 pods
  replicas: 10    # Cap at 10 pods
# Result: min(10, 10) = 10 buffer pods

# During scale-up to 100 replicas
# percentage: 20% of 100 = 20 pods
# replicas: still 10 pods
# Result: min(20, 10) = 10 buffer pods (cost protected)
```

### Percentage Calculation Precision

**Decision**: ✅ **Use integer division (floor), no rounding**

**Rationale**:
- Matches Cluster Autoscaler behavior: `int32(percentage * scalableReplicas / 100)`
- Simpler implementation with predictable behavior
- Users can adjust percentage if exact numbers are critical

**Example**:
```yaml
# Deployment with 15 replicas, 10% buffer
# Calculation: 10 * 15 / 100 = 1.5 → 1 (floor)
# Result: 1 buffer pod
```

### Cluster Autoscaler Compatibility

**Goal**: Maintain full API compatibility to enable seamless migration between Karpenter and Cluster Autoscaler.

**Design Principles**:
1. Use identical CRD (`autoscaling.x-k8s.io/v1alpha1`)
2. Support same validation rules and constraints
3. Match behavior for replica calculations
4. Preserve status field semantics

**Benefits**:
- Users can switch autoscalers without manifest changes
- Hybrid clusters can use both autoscalers
- Knowledge transfer between ecosystems
- Shared tooling and documentation

## Implementation Details

### Behavior Model

Karpenter maintains buffer capacity using virtual pods that participate in scheduling decisions:

**Virtual Pod Lifecycle:**
1. **Creation**: When a CapacityBuffer is created, generate virtual pods representing the buffer
2. **Continuous Maintenance**: As real pods are scheduled, maintain the buffer by keeping virtual pods in the scheduling pipeline
3. **Updates**: When CapacityBuffer spec changes (replicas, percentage), regenerate virtual pods
4. **Deletion**: When CapacityBuffer is deleted, remove virtual pods from consideration

**Key Behaviors:**
- Virtual pods are derived from `scalableRef` (existing workload) or `podTemplateRef` (custom template)
- Virtual pods include labels for identification: `karpenter.sh/capacity-type: buffer`
- Virtual pods participate in provisioning (trigger NodeClaim creation) and consolidation (prevent capacity removal)
- Buffer capacity is maintained continuously - Karpenter provisions additional capacity when real pods consume buffer capacity

**Example Flow:**
```
1. User creates CapacityBuffer with replicas: 5
2. Karpenter generates 5 virtual buffer pods
3. Provisioner includes these pods in scheduling → creates NodeClaims
4. Real pods are scheduled on this capacity
5. Virtual buffer pods remain in scheduling queue → trigger more provisioning
6. Buffer is maintained as long as CapacityBuffer exists
```

### Controller Architecture

#### Buffer Controller (New)
**Location**: `pkg/controllers/capacitybuffer/`

**Controller API:**
```go
type BufferController interface {
    // GetVirtualPods returns all active buffer pods for scheduling
    GetVirtualPods(ctx context.Context) ([]*corev1.Pod, error)
    
    // GetDisruptionCost returns cost for disrupting capacity on a node
    // Buffer virtual pods have lower disruption cost than real workloads
    GetDisruptionCost(ctx context.Context, node *corev1.Node) (float64, error)
    
    // Run starts the reconciliation loop
    Run(ctx context.Context) error
}
```

**Responsibilities:**
- Watch CapacityBuffer CRDs and translate them into virtual pods
- Maintain virtual pods in memory for scheduling integration
- Handle buffer lifecycle and status updates
- Provide virtual pods to provisioner and disruption controllers

**Internal Components**:
```go
type bufferController struct {
    kubeClient      client.Client
    bufferClient    *capacitybuffer.Client
    
    // Buffer-specific components
    bufferCache     map[types.NamespacedName]*BufferState
    translator      *BufferTranslator
    statusUpdater   *StatusUpdater
    filters         []Filter
}

type BufferState struct {
    Buffer          *v1alpha1.CapacityBuffer
    VirtualPods     []*corev1.Pod
    LastUpdate      time.Time
}

type BufferTranslator struct {
    // Combines multiple translators:
    // - PodTemplateTranslator for podTemplateRef
    // - ScalableObjectsTranslator for scalableRef
}
```

#### Scheduling Integration
**Location**: `pkg/controllers/provisioning/`

**Correct Integration Approach**:

**Option 1: Provisioner-Level Integration (Recommended)**
```go
// In provisioning/provisioner.go
func (p *Provisioner) Schedule(ctx context.Context) (scheduler.Results, error) {
    // 1. Get pending pods (existing logic)
    userPods := p.GetPendingPods(ctx)
    
    // 2. Get buffer pods for scheduling consideration
    bufferPods := p.bufferController.GetActiveBufferPods(ctx)
    
    // 3. Combine with priority handling
    allPods := p.prioritizeAndCombinePods(userPods, bufferPods)
    
    // 4. Use existing scheduler (no changes to core logic)
    scheduler := scheduling.NewScheduler(p.kubeClient, nodeClaimTemplates, ...)
    results, err := scheduler.Solve(ctx, allPods)
    
    // 5. Track which NodeClaims are accommodating buffer pods
    p.bufferController.UpdateProvisionedCapacity(ctx, results.NewNodeClaims, bufferPods)
    
    return results, err
}

func (p *Provisioner) prioritizeAndCombinePods(userPods, bufferPods []*v1.Pod) []*v1.Pod {
    // Set lower priority for buffer pods
    for _, pod := range bufferPods {
        if pod.Spec.Priority == nil {
            priority := int32(-1000)
            pod.Spec.Priority = &priority
        }
    }
    return append(userPods, bufferPods...)
}
```

**Option 2: Scheduler-Level Integration (Alternative)**
```go
// In scheduling/scheduler.go - modify Solve method
func (s *Scheduler) Solve(ctx context.Context, pods []*corev1.Pod) (Results, error) {
    // Existing logic with buffer awareness
    // Buffer pods participate in topology constraints and affinity calculations
    // But are treated as "evictable" during constraint solving
}
```

#### Disruption Integration  
**Location**: `pkg/controllers/disruption/`

**Integration Approach:**

Treat virtual buffer pods like regular pods during consolidation simulation. This is simpler than creating special buffer-specific logic.

```go
// In disruption/consolidation.go
func (c *consolidation) computeConsolidation(ctx context.Context, candidates ...*Candidate) (Command, error) {
    // Get all pods: real + virtual buffer pods
    pods := append(
        c.getRealPods(candidates),
        c.bufferController.GetVirtualPods(ctx)...,
    )
    
    // Simulate scheduling with all pods (no distinction needed)
    results, err := c.simulateScheduling(ctx, pods, candidates...)
    if err != nil {
        return Command{}, err
    }
    
    // If consolidation removes capacity that buffer pods need, it will fail simulation
    // No special buffer validation needed
    return c.buildConsolidationCommand(results), nil
}
```

**Disruption Cost:**

Virtual buffer pods should have lower disruption cost than real workloads:

```go
// Buffer controller provides disruption cost
func (bc *BufferController) GetDisruptionCost(ctx context.Context, node *corev1.Node) (float64, error) {
    // Check if node hosts virtual buffer pods
    bufferPodCount := bc.getBufferPodCountOnNode(node)
    if bufferPodCount == 0 {
        return 0, nil
    }
    
    // Buffer pods have minimal disruption cost (they don't actually exist)
    // This allows consolidation to prefer moving buffer capacity over real workloads
    return float64(bufferPodCount) * 0.01, nil
}
```

**Key Behaviors:**
- Virtual buffer pods don't need eviction (they don't actually exist)
- Virtual buffer pods can be removed from consideration during consolidation
- Consolidation simulation includes virtual pods - if they can't fit after consolidation, it's invalid
- Buffer pods have lower disruption cost, allowing consolidation to prefer disrupting buffer capacity

### API Integration

Karpenter will support the standard CapacityBuffer API from SIG Autoscaling:

```yaml
apiVersion: autoscaling.x-k8s.io/v1alpha1
kind: CapacityBuffer
metadata:
  name: my-app-buffer
  namespace: default
spec:
  # Provisioning strategy (defaults to "buffer.x-k8s.io/active-capacity" if not specified)
  provisioningStrategy: "buffer.x-k8s.io/active-capacity"
  
  # Option 1: Reference existing workload
  scalableRef:
    apiGroup: apps
    kind: Deployment
    name: my-app
  replicas: 5  # Type: int32, minimum: 0
  
  # Option 2: Reference pod template
  # podTemplateRef:
  #   name: my-template
  # replicas: 3
  
  # Option 3: Percentage-based buffer  
  # percentage: 10  # 10% of referenced workload (int32, minimum: 0)
  #
  # If both 'replicas' and 'percentage' are set, min() is used as a cap
  # See Design Decisions section for rationale
  
  # Option 4: Resource limits
  # limits:
  #   cpu: "8"
  #   memory: "16Gi"
  #   # Will create as many chunks as fit within these limits
    
status:
  conditions:
    - type: ReadyForProvisioning
      status: "True"
      reason: AttributesSetSuccessfully
      message: "Buffer is ready for provisioning"
      lastTransitionTime: "2025-12-18T00:00:00Z"
    - type: Provisioning
      status: "True" 
      reason: CapacityProvisioned
      message: "Buffer capacity has been provisioned"
  replicas: 5
  podTemplateRef:
    name: translated-template
  podTemplateGeneration: 1
  provisioningStrategy: "buffer.x-k8s.io/active-capacity"
```

### Buffer Translation and Reconciliation

Following Cluster Autoscaler's pattern:
1. **Controller watches CapacityBuffer CRDs**
2. **Filter by provisioning strategy** (default: `buffer.x-k8s.io/active-capacity`)
3. **Translate buffer specs**:
   - **ScalableRef resolution**: 
     - Query the referenced workload (Deployment, StatefulSet, etc.)
     - Extract pod template from the workload
     - Support scale subresource for custom resources
   - **PodTemplateRef resolution**: Use referenced template directly
   - Calculate replica count (absolute, percentage, or resource-limited) per Design Decisions
4. **Generate virtual pods** with buffer-specific labels
5. **Update buffer status** with translation results
6. **Inject virtual pods** into Karpenter's scheduling pipeline

### Implementation Phases

#### Prerequisites

Before implementation can begin:

1. **CapacityBuffer CRD Availability**
   - CRD is available in kubernetes/autoscaler repository (as of 1.34.0)
   - Can be installed separately or via Helm
   - Karpenter will not bundle the CRD by default (alpha feature)

2. **Feature Gate Infrastructure**
   - Feature gate mechanism must support `CapacityBuffer` flag
   - CRD installation documentation must be prepared

3. **Reference Implementation Available**
   - Cluster Autoscaler 1.34.0+ provides reference implementation
   - Can validate API compatibility and behavior

#### Alpha Release Scope

The initial alpha release must include:

**Core Functionality:**
- Support for `scalableRef` (Deployment, StatefulSet, ReplicaSet)
- Support for `podTemplateRef`
- Basic replica and percentage calculation
- Virtual pod generation and injection into scheduling
- Feature gate implementation (`--feature-gates=CapacityBuffer=true`)

**API Support:**
- CapacityBuffer CRD watching
- Status updates (ReadyForProvisioning condition)
- Filter by provisioning strategy (`buffer.x-k8s.io/active-capacity`)

**Integration:**
- Virtual pods participate in provisioner scheduling
- Basic consolidation protection (treat as real pods)
- Respect NodePool limits

**Excluded from Alpha:**
- Resource limits-based sizing (Option 4)
- Advanced consolidation strategies
- Cross-namespace references
- Custom resource support beyond built-in types
- Detailed metrics and observability

#### Phase 1: Foundation (Week 1-2)
- Add CapacityBuffer CRD client and watching
- Implement feature gate
- Implement buffer translators:
  - PodTemplateTranslator
  - ScalableObjectsTranslator (Deployment, StatefulSet, ReplicaSet)
- Implement filtering logic (strategy, status)
- Unit tests for translation logic

#### Phase 2: Virtual Pod Generation (Week 3-4)
- Build BufferController with reconciliation loop
- Implement virtual pod generation with buffer labels
- Implement status updater for buffer conditions
- Integration tests with CRD lifecycle

#### Phase 3: Scheduling Integration (Week 5-6)
- Modify provisioner.Schedule() to include buffer pods
- Implement priority-based pod combination logic
- Ensure buffer virtual pods trigger provisioning correctly
- E2E tests for provisioning scenarios

#### Phase 4: Consolidation Integration (Week 7-8)
- Treat buffer pods as real pods during consolidation simulation
- Validate consolidation doesn't remove buffer capacity
- E2E tests for consolidation scenarios

#### Phase 5: Alpha Release Polish (Week 9-10)
- Basic metrics (buffer count, virtual pod count)
- User documentation with installation instructions
- Alpha limitations documented
- Performance baseline testing

## Alternatives Considered

### Alternative 1: NodePool-specific Buffer Configuration
**Approach**: Add buffer fields directly to NodePool CRD
```yaml
apiVersion: karpenter.sh/v1beta1
kind: NodePool
spec:
  buffer:
    replicas: 5
```

**👍 Benefits**:
- Simpler implementation (no new controller)
- Tighter integration with NodePool

**👎 Drawbacks**:
- Not compatible with standard Kubernetes Buffer API
- Breaks compatibility with Cluster Autoscaler
- Users need different configurations for different autoscalers
- Not aligned with Kubernetes ecosystem

**Decision**: ❌ Rejected - Standardization is more valuable

### Alternative 2: Separate Buffer Pods (Pause Containers)
**Approach**: Create actual pause container pods instead of virtual pods

**👍 Benefits**:
- More "real" - actual pods exist in cluster
- Easier debugging (can see pods with kubectl)

**👎 Drawbacks**:
- Additional overhead (actual pods, scheduler load)
- Slower than virtual pods
- Cluster Autoscaler uses virtual pods (consistency)
- Unnecessary resource consumption

**Decision**: ❌ Rejected - Virtual pods are more efficient

## Security & Permissions

### RBAC Requirements
Buffer Controller will need:
```yaml
apiVersion: rbac.authorization.k8s.io/v1
kind: ClusterRole
metadata:
  name: karpenter-buffer-controller
rules:
- apiGroups: ["autoscaling.x-k8s.io"]
  resources: ["capacitybuffers"]
  verbs: ["get", "list", "watch", "update", "patch"]  # Update for status
- apiGroups: ["apps"]
  resources: ["deployments", "replicasets", "statefulsets"]
  verbs: ["get", "list"]  # For ScalableRef resolution
- apiGroups: [""]
  resources: ["pods", "podtemplates"]
  verbs: ["get", "list"]  # For PodTemplateRef resolution
- apiGroups: ["*"]
  resources: ["*/scale"]  # For scale subresource access
  verbs: ["get"]
```

### Cross-Namespace Considerations
Following the Buffer API proposal:
- PodTemplateRef: Same namespace only (initially)
- ScalableRef: Same namespace only (initially)
- Future: Consider ReferenceGrant for cross-namespace

## Testing Plan

### Unit Tests
- Buffer CRD parsing
- Virtual pod generation
- ScalableRef resolution
- PodTemplateRef resolution

### Integration Tests
- Buffer controller watches CRDs correctly
- Virtual pods are injected into scheduling
- Provisioning includes buffer capacity
- Consolidation respects buffer capacity

### E2E Tests
- Create CapacityBuffer → Nodes provisioned
- Delete CapacityBuffer → Nodes deprovisioned (if unused)
- User pods schedule immediately on buffer capacity
- Consolidation does not remove buffer nodes

### Performance Tests
- Impact on scheduling latency
- Memory overhead of virtual pods
- Watch performance with many buffers

## Feature Gating

Capacity Buffer support will be introduced as an **alpha feature** with appropriate protections.

### Feature Flag

- **Flag Name:** `CapacityBuffer`
- **Default State:** `disabled`
- **Opt-in:** Users must explicitly enable via `--feature-gates=CapacityBuffer=true`

### Alpha Protections

Following Karpenter's standard alpha feature practices:

1. **Disabled by Default**: Feature must be explicitly enabled
2. **No SLA**: No guarantees on behavior, performance, or stability
3. **May Have Bugs**: Alpha features may contain issues that could affect cluster stability
4. **API Changes**: The integration approach may change in future releases
5. **CRD Dependency**: Requires CapacityBuffer CRD to be installed in the cluster

### CRD Installation

The CapacityBuffer CRD (`autoscaling.x-k8s.io/v1alpha1`) must be installed separately:
- Not bundled with Karpenter installation by default
- Users must install from kubernetes/autoscaler repository or via Helm
- Karpenter will gracefully handle missing CRD (log warning, skip buffer processing)

### Graduation Criteria

**Beta Requirements:**
- Feature has been enabled in production environments for at least 2 releases
- Comprehensive e2e tests covering common scenarios
- Performance impact quantified and documented
- At least 3 external adopters providing feedback
- Documentation complete with examples and troubleshooting

**Stable Requirements:**
- Feature has been in beta for at least 2 releases
- No major bugs reported for 1 release cycle
- Performance acceptable (< 5% overhead on scheduling latency)
- API compatibility verified with Cluster Autoscaler
- Migration path documented for users switching between autoscalers

## Migration & Compatibility

### Existing Users
- ✅ No breaking changes
- ✅ Opt-in feature (requires creating CapacityBuffer)
- ✅ Works alongside existing NodePools

### Cluster Autoscaler Compatibility
- ✅ Uses same CRD (autoscaling.x-k8s.io)
- ✅ Can migrate between autoscalers without config changes

## Documentation Plan

### User Documentation
- Concepts: What is Buffer/Headroom
- Getting Started: Simple example
- Advanced: ScalableRef vs PodTemplateRef
- Best Practices: When to use buffers

### Examples
```
examples/buffer/
├── simple-buffer.yaml
├── deployment-based-buffer.yaml
├── percentage-buffer.yaml
└── multi-buffer.yaml
```

## Open Questions & Karpenter-Specific Considerations

1. **Q**: Should we support buffer priorities?
   **A**: Defer to future enhancement - not in initial scope

2. **Q**: How to handle ScalableRef when no pods exist yet?
   **A**: Cluster Autoscaler requires at least one existing pod for ScaleObjectPodResolver. For Karpenter, we should:
   - Support the same requirement initially for compatibility
   - For custom CRDs without existing pods, require PodTemplateRef
   - Consider future enhancement to support pod template discovery from CRD

3. **Q**: How to handle buffer with no matching nodes?
   **A**: Follow Karpenter's provisioning behavior - virtual buffer pods participate in scheduling, which creates NodeClaims as needed

5. **Q**: How does buffer capacity interact with NodePool limits?
   **A**: Virtual buffer pods participate in scheduling like real pods, so NodeClaims created to accommodate them must respect NodePool resource limits and budget constraints

6. **Q**: Integration with Karpenter's drift detection?
   **A**: NodeClaims that accommodate buffer pods participate in drift detection like any other NodeClaim - when drifted, they trigger replacement through normal disruption flow

7. **Q**: Handling of interruption events for buffer nodes?
   **A**: Buffer capacity on interrupted nodes should be re-provisioned immediately to maintain buffer requirements

8. **Q**: Metrics/observability for buffers?
   **A**: Add Karpenter-specific metrics:
   - `karpenter_buffer_nodeclaims_total`
   - `karpenter_buffer_capacity_cpu_cores`
   - `karpenter_buffer_capacity_memory_bytes`
   - `karpenter_buffer_consolidation_blocked_total`

9. **Q**: State persistence across Karpenter restarts?
   **A**: Buffer state must be reconstructable from CapacityBuffer CRDs and current cluster state (NodeClaims, Nodes)

## References

- Buffer API Proposal: https://github.com/kubernetes/autoscaler/blob/master/cluster-autoscaler/proposals/buffers.md
- Cluster Autoscaler Implementation: https://github.com/kubernetes/autoscaler/tree/master/cluster-autoscaler/capacitybuffer
- CapacityBuffer CRD: https://github.com/kubernetes/autoscaler/tree/master/cluster-autoscaler/apis/capacitybuffer
- Related Issues: kubernetes-sigs/karpenter#2571
- Community Requests: #749, #987, #3240, #3384, #4409
