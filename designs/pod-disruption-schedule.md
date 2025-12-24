# Pod Disruption Schedule

## Motivation

Users running workloads that are sensitive to interruptions at specific times need finer-grained control over Karpenter's disruption behavior. Currently the `karpenter.sh/do-not-disrupt` annotation only provides binary protection, either disruption is fully blocked or allowed indefinitely. This doesn't accommodate scenarios where disruptions on specific workloads should only occur during designated maintenance windows. NodePool disruption budgets are also insufficient because it means a user would either need to dedicate a NodePool for each workload's disruption window which leads to fragmentation and cost waste or blocking disruptions for _all_ workloads during the combined windows.

### Use Cases

**Long-running task executors**: Services that continuously poll work queues and execute expensive, multi-hour computations. For example, an ML training service that picks up training jobs from a queue—once a job starts, it may run for 8+ hours and cannot be checkpointed. These pods don't play well with normal Karpenter behavior and need protection during active work but can safely be disrupted during scheduled maintenance windows (e.g., Saturday 2-6 AM) when the queue is drained.

**Latency-sensitive applications**: Even when a workload can safely tolerate disruption, the disruption is never completely free—there's always some latency spike, brief capacity reduction, or connection reset during pod termination and rescheduling. For latency-sensitive services like payment processors, real-time bidding systems, or user-facing APIs, it's preferable to absorb this cost during off-peak hours rather than risk degraded user experience during peak traffic, even if the immediate cost savings from consolidation are deferred.

**Stateful workloads with expensive initialization**: Database replicas, search indices, or cache warmers that take significant time to become fully operational after restart. Disrupting these during high-traffic periods causes degraded performance while the new instance catches up. Maintenance windows allow controlled rollouts when traffic is low.

### Limitations of Existing Controls

Karpenter and Kubernetes provide several mechanisms for controlling disruption, but none address time-based per-workload scheduling:

#### `karpenter.sh/do-not-disrupt` Annotation

This annotation provides binary protection—disruption is either fully blocked or fully allowed. It cannot express "disrupt me only on weekends" or "protect me during business hours." Additionally, using `do-not-disrupt` alongside PDBs creates a dangerous interaction when nodes have a `terminationGracePeriod` configured.

When a node expires and reaches its TGP, Karpenter forcefully evicts all pods regardless of PDB constraints:

1. A Deployment has 3 replicas with a PDB of `maxUnavailable: 1`
2. All pods have `karpenter.sh/do-not-disrupt: "true"` to protect long-running work
3. The NodePool has `terminationGracePeriod: 24h` to enforce security patching
4. Two nodes hosting these pods expire and hit their TGP simultaneously
5. Karpenter force-evicts both pods at once, violating the PDB's `maxUnavailable: 1`

This means there is currently **no safe way** to use `do-not-disrupt` alongside PDBs when nodes have expiration configured.

#### `pod-deletion-cost` Annotation

The `controller.kubernetes.io/pod-deletion-cost` annotation influences *which* pods Karpenter prefers to disrupt—pods with higher deletion costs are disrupted last. However, this is a relative preference, not an absolute constraint. If a high-cost pod is the only option (e.g., the only pod on a node being consolidated), it will still be disrupted. Workloads that need time-based protection require a hard gate that blocks disruption entirely during certain periods, not just a preference that can be overridden.

#### NodePool Disruption Budgets

NodePool budgets operate at the infrastructure level and cannot express per-workload scheduling preferences:

- **Fragmentation**: To give workload A a Saturday maintenance window and workload B a Sunday window, you need separate NodePools. This fragments capacity, reduces bin-packing efficiency, and increases costs. A cluster with 10 different maintenance window requirements would need 10+ NodePools.

- **Least common denominator**: When workloads with different schedules share a NodePool, the budget must accommodate all of them. If workload A needs protection Mon-Fri 9-5 and workload B needs protection Sat-Sun, the combined budget blocks disruption almost entirely, eliminating consolidation savings.

- **Coarse granularity**: A NodePool budget of `nodes: 0` during business hours blocks ALL consolidation, even for pods that could safely move. Pod-level schedules allow Karpenter to continue optimizing around protected workloads.

#### `consolidateAfter`

The NodePool `consolidateAfter` setting delays when Karpenter begins evaluating a node for consolidation after it becomes consolidatable. This affects the timing of consolidation decisions but doesn't provide time-based windows—once the delay passes, consolidation proceeds normally regardless of time of day.

#### Summary

