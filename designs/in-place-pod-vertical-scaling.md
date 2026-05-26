# InPlacePodVerticalScaling Support

## Motivation

Since Kubernetes 1.27 (alpha) and 1.33 (beta), [InPlacePodVerticalScaling](https://kubernetes.io/blog/2023/05/12/in-place-pod-resize-alpha/) allows controllers to mutate a running pod's resource requests without restarting it. With Kubernetes 1.33 enabling this feature by default, controllers like VPA can now right-size pods without eviction. Karpenter must correctly account for resized pods in its disruption decisions to avoid incorrect consolidation during and after resize operations.

Karpenter's disruption logic simulates pod placement using `pod.spec.containers[].resources.requests`. With InPlacePodVerticalScaling, two correctness issues arise:

1. **During active resize**, `spec.requests` reflects the desired value while the kubelet's actual allocation (`status.allocatedResources`) may differ — either higher (resize-down in progress) or lower (resize-up in progress). Karpenter's view of node capacity is inaccurate until the resize completes.
2. **Resize activity creates churn** that triggers unnecessary consolidation evaluation on nodes where pods are still stabilizing.

For VPA's InPlace mode, VPA's admission webhook should set the replacement pod's requests to the current recommendation, which generally aligns with the pod's current spec. This makes `spec.requests` a reasonable approximation of replacement cost, and the problems above are primarily transitional.


## Background: How InPlacePodVerticalScaling Works

[KEP-1287](https://github.com/kubernetes/enhancements/tree/master/keps/sig-node/1287-in-place-update-pod-resources) introduced InPlacePodVerticalScaling, allowing pod resource requests to be mutated without restart. The feature graduated to alpha in Kubernetes 1.27 and beta in 1.33.

### The Resize Subresource

Before InPlacePodVerticalScaling, `pod.spec.containers[].resources` was immutable after creation. With the feature enabled, a controller (e.g., VPA) can patch the pod's resources via the resize subresource:

```bash
kubectl patch pod my-pod --subresource=resize --patch '{
  "spec": {"containers": [{"name": "app", "resources": {"requests": {"cpu": "2"}}}]}
}'
```

The kubelet applies the new resource values and updates `pod.status.containerStatuses[].resources` with the actual allocation. Resize status is communicated via Pod conditions:

- `PodResizePending`: The kubelet cannot immediately grant the request.
  - `reason: Infeasible` - the resize is impossible on the current node (e.g., requesting more resources than the node has).
  - `reason: Deferred` - the resize is not currently possible but may become feasible later (e.g., when another pod is removed). The kubelet retries deferred resizes periodically, prioritized by PriorityClass, then QoS class, then wait time.
- `PodResizeInProgress`: The kubelet has accepted the resize and allocated resources, but changes are still being applied. Errors during actuation are reported in the condition's message with `reason: Error`.

### VPA Update Modes

VPA supports multiple update modes that determine how recommendations are applied to pods:

| Mode | Behavior |
|------|----------|
| **Off** | VPA computes recommendations but takes no action |
| **Initial** | Admission webhook sets requests at pod creation only; no further updates |
| **Recreate** | VPA evicts pods whose requests diverge from recommendation; webhook sets new value on replacement |
| **InPlace** | VPA patches running pod's requests via resize subresource; webhook sets recommendation on replacement |
| **InPlaceOrRecreate** | InPlace with eviction fallback if resize is infeasible |

In all modes where the webhook is deployed, the replacement pod's requests are set to VPA's current recommendation at creation time.

### CPU Startup Boost

VPA can temporarily boost CPU requests at pod creation to accelerate startup (e.g., JVM warm-up), then resize back down after the pod becomes Ready. This is configured via `spec.startupBoost` on the VPA object and controlled by a `CPUStartupBoost` feature gate. The boosted value is computed dynamically (recommendation × factor, or a fixed quantity).

## How This Affects Karpenter Today

Karpenter's affected decision paths are **all disruption paths** - consolidation, drift, and expiration. All three use the same scheduling simulation that computes pod placement based on current `pod.spec.containers[].resources.requests`. Provisioning is unaffected because pending pods have their template requests (correct at creation time).

### InPlace Mode

During active resize, `spec.requests` and `status.allocatedResources` diverge. `spec.requests` reflects the desired value while `status.allocatedResources` reflects what the kubelet actually holds. This divergence can go in either direction — spec is lower during resize-down, higher during resize-up. Karpenter's view of node capacity is inaccurate until the resize completes. After resize completes, spec aligns with VPA's recommendation, so replacement cost is correctly estimated via `spec.requests`.

### CPU Startup Boost

After the boost period completes and VPA resizes down, the node may appear underutilized. If Karpenter consolidates, the replacement pod will be boosted again at creation, potentially requiring more capacity than the steady-state spec suggests. Currently, `consolidateAfter` does not reset on resize activity, so Karpenter may evaluate the node for consolidation during the boost/unboost cycle.

### Initial and Recreate Modes

In Initial and Recreate modes, VPA's recommendation evolves over time. When Karpenter evicts a pod, the replacement receives VPA's current recommendation, which may differ from the evicted pod's spec. Karpenter has no way to predict this value today.

## Goals

- Ensure accurate node capacity accounting during active pod resize across all disruption paths (consolidation, drift, expiration)
- Prevent unnecessary consolidation evaluation on nodes with active resize activity

## Non-Goals

- Predicting replacement pod requests for VPA Initial or Recreate modes, where the recommendation may differ from the current pod spec

## Proposal

### Part 1: Use Allocated Resources During Active Resize

When a pod is actively being resized down, its `spec.requests` reflects the new (lower) desired value, but `status.containerStatuses[].allocatedResources` reflects what the kubelet still physically holds. Karpenter today only reads `spec.requests`, which causes it to underestimate node utilization during the transition.

**Why this matters:**

During a resize-down transition, Karpenter's view of node capacity is inaccurate, it operates on incorrect data until the resize completes.

```
Node-A: 8 vCPU
  - Pod-X: spec.requests = 2 (resize down in progress, allocated = 6)
  Karpenter sees: used=2, free=6
  Actual (until resize completes): used=6, free=2
```

Enabling `UseStatusResources: true` ensures Karpenter always has an accurate view of what is physically committed on each node, rather than what is desired. This is good practice for correctness, consolidation decisions, scheduling simulations, and capacity metrics all benefit from reflecting reality rather than intent.

**Fix:** Enable `UseStatusResources: true` in Karpenter's resource calculation (`resources.Ceiling()`).

Today Karpenter computes pod resource usage with `UseStatusResources=false`, so `status.allocatedResources` is never consulted. Changing to `UseStatusResources: true` makes the helper return `max(spec.requests, status.allocatedResources)`, the safe value that reflects what the kubelet actually holds.

| Scenario | spec | allocated | max(spec, allocated) | Effect |
|----------|:---:|:---:|:---:|------|
| Normal (no resize) | 4 | 4 | 4 | No change |
| Resize DOWN in progress | 2 | 6 | **6** | Correctly reflects committed resources |
| Resize DOWN completed | 1 | 1 | 1 | No change |
| Resize UP in progress | 6 | 2 | 6 | Same as reading spec |
| Resize UP infeasible | 6 | 2 | 6 | Conservative (treats as needing 6) |
| Field not set (resize never occurred) | 4 | nil | 4 | Falls back to spec, backward compatible |

### Part 2: Resize Events Count for consolidateAfter

Part 1 fixes resource accounting during active resize. Part 2 prevents Karpenter from continuously evaluating a node for disruption while resize activity is ongoing.

Today, the `podevents` controller updates `nodeClaim.Status.LastPodEventTime` when pods are bound, go terminal, or go terminating. This timestamp is compared against `consolidateAfter` to determine when a node becomes consolidatable. Pod resize events do not currently trigger this.

By adding resize (change in `spec.requests`) as a pod event trigger, each resize resets the `consolidateAfter` timer. The node must be quiet (no resize activity) for the full `consolidateAfter` duration before becoming a consolidation candidate.

## Alternatives Considered

1. **Block consolidation during active resize (instead of `UseStatusResources`):** Simply skip nodes with resizing pods. Simpler but less precise, doesn't fix destination capacity calculation and blocks all consolidation on the node even when safe.

2. **Track original requests at pod creation:** Record initial requests when Karpenter first observes a pod. If requests later decrease, use the original for simulation. Doesn't require owner lookups but loses state on Karpenter restart and can't distinguish webhook-modified pods.

3. **Use template requests for displaced pod cost (`max(spec, template)`):** For displaced pods, look up the owner's pod template and use `max(pod.spec.requests, template.requests)` as the effective cost in scheduling simulation. This doesn't work well with VPA because VPA's webhook sets requests to the current recommendation at creation time, which may differ from the template. The template is not a reliable indicator of what the replacement pod will start with when VPA is involved.

## Future Work

- Upstream coordination with VPA to expose recommendations on pods (e.g., via an annotation) so that autoscalers like Karpenter can predict replacement pod requests

## References

- [InPlacePodVerticalScaling KEP-1287](https://github.com/kubernetes/enhancements/tree/master/keps/sig-node/1287-in-place-update-pod-resources)
- [Kubernetes Blog: In-Place Pod Resize Alpha](https://kubernetes.io/blog/2023/05/12/in-place-pod-resize-alpha/)
- [Issue #829: InPlacePodVerticalScaling Support](https://github.com/kubernetes-sigs/karpenter/issues/829)
