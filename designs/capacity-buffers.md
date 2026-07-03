#  RFC : Capacity Buffer Support For Karpenter

# Problem

Karpenter provisions nodes just-in-time based on pending pod demand. This creates scheduling latency as pods wait for node provisioning. Users need the ability to pre-provision spare capacity so pods can schedule immediately, with the buffer dynamically maintaining capacity as workloads scale.

**Existing Workarounds:**

Users maintain spare capacity through balloon pods, pause containers, or NodePools with fixed capacity. These approaches have limitations:

- **Balloon pods**: Hard to maintain, create scheduler overhead through preemption
- **Fixed NodePools**: Operate at node-level, require manual capacity planning, no workload awareness

**What's Missing:**

A pod-level capacity abstraction where users specify workload requirements and Karpenter determines optimal node configuration automatically. This should dynamically adjust as workloads scale and integrate with the standard Kubernetes SIG Autoscaling CapacityBuffer API.

The feature has been requested multiple times by the community:
- [#749](https://github.com/kubernetes-sigs/karpenter/issues/749)
- [#987](https://github.com/aws/karpenter-provider-aws/issues/987)
- [#3240](https://github.com/aws/karpenter-provider-aws/issues/3240)
- [#3384](https://github.com/aws/karpenter-provider-aws/issues/3384)
- [#4409](https://github.com/aws/karpenter-provider-aws/issues/4409)

# Goals

- Support the Kubernetes SIG Autoscaling CapacityBuffer API (`autoscaling.x-k8s.io/v1alpha1`)
- Provide pod-level capacity abstraction where users specify pod requirements and Karpenter determines optimal node configuration
- Implement active buffer strategy: Continuously maintain spare capacity that scales with workload changes
- Maintain buffer capacity through virtual pods that participate in scheduling simulation every provisioning cycle
- Preserve buffer capacity during disruption operations (consolidation, drift, expiration)

# Non-Goals

- Optimizing scheduling simulation performance for Virtual pods (will measure and optimize if needed)
- Initial implementation will only support same-namespace references for `scalableRef` and `podTemplateRef`
- Ephemeral capacity strategy: One-time capacity request pattern for batch systems like Kueue (deferred to future work)
- Adding `expireAfter` field to the API in initial implementation (requires upstream sig-autoscaling consensus)

# Proposal

Support the Kubernetes SIG Autoscaling CapacityBuffer API (`autoscaling.x-k8s.io/v1alpha1`) with active buffer strategy. This provides:

- Standard API compatible with Cluster Autoscaler
- Pod-level abstraction (specify pod requirements, not node requirements)
- Dynamic spare capacity that scales with workload changes
- Integration with Karpenter's provisioning and disruption systems

To achieve this we will 

**Add a new Buffer Controller:**
1. Watches CapacityBuffer CRDs
2. Resolves pod template from `scalableRef` or `podTemplateRef`
3. Calculates replica count from `replicas`, `percentage`, or `limits`
4. Writes to buffer status: `replicas` + `podTemplateRef`
5. Updates status conditions

**Make changes to existing Provisioner:**
1. Reads buffer status
2. Constructs virtual pods in-memory (not actual pod objects)
3. Runs scheduling simulation: Can these virtual pods fit on existing nodes?
   - Yes → Virtual pods can be placed on existing capacity, set `Provisioning: True`
   - No → Create NodeClaims, keep `Provisioning: False` until nodes are available
4. Only sets buffer status to `Provisioning: True` when virtual pods can be successfully placed on existing cluster capacity without creating new NodeClaims

**Key Point:** Virtual pods are reconstructed every provisioning loop from buffer status. No pod objects are created. The `Provisioning: True` status reflects actual available capacity in the cluster, ensuring the status accurately represents whether buffer capacity is ready for use even if NodeClaims fail to provision.


## Provisioning Strategies

## Active Capacity Buffer

![](./images/active_buffer.png)

**Behavior:**
- Maintains dynamic spare capacity continuously
- Reacts to workload scaling and template changes
- Buffer size adjusts with deployment size when using `percentage`
- Provisioner checks buffer status every provisioning cycle
- Virtual pods reconstructed each cycle to maintain capacity
- Buffer remains active until explicitly deleted

**Use Cases:**
- Spare capacity / headroom for burst workloads
- Pre-warming capacity for predictable traffic spikes
- Reducing pod scheduling latency for critical applications

**Lifecycle:**
1. User creates buffer (provisioningStrategy defaults to `buffer.x-k8s.io/active-capacity`)
2. Buffer controller resolves template and calculates replicas
3. Every provisioning cycle, provisioner attempts to provision capacity
4. Status updated to `Provisioning: True` only when virtual pods can be placed on existing nodes (without new NodeClaims)
5. Buffer continuously maintains capacity, reacting to workload changes
6. User deletes buffer when no longer needed

## Supported References

**scalableRef:**
- `apps/v1/Deployment`
- `apps/v1/StatefulSet`
- `apps/v1/ReplicaSet`

**podTemplateRef:**
- `core/v1/PodTemplate`

### ScalableRef Pod Template Resolution

The implementation uses typed Gets against the Kubernetes API to read the workload's `spec.template.spec` directly. This gives us the full PodSpec (including init containers, sidecar containers, tolerations, affinity, etc.) without needing running pods.

**Key Design Decision:** Unlike the Cluster Autoscaler (which derives templates from running pods via the scale subresource), Karpenter reads the workload spec directly. This means:
- The buffer initializes immediately when the workload exists — no need to wait for running pods
- Template changes are picked up on the next controller requeue (30s)
- The PodSpec includes all fields the scheduler needs (tolerations, nodeSelector, affinity, resource requests)

## CapacityBuffer Status Conditions

The CapacityBuffer uses standard Kubernetes conditions to report its state:

**ReadyForProvisioning:**
- True: Pod template is successfully resolved and target replicas are calculated
- False: Missing references (ScalableRefNotFound, PodTemplateNotFound), validation errors, or calculation failures

**Provisioning:**
- True: Capacity is actually available. Virtual pods fit onto existing nodes without requiring new NodeClaims
- False: Virtual pods don't fit; new NodeClaims are required, limits prevent scaling (InsufficientCapacity), or provisioning failed

## Replica Calculation

When both `replicas` and `percentage` are specified, use minimum to match Cluster Autoscaler behavior. When only `limits` is specified then we determine the chunks that fit based on the ref.


## Provisioner Integration

**Responsibilities:**
- Read buffer status for active buffers
- Construct virtual pods in-memory from buffer status
- Combine virtual pods with pending user pods
- Pass combined pod list to simulate scheduling
- Update buffer status with provisioning state

**Virtual Pod Construction:**
- Virtual pods reconstructed every provisioning loop from buffer status
- Pods created in-memory only (NOT stored in etcd or cluster state)
- Deterministic UUIDs assigned for logging and observability
- No API server or etcd overhead

## Disruption Integration

**Responsibilities:**
- Include virtual buffer pods in consolidation simulation to prevent premature capacity removal
- Treat buffer pods like real pods during scheduling simulation
- Reject consolidation if buffer pods can't fit after node removal
- Provide lower disruption cost for buffer pods
	- During consolidation, Karpenter prefers to disrupt nodes with buffer pods over nodes with real workloads
	- Real workloads are prioritized for stability; buffer capacity is more flexible

**Active Buffers:**
- Virtual pods always included in disruption simulation
- Capacity continuously preserved as buffer reacts to workload changes
- Buffer remains active until explicitly deleted



## Design Considerations

### Karpenter's Single-Loop Architecture

Karpenter uses a single provisioning loop for all scheduling decisions. This single-loop design has important implications for buffer pods:
- All pods (real + buffer) are scheduled together in one coherent decision, ensuring optimal resource utilization.
- Cluster state remains consistent during scheduling, preventing race conditions in capacity tracking.
- The singleton pattern helps with buffer pod tracking since there's no risk of concurrent provisioning loops interfering with each other.

### Performance Trade-offs 

The trade-off of this approach is that virtual pods must be reconstructed and injected into the scheduling simulation every provisioning loop. Caching virtual pods between cycles is not viable because cluster state changes between loops (pods are created/deleted, nodes scale, buffer status updates), so virtual pods must always be derived from the current buffer status to ensure correctness. That said, this reconstruction is a cheap operation.


### Virtual Pod Creation

Virtual pods are created in-memory (not stored in cluster state) with deterministic UUIDs for observability purposes. This provides unique identifiers for tracking buffer pods through the provisioning and disruption lifecycle.
Virtual pods are NOT stored in etcd or cluster state. They exist only in-memory during the provisioning cycle. This avoids overhead on the API server and etcd while still providing the observability benefits of unique identifiers.

**Future Consideration:** If we implement stateful virtual pod management (caching pods between cycles), the deterministic UUIDs will enable efficient state tracking without recreating pod identities.


## Open Questions

**Q: How does Karpenter determine when capacity is provisioned?**
A: Buffer status is set to `Provisioning: True` only when virtual pods can be successfully placed on existing cluster capacity without creating new NodeClaims. This ensures the status reflects actual available capacity, not pending capacity. This allows external systems to reliably determine when capacity is actually ready for use, even if NodeClaim provisioning fails.

**Q: Can users update buffer specs?**
A: Yes, active buffers allow spec updates (e.g., changing replicas or percentage). The buffer controller will recalculate and update the status, and the provisioner will adjust capacity accordingly in the next cycle.

**Q: What happens if buffer can't be satisfied due to NodePool limits?**
A: Buffer status reflects actual provisioned replicas may be less than requested. The provisioner will continue to retry provisioning in subsequent cycles. In the future, we can make the retry behavior configurable.


## Data Models

### CapacityBuffer CRD

We copy the upstream CapacityBuffer API types from [`k8s.io/autoscaler/cluster-autoscaler/apis/capacitybuffer/autoscaling.x-k8s.io/v1alpha1`](https://github.com/kubernetes/autoscaler/tree/master/cluster-autoscaler/apis/capacitybuffer/autoscaling.x-k8s.io/v1alpha1) into our own package rather than taking a direct dependency. This avoids pulling in the entire autoscaler module and its transitive dependencies, while giving us full control over release cadence. The copied types include a comment referencing the upstream source for future syncing.

The copied types define:


```go
type CapacityBuffer struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty" protobuf:"bytes,1,opt,name=metadata"`

	// Spec defines the desired characteristics of the buffer.
	// +required
	Spec CapacityBufferSpec `json:"spec" protobuf:"bytes,2,opt,name=spec"`

	// Status represents the current state of the buffer and its readiness for autoprovisioning.
	// +optional
	Status CapacityBufferStatus `json:"status,omitempty" protobuf:"bytes,3,opt,name=status"`
}

type CapacityBufferSpec struct {
	// provisioningStrategy defines how the buffer is utilized.
	// "buffer.x-k8s.io/active-capacity" is the default strategy.
	// +kubebuilder:default="buffer.x-k8s.io/active-capacity"
	// +optional
	ProvisioningStrategy *string `json:"provisioningStrategy,omitempty" protobuf:"bytes,1,opt,name=provisioningStrategy"`

	// podTemplateRef is a reference to a PodTemplate resource in the same namespace.
	// Exactly one of `podTemplateRef`, `scalableRef` should be specified.
	// +optional
	PodTemplateRef *LocalObjectRef `json:"podTemplateRef,omitempty" protobuf:"bytes,2,opt,name=podTemplateRef"`

	// scalableRef is a reference to an object with a scale subresource.
	// Exactly one of `podTemplateRef`, `scalableRef` should be specified.
	// +optional
	ScalableRef *ScalableRef `json:"scalableRef,omitempty" protobuf:"bytes,3,opt,name=scalableRef"`

	// replicas defines the desired number of buffer chunks to provision.
	// If neither `replicas` nor `percentage` is set, as many chunks as fit within
	// defined resource limits (if any) will be created. If both are set, the minimum
	// of the two will be used.
	// +optional
	// +kubebuilder:validation:Minimum=0
	Replicas *int32 `json:"replicas,omitempty" protobuf:"varint,4,opt,name=replicas"`

	// percentage defines the desired buffer capacity as a percentage of the
	// `scalableRef`'s current replicas. This is only applicable if `scalableRef` is set.
	// For example, if `scalableRef` has 10 replicas and `percentage` is 20, 2 buffer chunks will be created.
	// +optional
	// +kubebuilder:validation:Minimum=0
	Percentage *int32 `json:"percentage,omitempty" protobuf:"varint,5,opt,name=percentage"`

	// limits specifies resource constraints that limit the number of chunks created.
	// If `replicas` or `percentage` are not set, this will be used to create as many
	// chunks as fit into these limits.
	// +optional
	Limits Limits `json:"limits,omitempty" protobuf:"bytes,6,opt,name=limits"`
}

// CapacityBufferStatus defines the observed state of CapacityBuffer.
type CapacityBufferStatus struct {
	// conditions provide a standard mechanism for reporting the buffer's state.
	// +optional
	// +patchMergeKey=type
	// +patchStrategy=merge
	// +listType=map
	// +listMapKey=type
	Conditions []metav1.Condition `json:"conditions,omitempty" patchStrategy:"merge" patchMergeKey:"type" protobuf:"bytes,1,rep,name=conditions"`

	// podTemplateRef is the observed reference to the PodTemplate that was used
	// to provision the buffer.
	// +optional
	PodTemplateRef *LocalObjectRef `json:"podTemplateRef,omitempty" protobuf:"bytes,1,opt,name=podTemplateRef"`

	// replicas is the actual number of buffer chunks currently provisioned.
	// +optional
	Replicas *int32 `json:"replicas,omitempty" protobuf:"varint,2,opt,name=replicas"`

	// podTemplateGeneration is the observed generation of the PodTemplate.
	// +optional
	PodTemplateGeneration *int64 `json:"podTemplateGeneration,omitempty" protobuf:"varint,3,opt,name=podTemplateGeneration"`

	// provisioningStrategy defines how the buffer should be utilized.
	// +optional
	ProvisioningStrategy *string `json:"provisioningStrategy,omitempty" protobuf:"bytes,5,opt,name=provisioningStrategy"`
}

```



## Examples

### Example 1: Active Buffer with Fixed Replicas

**Use Case:** Maintain 5 spare pods for a web application to handle traffic spikes.

```yaml
apiVersion: autoscaling.x-k8s.io/v1alpha1
kind: CapacityBuffer
metadata:
  name: web-app-buffer
  namespace: production
spec:
  provisioningStrategy: "buffer.x-k8s.io/active-capacity"
  scalableRef:
	apiGroup: apps
	kind: Deployment
	name: web-app
  replicas: 5
```

### Example 2: Active Buffer with Percentage

**Use Case:** Maintain 20% spare capacity for a microservice that scales frequently.

```yaml
apiVersion: autoscaling.x-k8s.io/v1alpha1
kind: CapacityBuffer
metadata:
  name: api-service-buffer
  namespace: production
spec:
  provisioningStrategy: "buffer.x-k8s.io/active-capacity"
  scalableRef:
	apiGroup: apps
	kind: Deployment
	name: api-service
  percentage: 20
  replicas: 10  # Cap at 10 pods maximum
```


### Example 3: Active Buffer with PodTemplate

**Use Case:** Maintain spare capacity for batch processing jobs. Capacity will be continuously maintained until buffer is deleted.

```yaml
apiVersion: v1
kind: PodTemplate
metadata:
  name: batch-job-template
  namespace: batch-workloads
template:
  spec:
	containers:
	- name: processor
	  image: batch-processor:v2
	  resources:
		requests:
		  cpu: "4"
		  memory: "16Gi"
	nodeSelector:
	  workload-type: batch
---
apiVersion: autoscaling.x-k8s.io/v1alpha1
kind: CapacityBuffer
metadata:
  name: batch-job-capacity
  namespace: batch-workloads
spec:
  provisioningStrategy: "buffer.x-k8s.io/active-capacity"
  podTemplateRef:
	name: batch-job-template
  replicas: 5
```


### Example 4: Active Buffer with Resource Limits

**Use Case:** Maintain spare capacity up to a specific resource limit.

```yaml
apiVersion: v1
kind: PodTemplate
metadata:
  name: worker-template
  namespace: workers
template:
  spec:
	containers:
	- name: worker
	  image: worker:latest
	  resources:
		requests:
		  cpu: "2"
		  memory: "4Gi"
---
apiVersion: autoscaling.x-k8s.io/v1alpha1
kind: CapacityBuffer
metadata:
  name: worker-buffer
  namespace: workers
spec:
  provisioningStrategy: "buffer.x-k8s.io/active-capacity"
  podTemplateRef:
	name: worker-template
  limits:
	cpu: "20"
	memory: "40Gi"
```


## Testing Strategy

For testing, we will add comprehensive integration tests to ensure the feature works correctly across different scenarios:
- Active buffer scales with deployment changes
- Active buffer reacts to percentage-based sizing
- Buffer respects NodePool limits
- Consolidation preserves active buffer capacity
- Buffer status accurately reflects provisioning state
- Virtual pod construction and lifecycle
- Multiple buffers with different configurations

## Observability

Controller-runtime metrics already provide baseline visibility into reconcile performance and errors. We will have status fields to let customers know the status of the buffer.


# Graduation Criteria

## Alpha (`v1alpha1`)

- Feature gate `CapacityBuffer` defaults to false
- CapacityBuffer CRD with active buffer strategy (`buffer.x-k8s.io/active-capacity`)
- Support for `scalableRef` (Deployment, StatefulSet, ReplicaSet), `podTemplateRef`, `replicas`, `percentage`, and `limits`
- Virtual pod integration with provisioning and disruption loops
- Status conditions (`ReadyForProvisioning`, `Provisioning`) accurately reflect buffer state
- Emptiness protection: buffer nodes are not consolidated as "empty"
- Consolidation awareness: virtual pods participate in `SimulateScheduling` to prevent undersized replacements
- Drift and expiry: allowed — buffer refills after node replacement
- Memory leak prevention: virtual pods filtered from cluster state scheduling decision maps
- Sidecar-aware resource accounting (KEP-753) via `k8s.io/component-helpers/resource.PodRequests`
- PVC and ephemeral volume stripping from virtual pods to avoid topology resolution errors
- Integration tests covering core scenarios (provisioning, refill, disruption, NodePool limits, multiple buffers)
- Unit tests covering replica calculation, virtual pod construction, sanitization, classification, and filtering
- Same-namespace restriction for `scalableRef` and `podTemplateRef`
- RBAC rules for `autoscaling.x-k8s.io/capacitybuffers` and `core/v1/podtemplates`

## Beta (`v1beta1`)

- Feature gate `CapacityBuffer` defaults to true
- At least two releases in alpha with no breaking API changes
- Positive user feedback from alpha adoption
- Performance benchmarks demonstrating acceptable overhead from virtual pod scheduling simulation
- E2E test coverage across multiple cloud providers (AWS, Azure)
- Metrics for buffer utilization and provisioning latency impact
- Support for `batch/v1/Job` via scale subresource (if upstream API adds support)
- Watches on scalableRef workloads for faster reaction to Deployment/StatefulSet changes (currently 30s polling)

## GA (`v1`)

- At least two releases in beta with no breaking API changes
- Feature gate removed; feature always enabled
- `fulfilledBy` field implemented and validated — tracking which pods consume buffer capacity
- No outstanding critical bugs or performance regressions
- API stability: upstream sig-autoscaling API has reached v1 or Karpenter has committed to supporting the alpha/beta API shape indefinitely

# Future Work

**Ephemeral Capacity Strategy:**

To harden support for batch systems. Kueue can work with active buffers today, but it's racey — ephemeral strategy provides deterministic one-time capacity with completion semantics:
- One-time capacity request that completes when consumed
- Integration with external admission controllers
- Eliminates race between buffer refill and batch job admission

This is deferred to allow us to:
- Validate the active buffer implementation first
- Gather user feedback on the core functionality
- Drive consensus with sig-autoscaling on ephemeral strategy semantics

**Precomputed Virtual Pod Store:**

Consider having the buffer controller precompute virtual pods into an in-memory store (similar to `pkg/controllers/state/Cluster`) so the provisioner hot path becomes a cache read instead of List + Get per scheduling pass. Currently the overhead is negligible at typical buffer counts (1-10), but may matter at scale (100+).

**fulfilledBy Field (Capacity Tracking):**

We propose adding a `fulfilledBy` field to track which pods are consuming buffer capacity. This would provide:

**Functionality:**
- Selector-based mechanism to identify pods that fulfill buffer capacity
- Automatic tracking of buffer capacity consumption as matching pods schedule
- Status field showing which pods are currently using buffer capacity
- Enables more intelligent buffer sizing and capacity planning

**Use Cases:**
- Visibility into which workloads are using pre-provisioned capacity
- Automatic buffer adjustment as matching pods consume capacity
- Better integration with batch schedulers that need to know capacity allocation
- Debugging and observability for capacity utilization

**Example:**
```yaml
apiVersion: autoscaling.x-k8s.io/v1alpha1
kind: CapacityBuffer
metadata:
  name: web-app-buffer
spec:
  provisioningStrategy: "buffer.x-k8s.io/active-capacity"
  scalableRef:
    apiGroup: apps
    kind: Deployment
    name: web-app
  replicas: 5
  fulfilledBy:  # Future field
    matchLabels:
      app: web-app
      tier: frontend
status:
  replicas: 5
```

**Implementation Considerations:**
- Requires tracking pod-to-buffer mappings in memory
- Need to handle pod lifecycle events (creation, deletion, updates)
- Should work with both `scalableRef` and `podTemplateRef`

**Rationale for Deferring:**
- Adds complexity to initial implementation
- Can be added incrementally without breaking existing buffers
- Want to validate core buffer functionality first

# Alternatives

### Alternative 1: Balloon Pods/Deployments


**Why CapacityBuffer is better:**
- Virtual pods avoid scheduler preemption overhead (no actual pods to evict)
- Automatic adaptation to workload changes
- Pod-level abstraction with automatic node selection
- Standard API compatible with Cluster Autoscaler
- Clear semantics for maintaining spare capacity


## References

- [Cluster Autoscaler Buffer Proposal](https://github.com/kubernetes/autoscaler/blob/master/cluster-autoscaler/proposals/buffers.md)
- [CapacityBuffer CRD](https://github.com/kubernetes/autoscaler/tree/master/cluster-autoscaler/apis/capacitybuffer)
- [Karpenter Issue #2571](https://github.com/kubernetes-sigs/karpenter/issues/2571)