| Control | Scope | Type | Time-Based |
|---------|-------|------|------------|
| `do-not-disrupt` | Pod | Hard block | No (binary) |
| `pod-deletion-cost` | Pod | Soft preference | No |
| NodePool budgets | NodePool | Hard block | Yes, but per-NodePool |
| `consolidateAfter` | NodePool | Delay | No |
| **Disruption schedule** | **Pod** | **Hard block** | **Yes** |

Pod disruption schedules fill the gap: a per-pod, time-based hard constraint that allows workloads to opt into scheduled maintenance windows where Karpenter can gracefully disrupt pods while respecting PDBs.

### Current Workarounds

I'm currently working around this limitation by running custom controllers such as [karpenter-deprovision-controller](https://github.com/jukie/karpenter-deprovision-controller) (Mostly just a POC, I built a more production ready tool internally at my org) that dynamically adds/removes the `do-not-disrupt` annotation based on schedules. This feature would provide native support for time-based disruption controls, eliminating the operational burden of maintaining external controllers.

See: [#1719](https://github.com/kubernetes-sigs/karpenter/issues/1719)

## Proposed Spec

Two new annotations on pods to control when disruption is permitted:

```yaml
apiVersion: v
kind: Pod
metadata:
  name: long-running-task
  annotations:
    # Cron schedule defining the disruption window
    karpenter.sh/disruption-schedule: "0 2 * * 6"  # 2 AM on Saturdays
    # Duration of the disruption window
    karpenter.sh/disruption-schedule-duration: "4h"
```

### Semantic Options

There are two possible semantics for interpreting the schedule:

#### Option A: Allow Window (Recommended)
The schedule defines when the pod **CAN** be disrupted. Outside the window, disruption is blocked (similar to `do-not-disrupt: true`).

```yaml
# Pod can only be disrupted on Saturdays between 2 AM and 6 AM
karpenter.sh/disruption-schedule: "0 2 * * 6"
karpenter.sh/disruption-schedule-duration: "4h"
```

**Pros:**
- Intuitive for maintenance windows ("disrupt me at 2 AM Saturday for 4 hours")
- Safer default—disruption is blocked unless explicitly allowed
- Aligns with how users think about maintenance windows

**Cons:**
- Inverts the typical meaning of `do-not-disrupt` (which blocks disruption)

#### Option B: Block Window
The schedule defines when the pod **CANNOT** be disrupted. Outside the window, disruption is allowed normally.

```yaml
# Pod cannot be disrupted on weekdays during business hours
karpenter.sh/disruption-block-schedule: "0 9 * * 1-5"
karpenter.sh/disruption-block-duration: "8h"
```

**Pros:**
- Consistent with `do-not-disrupt` semantics (both block disruption)
- Useful for protecting during specific critical periods

**Cons:**
- Less intuitive for maintenance window use case
- Requires users to think in negatives

### Example Configurations

#### ML Training Pod - Weekend Maintenance Only
```yaml
metadata:
  annotations:
    karpenter.sh/disruption-schedule: "0 2 * * 6"    # 2 AM Saturday
    karpenter.sh/disruption-schedule-duration: "6h"  # Until 8 AM
```

#### Business Application - After Hours Only
```yaml
metadata:
  annotations:
    karpenter.sh/disruption-schedule: "0 22 * * *"   # 10 PM daily
    karpenter.sh/disruption-schedule-duration: "8h"  # Until 6 AM
```

#### Batch Job - Anytime Sunday
```yaml
metadata:
  annotations:
    karpenter.sh/disruption-schedule: "0 0 * * 0"    # Midnight Sunday
    karpenter.sh/disruption-schedule-duration: "24h" # All day
```

## Code Definition

```go
const (
    // DisruptionScheduleAnnotationKey defines a cron schedule when disruption is permitted.
    // Format follows standard cron syntax: "Minute Hour DayOfMonth Month DayOfWeek"
    // If not set, disruption follows normal pod disruption rules.
    DisruptionScheduleAnnotationKey = apis.Group + "/disruption-schedule"

    // DisruptionScheduleDurationAnnotationKey defines how long the disruption window is active
    // after each schedule trigger. Accepts Go duration format (e.g., "4h", "30m", "1h30m").
    // Required if disruption-schedule is set. Defaults to "1h" if omitted.
    DisruptionScheduleDurationAnnotationKey = apis.Group + "/disruption-schedule-duration"
)
```

### Helper Function

```go
// HasActiveDisruptionSchedule returns true if the pod has a disruption schedule
// and the current time falls within an active disruption window.
// Returns true (allow disruption) if:
//   - No schedule annotation is set
//   - Schedule is set and current time is within the window
// Returns false (block disruption) if:
//   - Schedule is set and current time is outside the window
func HasActiveDisruptionSchedule(ctx context.Context, pod *corev1.Pod) bool {
    schedule := pod.Annotations[DisruptionScheduleAnnotationKey]
    if schedule == "" {
        return true // No schedule means always disruptable
    }

    duration := pod.Annotations[DisruptionScheduleDurationAnnotationKey]
    if duration == "" {
        duration = "1h" // Default to 1 hour or some reasonable default
    }

    // Parse cron schedule and check if current time is within window
    // ... implementation details
}
```

## Validation/Defaults

### Validation Rules
- If `disruption-schedule` is set, it must be a valid cron expression
- `disruption-schedule-duration` must be a valid Go duration string (e.g., "1h", "30m", "2h30m")
- Duration must be positive and at least 1 minute
- Duration should not exceed 7 days (to prevent effectively permanent windows)

### Defaults
- If `disruption-schedule-duration` is omitted when `disruption-schedule` is set, default to `"1h"` (or some reasonable default)
- If neither annotation is set, pod follows normal disruption rules (equivalent to current behavior)

### Error Handling
- Invalid cron expressions should be logged and treated as "always disruptable" (fail-open).
- Invalid durations should be logged and default to 1 hour
- Pods with invalid schedules emit a Warning event

## Integration with Existing Features

### TerminationGracePeriod Interaction
The NodeClaim's `terminationGracePeriod` takes precedence over pod disruption schedules. When a node has exceeded its TGP:
1. Pods are forcefully evicted regardless of their disruption schedule
2. This maintains the cluster administrator's ability to enforce maximum node lifetimes
3. The disruption schedule is a best-effort preference, not a guarantee

```
Node TGP reached -> Force eviction -> Ignore pod schedules
```

### PodDisruptionBudget Interaction
Disruption schedules work alongside PDBs:
- A pod must satisfy BOTH its disruption schedule AND any applicable PDBs to be disrupted
- If a pod has an active disruption window but the PDB blocks eviction, the pod is not disrupted
- This provides defense-in-depth for workload protection

### do-not-disrupt Annotation
- `karpenter.sh/do-not-disrupt: "true"` takes precedence over any schedule
- If both are set, `do-not-disrupt` wins (pod is never disrupted)
- Users should use one or the other, not both

## Alternative Approaches

### Alternative 1: NodePool-Level Schedule
Add a disruption schedule to the NodePool's disruption block instead of pod annotations.

```yaml
apiVersion: karpenter.sh/v1
kind: NodePool
spec:
  disruption:
    budgets:
    - schedule: "0 2 * * 6"
      duration: 4h
      nodes: "100%"
    - nodes: "0"  # Block at all other times
```

**Pros:**
- Centralized configuration for cluster administrators
- Reuses existing budget infrastructure
- No pod annotation sprawl

**Cons:**
- Less granular—applies to all pods in a NodePool
- Application teams cannot self-service their disruption windows
- May require multiple NodePools for different workload types

### Alternative 2: Custom Resource
Create a `PodDisruptionSchedule` CRD with label selectors.

```yaml
apiVersion: karpenter.sh/v1
kind: PodDisruptionSchedule
metadata:
  name: ml-training-schedule
spec:
  selector:
    matchLabels:
      app: ml-training
  schedule: "0 2 * * 6"
  duration: 4h
```

**Pros:**
- Declarative, reusable configuration
- Centralized management
- Works across namespaces

**Cons:**
- Additional CRD to manage
- More complex implementation
- Potential for selector conflicts

## Implementation

### Modified Functions

#### `pkg/utils/pod/scheduling.go`
- `IsDisruptable()` - Add check for active disruption schedule
- `IsEvictable()` - Add context parameter, check disruption schedule

#### `pkg/apis/v1/labels.go`
- Add `DisruptionScheduleAnnotationKey` constant
- Add `DisruptionScheduleDurationAnnotationKey` constant

#### `pkg/controllers/disruption/validation.go`
- Update `ShouldDisrupt()` to respect disruption schedules

### Testing Strategy
1. Unit tests for cron parsing and window evaluation
2. Integration tests for disruption schedule enforcement
3. E2E tests for schedule transitions (entering/exiting windows)
4. Edge case tests (DST transitions, leap years, invalid schedules)

## Open Questions

1. **Multiple schedules**: Should we support multiple disruption windows per pod?
2. **Metrics/Events**: What kind of metrics should be emitted when pods are protected by their schedule?
