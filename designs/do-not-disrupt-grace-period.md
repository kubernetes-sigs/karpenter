# Do-Not-Disrupt Grace Period

## Summary

This feature adds a grace period option to the `karpenter.sh/do-not-disrupt` pod annotation, allowing users to temporarily protect pods from disruption while ensuring that nodes can eventually be disrupted for cluster maintenance and updates.

## Motivation

### Problem Statement

Currently, when a pod has the `karpenter.sh/do-not-disrupt: "true"` annotation, it indefinitely prevents Karpenter from disrupting the node where the pod is running. This creates several issues:

1. **Cluster Maintenance Challenges**: Workload owners can inadvertently or intentionally block critical cluster maintenance operations (security updates, node replacements, etc.)
2. **No Upper Bound**: Long-running jobs (CI/CD pipelines, ML training) need protection during execution, but there's no mechanism to automatically lift this protection after a reasonable time
3. **Administrative Control**: Cluster administrators have limited control over how long a workload can block node disruption

### Use Cases

1. **CI/CD Pipelines**: Jobs that should not be interrupted but have a known maximum runtime (e.g., 4 hours)
2. **ML Training Jobs**: Long-running training that should complete uninterrupted but has an expected duration
3. **Batch Processing**: Jobs that should run to completion but have predictable execution times
4. **Cluster Maintenance**: Ensuring that even with do-not-disrupt pods, nodes can eventually be disrupted for mandatory updates

## Proposal

### API Design

Add a new annotation `karpenter.sh/do-not-disrupt-grace-period` that specifies the duration (in seconds) for which the `karpenter.sh/do-not-disrupt` protection should remain active.

#### Annotation

```yaml
apiVersion: v1
kind: Pod
metadata:
  name: long-running-job
  annotations:
    karpenter.sh/do-not-disrupt: "true"
    karpenter.sh/do-not-disrupt-grace-period: "14400"  # 4 hours in seconds
spec:
  # ...
```

### Behavior

1. **Without Grace Period**: If only `karpenter.sh/do-not-disrupt: "true"` is set (no grace period annotation), the behavior remains the same as today - indefinite protection
2. **With Grace Period**: When both annotations are set:
   - The pod is protected from disruption for the specified duration starting from the pod's creation time
   - After the grace period expires, the pod is treated as if it doesn't have the do-not-disrupt annotation
   - The node becomes eligible for disruption if no other constraints prevent it
3. **Invalid Grace Period**: If the grace period value is invalid (non-numeric, zero, or negative), it is treated as indefinite protection (backward compatible behavior)

### Time Calculation

The grace period expiration time is calculated as:
```
expiration_time = pod.CreationTimestamp + grace_period_seconds
```

### Implementation Details

#### New Constants

```go
// pkg/apis/v1/labels.go
const (
    DoNotDisruptAnnotationKey            = apis.Group + "/do-not-disrupt"
    DoNotDisruptGracePeriodAnnotationKey = apis.Group + "/do-not-disrupt-grace-period"
)
```

#### Core Logic Changes

1. **`pkg/utils/pod/scheduling.go`**:
   - New function `HasDoNotDisruptWithGracePeriod(pod, clock)` - checks if the grace period has expired
   - New function `IsDisruptableWithClock(pod, clock)` - disruption check that respects grace period
   
2. **`pkg/controllers/state/statenode.go`**:
   - New method `ValidatePodsDisruptableWithClock(ctx, kubeClient, pdbs, clock)` - validates pod disruptability with grace period support

3. **`pkg/controllers/disruption/types.go`**:
   - Updated `NewCandidate()` to use `ValidatePodsDisruptableWithClock()` with clock parameter

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
        karpenter.sh/do-not-disrupt: "true"
        karpenter.sh/do-not-disrupt-grace-period: "7200"  # 2 hours
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
    karpenter.sh/do-not-disrupt: "true"
    karpenter.sh/do-not-disrupt-grace-period: "86400"  # 24 hours
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
    # No grace period - indefinite protection
spec:
  containers:
  - name: service
    image: critical-app:latest
```

**Behavior**:
- Maintains backward compatibility
- Pod is protected indefinitely
- Same behavior as before this feature was added

### Interaction with Other Features

#### Termination Grace Period

The `karpenter.sh/do-not-disrupt-grace-period` works independently of:
- Pod's `terminationGracePeriodSeconds`
- NodeClaim's `spec.terminationGracePeriod`

When both do-not-disrupt grace period expires AND NodeClaim has a terminationGracePeriod set:
1. If in **graceful disruption** mode: Node won't be disrupted even after grace period expires if blocking PDBs exist
2. If in **eventual disruption** mode: Node will be disrupted after grace period expires, ignoring blocking PDBs

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

### Alternative 2: Absolute Timestamp Annotation

Use an absolute timestamp instead of duration.

**Rejected because**:
- Requires users to calculate absolute timestamps
- More error-prone
- Less intuitive than duration-based specification

### Alternative 3: Controller-Managed Expiration

Have a controller automatically remove the do-not-disrupt annotation after a period.

**Rejected because**:
- Modifying user-defined annotations can be confusing
- Doesn't preserve the original intent in pod spec
- More complex implementation

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
