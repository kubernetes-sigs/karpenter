# Do-Not-Disrupt Grace Period

## Summary

This feature adds a grace period option to the `karpenter.sh/do-not-disrupt` pod annotation, allowing users to temporarily protect pods from disruption while ensuring that nodes can eventually be disrupted for cluster maintenance and updates.

## Motivation

### Problem Statement

Currently, when a pod has the `karpenter.sh/do-not-disrupt: "true"` annotation, it indefinitely prevents Karpenter from disrupting the node where the pod is running. This creates several issues:

1. **Cluster Maintenance Challenges**: Workload owners can inadvertently or intentionally block critical cluster maintenance operations (security updates, node replacements, etc.)
2. **No Upper Bound**: Long-running jobs (CI/CD pipelines, ML training) need protection during execution, but there's no mechanism to automatically lift this protection after a reasonable time
3. **Administrative Control**: Cluster administrators have limited control over how long a workload can block node disruption

### Why This Feature Belongs in Karpenter

While this might appear to be a concern for higher-level abstractions (PodDisruptionBudget, Job controllers, or CI/CD systems), this feature addresses a specific gap in the Kubernetes ecosystem that is uniquely suited to Karpenter's role as a node lifecycle manager:

1. **Platform Engineering Reality**: As noted in [#752](https://github.com/kubernetes-sigs/karpenter/issues/752#issuecomment-1099635819), "having pods stuck due to user error and blocking node maintenance is a common problem when running a platform." Platform teams need declarative mechanisms to prevent indefinite resource blocking without requiring:
   - Manual intervention to find and remove stuck annotations
   - External policy engines (OPA/Kyverno) that add operational complexity
   - Trust that all application teams will implement proper cleanup

2. **Separation of Concerns**: This feature addresses **two distinct disruption scenarios** ([#752](https://github.com/kubernetes-sigs/karpenter/issues/752#issuecomment-2109882683)):
   - **Cost-optimization disruptions** (consolidation): Workload owners should have full control to block these
   - **Mandatory maintenance disruptions** (security updates, node replacements): Cluster administrators need eventual override capability
   
   The grace period provides workload owners with a way to declare maximum protection time, preventing indefinite blocking while still allowing necessary protection during normal execution.

3. **Beyond Job Abstractions**: Many workloads requiring protection are **not Kubernetes Jobs**:
   - CI/CD systems (Jenkins agents, Tekton tasks, custom runners) often run as standalone Pods
   - These systems have their own orchestration layers and retry mechanisms
   - They need integration with existing infrastructure that may not use Job primitives
   - `activeDeadlineSeconds` is Job-specific and doesn't apply to these use cases

4. **Complementary to terminationGracePeriod**: While NodeClaim's `terminationGracePeriod` provides cluster-wide eventual disruption, this feature enables:
   - **Self-documenting workload requirements**: "This pod needs 4 hours, not indefinite protection"
   - **Granular per-workload control**: Different workloads have different time requirements
   - **Declarative expiration**: No external controllers or manual cleanup needed

This is fundamentally a **node lifecycle management concern**, not an application lifecycle concern, making it a natural fit for Karpenter's responsibility domain.

### Use Cases

The grace period is not about "jobs getting cheaper to disrupt over time" - it's about establishing a **maximum protection duration** to prevent indefinite resource blocking while still protecting workloads during their expected execution window.

1. **CI/CD Pipelines**: Jobs that should not be interrupted but have a known maximum runtime (e.g., 4 hours)
   - **Scenario**: An E2E test suite that typically completes in 2 hours but might take up to 4 hours
   - **Grace Period**: 4 hours - protects normal execution while ensuring stuck/hanging tests can't block nodes indefinitely
   - **Why**: Re-running expensive integration tests is costly, but indefinite protection blocks cluster maintenance

2. **Preventing Stuck Workloads**: Protection against user error or application bugs
   - **Scenario**: A workload owner forgets to remove the annotation after job completion, or the application hangs
   - **Grace Period**: Declares "this should never run longer than X hours" - automatic cleanup without manual intervention
   - **Why**: Platform teams need defense mechanisms against misconfigured workloads blocking node lifecycle

3. **ML Training Jobs**: Long-running training that should complete uninterrupted but has an expected duration
   - **Scenario**: Training that typically takes 20 hours
   - **Grace Period**: 24 hours - provides buffer for normal execution while preventing indefinite node blocking
   - **Why**: Balances protection for expensive computation with eventual disruption capability

4. **Batch Processing**: Jobs that should run to completion but have predictable execution times
   - **Scenario**: Nightly data processing that normally takes 1-2 hours
   - **Grace Period**: 4 hours - generous buffer while ensuring daylight maintenance windows remain available

5. **Cluster Maintenance Windows**: Ensuring nodes can eventually be disrupted for mandatory updates
   - **Scenario**: Security patches require node replacement, but workload owners have used do-not-disrupt
   - **Grace Period**: Provides cluster administrators confidence that nodes will become available within a known timeframe
   - **Why**: Enables planning maintenance windows without requiring coordination with all workload owners

## Proposal

### API Design

Extend the existing `karpenter.sh/do-not-disrupt` annotation to accept duration values in addition to boolean values. This provides a cleaner, more intuitive API compared to using separate annotations.

#### Annotation Values

- `"true"`: Indefinite protection (existing behavior, backward compatible)
- Duration string: Protection for the specified duration from pod creation time (e.g., `"4h"`, `"30m"`, `"1h30m"`)
  - Follows Go's `time.Duration` format
  - Common examples: `"30m"`, `"1h"`, `"4h"`, `"24h"`, `"1h30m"`

#### Example

```yaml
apiVersion: v1
kind: Pod
metadata:
  name: long-running-job
  annotations:
    karpenter.sh/do-not-disrupt: "4h"  # Protected for 4 hours
spec:
  # ...
```

### Behavior

1. **Indefinite Protection**: If `karpenter.sh/do-not-disrupt: "true"` is set, the behavior remains the same as today - indefinite protection (backward compatible)
2. **Time-Limited Protection**: If set to a duration value (e.g., `"4h"`):
   - The pod is protected from disruption for the specified duration starting from the pod's creation time
   - After the grace period expires, the pod is treated as if it doesn't have the do-not-disrupt annotation
   - The node becomes eligible for disruption if no other constraints prevent it
3. **Invalid Values**: If the value cannot be parsed as either `"true"` or a valid duration, it is treated as indefinite protection (backward compatible, fail-safe behavior)

### Time Calculation

The grace period expiration time is calculated as:
```
expiration_time = pod.CreationTimestamp + parsed_duration
```

For example, if a pod is created at `2024-01-01T10:00:00Z` with `karpenter.sh/do-not-disrupt: "4h"`, it will be protected until `2024-01-01T14:00:00Z`.

### Implementation Details

#### Parsing Logic

```go
// pkg/utils/pod/scheduling.go
func parseDoNotDisrupt(value string) (indefinite bool, duration time.Duration, err error) {
    if value == "true" {
        return true, 0, nil
    }
    
    d, err := time.ParseDuration(value)
    if err != nil {
        // Invalid format - treat as indefinite (fail-safe)
        return true, 0, nil
    }
    
    if d <= 0 {
        // Zero or negative - treat as indefinite (fail-safe)
        return true, 0, nil
    }
    
    return false, d, nil
}
```

#### Core Logic Changes

1. **`pkg/utils/pod/scheduling.go`**:
   - New function `parseDoNotDisrupt(value)` - parses the annotation value
   - New function `IsDoNotDisruptActive(pod, clock)` - checks if protection is still active
   - Update `IsDisruptable(pod)` to use clock-aware checking

2. **`pkg/controllers/state/statenode.go`**:
   - Update pod disruptability validation to use clock parameter for testing

3. **`pkg/controllers/disruption/types.go`**:
   - Use updated disruptability checking logic

### Examples

#### Example 1: Batch Job with 2-hour Grace Period

```yaml
apiVersion: batch/v1
kind: Job
metadata:
  name: data-processing-job
spec:
  template:
    metadata:
      annotations:
        karpenter.sh/do-not-disrupt: "2h"
    spec:
      containers:
      - name: processor
        image: data-processor:latest
        # Job that typically completes in 1-2 hours
```

**Behavior**:
- For the first 2 hours after pod creation, the node hosting this job will not be disrupted
- After 2 hours, if the job hasn't completed, the node becomes eligible for disruption
- This provides protection for normal execution while ensuring the node can eventually be disrupted

#### Example 2: ML Training Job with 24-hour Grace Period

```yaml
apiVersion: v1
kind: Pod
metadata:
  name: ml-training
  annotations:
    karpenter.sh/do-not-disrupt: "24h"
spec:
  containers:
  - name: trainer
    image: ml-framework:latest
    # Training job expected to complete in ~20 hours
```

**Behavior**:
- Protected from disruption for 24 hours
- After 24 hours, the node can be disrupted even if the training is still running
- Allows cluster administrators to enforce maximum protection duration

#### Example 3: Critical Service with Indefinite Protection

```yaml
apiVersion: v1
kind: Pod
metadata:
  name: critical-service
  annotations:
    karpenter.sh/do-not-disrupt: "true"
spec:
  containers:
  - name: service
    image: critical-app:latest
```

**Behavior**:
- Maintains backward compatibility
- Pod is protected indefinitely
- Same behavior as before this feature was added

#### Example 4: Short-Running Task

```yaml
apiVersion: v1
kind: Pod
metadata:
  name: quick-task
  annotations:
    karpenter.sh/do-not-disrupt: "30m"
spec:
  containers:
  - name: task
    image: task-runner:latest
```

**Behavior**:
- Protected for 30 minutes
- Suitable for tasks with predictable, short execution times
- Demonstrates flexibility of duration format

### Interaction with Other Features

#### Comparison with terminationGracePeriod

This feature is complementary to, but distinct from, NodeClaim's `spec.terminationGracePeriod`:

| Feature | Scope | Purpose | Set By | Behavior |
|---------|-------|---------|--------|----------|
| `karpenter.sh/do-not-disrupt: "4h"` | **Pod-level** | Self-expiring protection | Workload owner | "Protect this specific pod for 4 hours, then allow disruption" |
| `spec.terminationGracePeriod` | **NodeClaim-level** | Eventual disruption guarantee | Cluster admin | "Disrupt this node after N hours regardless of pods" |

**Key Differences**:

1. **Granularity**: 
   - `do-not-disrupt` duration: Per-pod protection with automatic expiration
   - `terminationGracePeriod`: Global override affecting all pods on a node

2. **Ownership**:
   - `do-not-disrupt` duration: Declared by workload owners in pod specs
   - `terminationGracePeriod`: Configured by cluster administrators in NodePool

3. **Use Case**:
   - `do-not-disrupt` duration: "This workload needs protection for its expected runtime"
   - `terminationGracePeriod`: "Ensure all nodes can eventually be disrupted for maintenance"

**Combined Behavior**:

When both mechanisms are active:
1. Pod-level protection expires first → Node becomes eligible for disruption if no other blocking pods
2. NodeClaim termination grace period expires → Node is forcefully disrupted even with blocking pods (eventual disruption mode)

**Example Scenario**:
```yaml
# NodePool with 72-hour eventual disruption
spec:
  disruption:
    consolidationPolicy: WhenEmpty
  template:
    spec:
      terminationGracePeriod: 72h

---
# Pod with 4-hour protection
metadata:
  annotations:
    karpenter.sh/do-not-disrupt: "4h"
```

Timeline:
- **0-4 hours**: Pod is protected, node cannot be disrupted
- **4-72 hours**: Pod protection expired, node eligible for disruption (respects PDBs in graceful mode)
- **After 72 hours**: Node forcefully disrupted regardless of pods (eventual disruption mode)

This layered approach provides:
- **Workload owners**: Ability to protect their workloads during expected execution
- **Platform teams**: Confidence that protection will automatically expire
- **Cluster admins**: Ultimate control via terminationGracePeriod safety net

#### Interaction with PodDisruptionBudget

Pod-level protection expiration is evaluated **before** PDB checks:

1. Check if `do-not-disrupt` protection is still active
2. If expired or not set, proceed to PDB evaluation
3. Apply disruption based on PDB constraints

This ensures time-limited protection doesn't permanently circumvent PDBs.

#### Pod terminationGracePeriodSeconds

The annotation controls **when** a pod can be disrupted. Pod's `terminationGracePeriodSeconds` controls **how** gracefully the pod is terminated after disruption begins. These are independent concerns.

### Monitoring and Observability

Users can determine the grace period status by:
1. Checking pod annotations for the grace period value
2. Comparing current time against `pod.CreationTimestamp + grace_period`
3. Observing Karpenter events when nodes become eligible for disruption

Potential future enhancements:
- Add metrics for pods with do-not-disrupt grace periods
- Emit events when grace periods expire

## Alternatives Considered

### Alternative 1: Node-Level Grace Period

Set grace period at the node level instead of pod level.

**Rejected because**: 
- Less granular control
- Doesn't align with the pod-level nature of do-not-disrupt annotation
- Multiple pods on a node may have different requirements

### Alternative 2: Separate Grace Period Annotation

Use two separate annotations: `karpenter.sh/do-not-disrupt: "true"` and `karpenter.sh/do-not-disrupt-grace-period: "14400"`.

**Rejected because**:
- Less intuitive - requires understanding two related annotations
- More verbose - users must specify both annotations
- Easier to misconfigure - what if grace-period is set without do-not-disrupt?
- Single annotation with overloaded values is cleaner and follows Kubernetes patterns (e.g., affinity weights)
- Suggested by reviewer: "I think this might be better modeled as a go duration in the same field"

### Alternative 3: Absolute Timestamp Annotation

Use an absolute timestamp instead of duration.

**Rejected because**:
- Requires users to calculate absolute timestamps
- More error-prone
- Less intuitive than duration-based specification

### Alternative 4: Controller-Managed Expiration

Have a controller automatically remove the do-not-disrupt annotation after a period.

**Rejected because**:
- Modifying user-defined annotations can be confusing
- Doesn't preserve the original intent in pod spec
- More complex implementation

### Alternative 5: Rely on Higher-Level Abstractions

Push this responsibility to Job controllers, CI/CD systems, or PodDisruptionBudget enhancements.

**Rejected because**:
- **Job limitation**: Many workloads requiring protection are not Jobs (CI/CD pods, custom runners)
- **System diversity**: Different CI/CD systems have different architectures; requiring all to implement deadline logic is impractical
- **Platform engineering reality**: Platform teams need defense mechanisms against misconfigured or stuck workloads
- **PDB scope**: PodDisruptionBudget controls disruption policy but doesn't provide time-based expiration
- **Pragmatic leverage point**: Karpenter is the natural place for node lifecycle controls to prevent indefinite resource blocking
- As reviewer noted: "I understand the competing layers and how Karpenter ends up being a pragmatic point of leverage for platform engineering teams"

While ideally this would be a Kubernetes-native PDB feature, the immediate problem space (platform teams managing node lifecycle with reasonable bounds on user-controlled blocking) makes Karpenter a suitable implementation point.

## Testing

### Unit Tests

- Test `HasDoNotDisruptWithGracePeriod` with various grace period values
- Test grace period expiration calculation
- Test invalid grace period handling
- Test backward compatibility (no grace period annotation)

### Integration Tests

- Test node disruption with expired grace periods
- Test interaction with eventual vs graceful disruption modes
- Test multiple pods with different grace periods on the same node

## Backward Compatibility

This feature is fully backward compatible:
- Existing pods with only `karpenter.sh/do-not-disrupt: "true"` continue to work as before
- Invalid grace period values default to indefinite protection
- No changes to existing API contracts

## Rollout Plan

1. **Phase 1**: Implement core functionality and unit tests
2. **Phase 2**: Add integration tests
3. **Phase 3**: Update documentation and examples
4. **Phase 4**: Release in a minor version with feature announcement

## Documentation Updates

Required documentation changes:
- Update disruption documentation to explain grace period annotation
- Add examples showing different use cases
- Update API reference to include new annotation
- Add migration guide for users who want to adopt grace periods

## Future Enhancements

Potential future improvements:
1. **Metrics**: Expose metrics for grace period usage and expiration
2. **Validation**: Add admission webhook validation for grace period values
3. **Events**: Emit Kubernetes events when grace periods expire
4. **Dashboard**: Show grace period status in cluster dashboards
