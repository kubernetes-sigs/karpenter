# Capacity Buffer API Support

## Background

Karpenter currently operates as a dynamic cluster autoscaler, provisioning nodes just-in-time based on pending pod demand. However, several important use cases require maintaining pre-provisioned spare capacity:

1. **Performance-critical applications** where just-in-time provisioning latency is unacceptable
2. **Burst workloads** that need immediate scheduling for CI/CD, batch jobs, or event-driven applications
3. **High-availability services** that require buffer capacity to handle traffic spikes or node failures
4. **Consistent user experience** across different autoscaling solutions in the Kubernetes ecosystem

Currently, users attempt to achieve buffer capacity through workarounds:
- Deploying "balloon pods" or placeholder workloads to maintain minimum capacity
- Using pause containers with resource requests to reserve capacity
- Over-provisioning through static NodePools

The Kubernetes SIG Autoscaling has standardized a CapacityBuffer API to declare spare capacity/headroom in clusters. Cluster Autoscaler supports this API (autoscaling.x-k8s.io/v1alpha1), providing a vendor-agnostic way to express capacity requirements.

## Proposal

Extend Karpenter to support the standard CapacityBuffer API (autoscaling.x-k8s.io/v1alpha1) by integrating buffer capacity into scheduling and consolidation algorithms.

```yaml
apiVersion: autoscaling.x-k8s.io/v1alpha1
kind: CapacityBuffer
metadata:
  name: my-app-buffer
  namespace: default
spec:
  # Reference existing workload to derive capacity
  scalableRef:
    apiGroup: apps
    kind: Deployment
    name: my-app
  replicas: 5  # Maintain buffer for 5 additional replicas
```

Key aspects:
1. **Virtual Pod Approach**: Follow Cluster Autoscaler's pattern using in-memory virtual pods
2. **Scheduling Integration**: Buffer pods participate in Karpenter's bin-packing algorithms
3. **Consolidation Protection**: Ensure buffer capacity is preserved during node optimization
4. **Standard Compatibility**: Use the same CRD as Cluster Autoscaler for ecosystem consistency

### Modeling & Validation

When a `CapacityBuffer` is created, Karpenter generates virtual pods that participate in scheduling decisions but are never actually created in the cluster. These virtual pods:

- Are derived from either `scalableRef` (existing workload) or `podTemplateRef` (custom template)
- Include special labels for identification: `karpenter.sh/capacity-type: buffer`
- Have lower priority than real workloads to ensure user pods take precedence
- Are considered during both provisioning and consolidation decisions

Buffer capacity persists until:
- The CapacityBuffer resource is deleted
- Real workloads consume the buffered capacity
- Consolidation occurs that preserves equivalent buffer capacity elsewhere

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
- `core/v1/PodTemplate` (via PodTemplateRef)
- Any resource with scale subresource

### Virtual Pod Generation

Following Cluster Autoscaler's pattern:

1. **ScalableRef Resolution**: Query the referenced workload's pod template
2. **PodTemplateRef Resolution**: Directly use the referenced template
3. **Replica Calculation**: Compute absolute replicas from relative percentages
4. **Virtual Pod Creation**: Generate N in-memory pods with special labels:
   ```yaml
   labels:
     karpenter.sh/capacity-type: buffer
     autoscaling.x-k8s.io/buffer: <buffer-name>
     autoscaling.x-k8s.io/buffer-namespace: <buffer-namespace>
   ```
5. **Priority Assignment**: Use system priority class or lowest priority to ensure user pods take precedence

## Implementation Details

### Controller Architecture

We will implement capacity buffer support by adding a new controller and integrating with existing Karpenter components. 

**⚠️ Critical Integration Considerations**: This implementation must account for Karpenter's unique architecture that operates at the NodeClaim level rather than individual pod scheduling, and integrates deeply with Karpenter's state management system.

#### Buffer Controller (New)
**Location**: `pkg/controllers/capacitybuffer/`

**Purpose**: 
- Watch CapacityBuffer CRDs and translate them into virtual pods
- Maintain an in-memory cache of buffer pods for scheduling integration
- Handle buffer lifecycle and status updates

**Key Components**:
```go
type Controller struct {
    kubeClient      client.Client
    cluster         *state.Cluster        // Karpenter's cluster state management
    provisioner     *provisioning.Provisioner
    
    // Buffer-specific components
    bufferCache     map[types.NamespacedName]*BufferState
    translator      *BufferTranslator
    statusUpdater   *StatusUpdater
    filters         []Filter
}

type BufferState struct {
    Buffer          *v1alpha1.CapacityBuffer
    VirtualPods     []*corev1.Pod
    NodeClaims      []*scheduler.NodeClaim  // Track at NodeClaim level
    LastScheduled   time.Time
}

type BufferTranslator struct {
    // Combines multiple translators:
    // - PodTemplateTranslator for podTemplateRef
    // - ScalableObjectsTranslator for scalableRef
    // - ResourceLimitsTranslator for limits-based sizing
}
```

