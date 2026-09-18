# InPlacePodVerticalScaling Support

## Motivation

Since Kubernetes 1.27 (alpha) and 1.33 (beta), [InPlacePodVerticalScaling](https://kubernetes.io/blog/2023/05/12/in-place-pod-resize-alpha/) allows controllers to mutate a running pod's resource requests without restarting it. With Kubernetes 1.33 enabling this feature by default, controllers like VPA can now right-size pods without eviction. Karpenter must correctly account for resized pods in its disruption decisions to avoid incorrect consolidation during and after resize operations.

Karpenter's disruption logic simulates pod placement using `pod.spec.containers[].resources.requests`. With InPlacePodVerticalScaling, two correctness issues arise:

1. **During active resize**, `spec.requests` reflects the desired value while the kubelet's actual allocation (`status.allocatedResources`) may differ. Karpenter's scheduling simulation diverges from the kube-scheduler's view, causing consolidation decisions the scheduler can't fulfill.
2. **After eviction**, a mutating webhook (e.g., VPA) may set different resource requests on the recreated pod than what the evicted pod had. Karpenter has no way to predict this, so it may evict pods expecting them to land on a replacement node that can't actually fit the recreated pod's larger requests.



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

During active resize, `spec.requests` and `status.allocatedResources` diverge. `spec.requests` reflects the desired value while `status.allocatedResources` reflects what the kubelet actually holds. This divergence can go in either direction, spec is lower during resize-down, higher during resize-up. Karpenter's view of node capacity is inaccurate until the resize completes. After resize completes, spec aligns with VPA's recommendation, so replacement cost is correctly estimated via `spec.requests`.

### CPU Startup Boost

After the boost period completes and VPA resizes down, the node may appear underutilized. If Karpenter consolidates, the replacement pod will be boosted again at creation, requiring more capacity than the steady-state spec suggests. Without prediction, Karpenter bases its consolidation decision on the current (post-boost) spec and may move the pod to a node that can't fit the boosted value on recreation.

### Initial and Recreate Modes

In Initial and Recreate modes, VPA's admission webhook sets pod requests to the current recommendation at creation time. Since recommendations evolve continuously, when Karpenter evicts a pod the replacement may receive different resource requests than the evicted pod had. Karpenter has no way to predict this value today.

## Goals

- Ensure accurate node capacity accounting during active pod resize across all disruption paths (consolidation, drift, expiration)
- Predict post-eviction pod resource requests so that consolidation decisions account for mutating webhooks (e.g., VPA) that may change a pod's resources on recreation

## Proposal

### Part 1: Use Allocated Resources During Active Resize

When a pod is actively being resized down, its `spec.requests` reflects the new (lower) desired value, but `status.containerStatuses[].allocatedResources` reflects what the kubelet still physically holds. Karpenter today only reads `spec.requests`, which causes it to underestimate node utilization during the transition.

**Why this matters:**

During a resize-down transition, Karpenter's scheduling simulation diverges from the kube-scheduler's view. The kube-scheduler uses `UseStatusResources` to account for what pods actually hold, while Karpenter (without this change) only reads `spec.requests`. This divergence causes Karpenter to make consolidation decisions the scheduler can't fulfill, Karpenter thinks a target node has room, moves pods there, but the scheduler sees the node is fuller than Karpenter thought and can't place the incoming pods. The result is unnecessary pod disruption and temporarily pending pods.

```
Node-B (target): 8 vCPU
  - Pod-Z: spec.requests = 2 (resize down in progress, allocated = 6)
  Karpenter sees: used=2, free=6
  Scheduler sees: used=6, free=2
  Karpenter moves pods expecting 6 free → scheduler rejects → pods pending
```

Enabling `UseStatusResources: true` aligns Karpenter's resource accounting with the kube-scheduler, ensuring consolidation decisions are compatible with actual pod placement.

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

### Part 2: Predict Post-Eviction Resource Requests via VPA Recommendation

Part 1 fixes resource accounting during active resize. Part 2 ensures consolidation decisions account for how pods will look after recreation.

When Karpenter consolidates a node, it evicts pods that are then recreated by their controllers. If a mutating webhook (e.g., VPA's admission controller) changes the pod's resource requests on creation, the recreated pod may require more or less resources than the original. Without prediction, Karpenter might move pods to a node that can't fit them post-recreation, causing unnecessary pending pods.

**Mechanism:**

Karpenter watches `VerticalPodAutoscaler` objects and reads `status.recommendation.containerRecommendations[].target`, the per-container recommended requests.

**Discovery:**

The VPA object itself signals which pods are managed:
1. Read `spec.targetRef` (e.g., kind: Deployment, name: my-app)
2. Only consider VPAs where `spec.updatePolicy.updateMode` != Off (webhook won't mutate if Off)
3. Resolve: VPA → Deployment → ReplicaSets → Pods

**Flow:**

1. A background controller watches VPA objects. For each VPA with updateMode != Off, it reads `status.recommendation.containerRecommendations[]` and picks a representative pod from the targeted workload.
2. For each container in the pod: if the container appears in the recommendation and is a controlled resource (per `spec.resourcePolicy.containerPolicies[].controlledResources`), use the `target` value. Otherwise, use that container's current requests from the pod spec.
3. Compute `Ceiling()` from the merged result. Store in cache keyed by `(namespace, owner-uid)`.
4. During consolidation simulation, VPA-targeted pods use their cached predicted resources. Non-targeted pods use current resources as today.
5. Before eviction, re-read VPA recommendation fresh. If it differs from what the simulation assumed, abort and re-evaluate on the next cycle.
6. Cache is refreshed via watch on VPA status updates (no TTL polling needed). Pod spec changes (deployment updates) also trigger recomputation.

**RBAC:**

Requires `get`, `list`, `watch` on `verticalpodautoscalers.autoscaling.k8s.io`.

**Startup boost:**

VPA's `status.recommendation.target` is the steady-state recommendation. Startup boost is configured separately in `spec.startupBoost` and computed dynamically by the webhook at creation time (`target × factor`). The boosted value is not stored in VPA status. To predict it, Karpenter would need to read the boost configuration and replicate the computation.

**Pros:**

- No `create pods` RBAC
- No annotation needed (VPA object is the signal)
- Immediate reaction to recommendation changes via watch

**Cons:**

- Couples Karpenter to VPA's CRD schema
- Only works for VPA, other resource-mutating webhooks are not covered
- Replicates webhook logic: `controlledResources`, `updateMode`, startup boost computation, container name matching and merging


## Alternatives Considered

### 1. Dry-Run Pod Creates

A prediction cache runs dry-run pod creates dry-run pod creates (`POST /api/v1/namespaces/{ns}/pods?dryRun=All`) to determine what a pod's resources would be after recreation. This goes through the full API server admission chain (all mutating webhooks) without persisting anything.

**Opt-in via annotation:**

Pods (or their templates) must be annotated with `autoscaling.k8s.io/volatile-requests: "true"` to signal that their requests may change on recreation. This annotation is set by the vertical pod autoscaler (or any autoscaler that uses a mutating webhook to adjust resources at pod creation). Karpenter only runs dry-runs for annotated pods, avoiding unnecessary API calls for workloads whose requests are static.

**Flow:**

1. A background controller lists pods with the `volatile-requests` annotation, groups them by owner (one representative per ReplicaSet), strips runtime fields, and submits dry-run creates. Pods without an owner are skipped, they won't be recreated after eviction.
2. The predicted `Ceiling()` result is cached, keyed by `(namespace, owner-uid)`.
3. During consolidation simulation, annotated pods use their cached predicted resources instead of current `Ceiling(pod)`. Non-annotated pods use current resources as today.
4. During consolidation validation, Karpenter re-runs a fresh dry-run for annotated pods in the plan. If the result differs from what the simulation assumed, the decision is rejected.
5. The cache refreshes periodically (e.g., every 60s). On startup, consolidation is gated until the first pass completes (consistent with how other caches like instance types and cluster state gate disruption decisions). If no annotated pods exist, this completes immediately.

**Pros:**

- Autoscaler-agnostic: works with any mutating webhook (VPA, custom controllers, sidecar injectors, etc.)
- No coupling to specific CRDs or autoscaler implementations

**Cons:**

- Requires `create pods` RBAC cluster-wide. RBAC does not distinguish dry-run from real creates.
- During active rollouts (Deployment or StatefulSet), the recreated pod may use a newer template than what was dry-run'd. This can cause one incorrect consolidation where the pod doesn't fit on the target node. The pod goes pending until Karpenter provisions a new node. The cache self-corrects on the next refresh once the recreated pod is running with the new template.


### 2. Annotation Carrying the Prediction

The vertical pod autoscaler annotates workloads with the total predicted pod-level resource requests, the Ceiling equivalent of what its webhook would produce on a new pod. Karpenter reads this annotation during consolidation simulation.

**Annotation:**

```
autoscaling.k8s.io/predicted-pod-requests: '{"cpu": "8500m", "memory": "2304Mi"}'
```

The autoscaler computes this by taking its recommendation for managed containers (with startup boost applied if configured), current spec for unmanaged containers, and computing the total using standard Ceiling logic.

**Where the annotation lives:**

Two variants:

1. **On the owner (Deployment/StatefulSet)** (recommended): VPA patches `metadata.annotations` on the Deployment. One patch per workload per recommendation change. Karpenter resolves pod → RS → Deployment → read annotation.
2. **On live pods**: VPA patches the annotation on every managed pod whenever the recommendation changes. Simpler resolution (annotation is on the pod directly) but scales poorly, N patches per recommendation change.

**Flow:**

1. VPA computes the predicted post-eviction resources and patches the annotation on the owner (or pods).
2. Karpenter reads the annotation during consolidation simulation. If present, uses it as the predicted resources for pods being moved. If absent, uses current `Ceiling(pod)`.
3. Before eviction, Karpenter re-reads the annotation. If it changed since simulation, abort and re-evaluate.

**RBAC:**

No additional RBAC for Karpenter (already reads pods and watches Deployments). VPA needs patch on Deployments/StatefulSets (if annotating the owner).

**Pros:**

- No additional RBAC for Karpenter
- Autoscaler-agnostic (any controller can adopt the convention)
- Simple read path for Karpenter (just read annotation)
- No coupling to specific CRDs

**Cons:**

- Schema must be agreed upon across projects
- If annotating live pods (variant 2): API load at scale (N patches per recommendation change)


### 3. Call Webhook Endpoints Directly

Discover `MutatingWebhookConfiguration` resources, construct `AdmissionReview` requests, and call webhook services. Gets exact results without pod create RBAC but is extremely complex (auth, ordering, chaining, network access) and fragile. Essentially re-implements the API server's admission chain.

### 4. Delay or Block Consolidation During Resize

Delay consolidation after a resize event (e.g., reset `consolidateAfter`) or skip nodes with active resizes entirely. Simpler but doesn't solve the core problem (incorrect prediction of post-eviction size), doesn't fix destination capacity calculation, and blocks consolidation on a node even when safe. With accurate predictions, consolidation can safely proceed without artificial delays.

## Graduation Criteria

The VPA prediction mechanism (Part 2) starts with reading VPA CRD objects directly. This is the correct starting point regardless of which long-term mechanism materializes, because it requires no upstream changes and works with all existing VPA installations. Karpenter only produces predictions when VPA objects exist in the cluster; if VPA is not installed or no VPAs are configured, there is no effect on Karpenter's behavior.

The long-term graduation path depends on which upstream mechanism is adopted:

### Path A: VPA-Independent Mechanism

If a VPA-independent prediction mechanism becomes available (e.g., dry-run pod creates with dedicated RBAC), Karpenter can switch without requiring any changes to VPA. The existing admission webhook already handles dry-run requests, so VPA doesn't need to ship a new API.

| | Mechanism | When | Customer Action |
|--|-----------|------|-----------------|
| Initial | CRD reading | Until the new mechanism is available in K8s | None |
| Dual support | Both (runtime K8s version check) | While Karpenter supports K8s versions with and without the new mechanism | None |
| CRD reading dropped | New mechanism only | Once Karpenter's minimum supported K8s version includes the new mechanism | Upgrade cluster to meet Karpenter's minimum K8s version |

No VPA coordination required. The transition is driven entirely by K8s version support.

### Path B: VPA-Dependent Mechanism

If the prediction mechanism requires VPA to ship a new API, the transition requires coordination with the customer's VPA version.

| | Mechanism | When | Customer Action |
|--|-----------|------|-----------------|
| Initial | CRD reading | Until VPA ships a new prediction API | None |
| Dual support | Both (prefers new API, falls back to CRD reading) | Karpenter versions that support both old and new VPA | None (auto-detected) |
| CRD reading dropped | New API only | Karpenter version that removes CRD reading | Upgrade VPA before upgrading Karpenter |

Requires VPA to ship the new API, which is outside our control. CRD reading is dropped at a documented Karpenter version boundary: customers must upgrade VPA before upgrading Karpenter past that version.


### Notes

- CRD reading has near-zero maintenance cost (GA API, stable fields, read-only). We retain the option to keep it indefinitely if neither path materializes.


## References

- [InPlacePodVerticalScaling KEP-1287](https://github.com/kubernetes/enhancements/tree/master/keps/sig-node/1287-in-place-update-pod-resources)
- [Kubernetes Blog: In-Place Pod Resize Alpha](https://kubernetes.io/blog/2023/05/12/in-place-pod-resize-alpha/)
- [Issue #829: InPlacePodVerticalScaling Support](https://github.com/kubernetes-sigs/karpenter/issues/829)