#### Scheduling Integration
**Location**: `pkg/controllers/provisioning/`

**Critical Architecture Consideration**: 
Karpenter's scheduling operates at the `NodeClaim` level, not individual pods. The `provisioner.Schedule()` method creates `NodeClaim` objects through complex constraint solving.

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
    
    // 5. Track buffer NodeClaims for consolidation
    p.bufferController.UpdateBufferNodeClaims(ctx, results.NewNodeClaims)
    
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

**Behavior**:
- Buffer pods can be evicted during consolidation (they're virtual and easily recreated)
- However, consolidation simulation must ensure equivalent buffer capacity remains available
- Include buffer pods in scheduling simulation when evaluating consolidation candidates

**Critical Consolidation Challenges**:

1. **Multi-Strategy Disruption**: Karpenter executes multiple disruption strategies sequentially (Emptiness, Drift, Multi-Node Consolidation, Single-Node Consolidation)

2. **Budget Mapping**: Complex disruption budget calculations across node families

3. **Parallel Execution**: Disruption commands are executed in parallel with careful orchestration

**Revised Integration Approach**:

```go
// In disruption/consolidation.go
func (c *consolidation) computeConsolidation(ctx context.Context, candidates ...*Candidate) (Command, error) {
    // 1. Get buffer requirements for affected nodes
    bufferRequirements := c.bufferController.GetBufferRequirementsForNodes(ctx, candidates)
    
    // 2. Include buffer pods in simulation
    realPods := c.getRealPods(candidates)
    bufferPods := c.bufferController.GetBufferPodsForNodes(ctx, candidates)
    
    // 3. Simulate with buffer capacity preservation
    results, err := c.simulateSchedulingWithBuffers(ctx, realPods, bufferPods, candidates...)
    if err != nil {
        return Command{}, err
    }
    
    // 4. Validate buffer capacity is maintained
    if !c.validateBufferCapacityPreservation(results, bufferRequirements) {
        return Command{}, fmt.Errorf("consolidation would violate buffer capacity requirements")
    }
    
    return c.buildConsolidationCommand(results), nil
}

// New method: Buffer-aware scheduling simulation
func (c *consolidation) simulateSchedulingWithBuffers(
    ctx context.Context, 
    realPods, bufferPods []*corev1.Pod, 
    candidates ...*Candidate,
) (*SimulationResults, error) {
    // Create temporary scheduler with buffer awareness
    scheduler := scheduling.NewScheduler(
        c.kubeClient, 
        c.nodeClaimTemplates,
        scheduling.WithBufferHandling(),  // New option
    )
    
    // Simulate scheduling with combined pods
    allPods := append(realPods, bufferPods...)
    return scheduler.SimulateScheduling(ctx, allPods, candidates...)
}

// Buffer capacity validation
func (c *consolidation) validateBufferCapacityPreservation(
    results *SimulationResults, 
    requirements *BufferRequirements,
) bool {
    // Ensure equivalent buffer capacity is available after consolidation
    return c.bufferController.ValidateCapacityPreservation(results, requirements)
}
```

**State Management Integration**:
```go
// Integration with state.Cluster
func (bc *BufferController) syncWithClusterState(ctx context.Context) error {
    // 1. Get current cluster state
    nodes := bc.cluster.Nodes()
    nodeClaims := bc.cluster.NodeClaims()
    
    // 2. Reconcile buffer allocations with actual cluster state
    for bufferName, bufferState := range bc.bufferCache {
        // Check if buffer NodeClaims still exist
        activeNodeClaims := bc.filterActiveNodeClaims(bufferState.NodeClaims, nodeClaims)
        
        // Update buffer state based on cluster reality
        bufferState.NodeClaims = activeNodeClaims
        
        // Trigger re-provisioning if buffer capacity is insufficient
        if bc.isBufferCapacityInsufficient(bufferState) {
            bc.requestBufferProvisioning(ctx, bufferState)
        }
    }
    
    return nil
}
```

**Revised Protection Strategy**:

1. **NodeClaim-Level Tracking**: Buffer capacity is tracked at the NodeClaim level, not just pod level

2. **State Synchronization**: Buffer controller maintains consistency with Karpenter's `state.Cluster`

3. **Disruption Budget Integration**: Buffer requirements participate in Karpenter's disruption budget calculations

4. **Capacity Preservation Validation**: Before any consolidation, validate that equivalent buffer capacity will remain available

5. **Graceful Degradation**: If buffer capacity cannot be maintained, prioritize user workloads and log buffer capacity warnings

### API Integration

Karpenter will support the standard CapacityBuffer API from SIG Autoscaling:

```yaml
apiVersion: autoscaling.x-k8s.io/v1alpha1
kind: CapacityBuffer
metadata:
  name: my-app-buffer
  namespace: default
spec:
  # Default provisioning strategy
  provisioningStrategy: "buffer.x-k8s.io/active-capacity"
  
  # Option 1: Reference existing workload
  scalableRef:
    apiGroup: apps
    kind: Deployment
    name: my-app
  replicas: 5
  
  # Option 2: Reference pod template
  # podTemplateRef:
  #   name: my-template
  # replicas: 3
  
  # Option 3: Percentage-based buffer  
  # percentage: 10  # 10% of referenced workload
  
  # Option 4: Resource limits
  # limits:
  #   cpu: "8"
  #   memory: "16Gi"
    
status:
  conditions:
    - type: ReadyForProvisioning
      status: "True"
      reason: BufferTranslated
    - type: Provisioning
      status: "True" 
      reason: CapacityProvisioned
  replicas: 5
  podTemplateRef:
    name: translated-template
  podTemplateGeneration: 1
```

**Supported Reference Types**:
- `apps/v1/Deployment`
- `apps/v1/ReplicaSet` 
- `apps/v1/StatefulSet`
- `core/v1/PodTemplate` (via PodTemplateRef)
- Future: Custom scalable resources with scale subresource

### Virtual Pod Generation

Following Cluster Autoscaler's pattern:
1. **Controller watches CapacityBuffer CRDs**
2. **Filter by provisioning strategy** (buffer.x-k8s.io/active-capacity)
3. **Translate buffer specs**:
   - ScalableRef → query target workload's pod template
   - PodTemplateRef → use referenced template directly
   - Calculate replica count (absolute, percentage, or resource-limited)
4. **Generate virtual pods** with buffer-specific labels
5. **Update buffer status** with translation results
6. **Inject virtual pods** into Karpenter's scheduling pipeline

### Implementation Phases

#### Phase 1: Foundation & Architecture Analysis (Week 1-2)
- Add CapacityBuffer CRD watching and client setup
- Implement buffer translators (following cluster-autoscaler patterns):
  - PodTemplateTranslator
  - ScalableObjectsTranslator  
  - ResourceLimitsTranslator
- Implement filtering logic (strategy, status, generation)
- **Deep analysis of Karpenter's state.Cluster integration points**
- Unit tests for translation logic

#### Phase 2: State Management Integration (Week 3-4)
- Build BufferController with reconciliation loop
- **Implement state.Cluster synchronization for buffer NodeClaims**
- Implement status updater for buffer conditions
- Virtual pod generation with NodeClaim-level tracking
- Integration tests with cluster state

#### Phase 3: Provisioning Integration (Week 5-7)
- **Modify provisioner.Schedule() to handle buffer pods correctly**
- Implement priority-based pod combination logic
- **Integration with scheduling.Scheduler constraint solving**
- Ensure buffer capacity triggers NodeClaim creation
- **Handle topology spread constraints and affinity with buffer pods**
- E2E tests for provisioning scenarios

#### Phase 4: Disruption Integration (Week 8-10)
- **Analyze all disruption strategies (Emptiness, Drift, Consolidation)**
- Implement buffer-aware consolidation simulation
- **Integrate with disruption budget calculations**
- Add buffer capacity preservation validation
- **Handle parallel disruption command execution**
- E2E tests for complex consolidation scenarios

#### Phase 5: Observability & Polish (Week 11-12)
- **Add buffer-specific metrics integration with state.Cluster**
- User documentation with Karpenter-specific examples
- Performance testing with large-scale buffer scenarios
- **Validate integration with Karpenter's event system**

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
   **A**: Defer to future KEP - not in initial scope

2. **Q**: How to handle buffer with no matching nodes?
   **A**: Follow Karpenter's provisioning behavior - create suitable NodeClaims through scheduler constraint solving

3. **Q**: How does buffer capacity interact with NodePool limits?
   **A**: Buffer NodeClaims must respect NodePool resource limits and budget constraints

4. **Q**: Integration with Karpenter's drift detection?
   **A**: Buffer NodeClaims participate in drift detection - when drifted, they trigger replacement through normal disruption flow

5. **Q**: Handling of interruption events for buffer nodes?
   **A**: Buffer capacity on interrupted nodes should be re-provisioned immediately to maintain buffer requirements

6. **Q**: Metrics/observability for buffers?
   **A**: Add Karpenter-specific metrics:
   - `karpenter_buffer_nodeclaims_total`
   - `karpenter_buffer_capacity_cpu_cores`
   - `karpenter_buffer_capacity_memory_bytes`
   - `karpenter_buffer_consolidation_blocked_total`

7. **Q**: State persistence across Karpenter restarts?
   **A**: Buffer state must be reconstructable from CapacityBuffer CRDs and current cluster state (NodeClaims, Nodes)

## References

- Buffer API Proposal: https://github.com/kubernetes/autoscaler/blob/master/cluster-autoscaler/proposals/buffers.md
- Cluster Autoscaler Implementation: https://github.com/kubernetes/autoscaler/tree/master/cluster-autoscaler/capacitybuffer
- CapacityBuffer CRD: https://github.com/kubernetes/autoscaler/tree/master/cluster-autoscaler/apis/capacitybuffer
- Related Issues: kubernetes-sigs/karpenter#2571
- Community Requests: #749, #987, #3240, #3384, #4409
