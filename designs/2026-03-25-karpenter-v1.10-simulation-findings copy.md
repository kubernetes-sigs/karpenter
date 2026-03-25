# Karpenter v1.10.0 Simulation Findings

## Context

Kamera simulation testing of Karpenter v1.10.0 (upgraded from a local dev build) revealed two ordering-sensitive behaviors in the provisioning pipeline. These were found by exploring all possible controller interleaving orders against a simple scenario: a single pending pod with one NodePool and one TestNodeClass.

An initial simulation run surfaced both issues at high frequency. After improving harness fidelity (adding an `operatorpkg` status controller stub and fixing the seed state), we re-ran the simulation to distinguish harness artifacts from genuine Karpenter behaviors. The results below reflect the higher-fidelity run.

## Finding 1: Provisioner skips NodePool due to transiently stale Ready condition

### Severity: Low (narrow race window in practice)

### What happens

The `Provisioner.NewScheduler()` filters NodePools using `np.StatusConditions().IsTrue(status.ConditionReady)`. The `Ready` root condition is computed by the `operatorpkg` status library from two dependent conditions: `ValidationSucceeded` and `NodeClassReady`. A third condition, `NodeRegistrationHealthy`, is independent — it does not gate `Ready`.

When the `nodeclaim.lifecycle` liveness controller calls `updateNodePoolRegistrationHealth()` and patches the NodePool's status conditions via `Status().Patch(MergeFromWithOptimisticLock)`, there is a brief window before the `operatorpkg` status controller recomputes the `Ready` root. If the provisioner runs during this window, it sees `Ready != True`, logs `"ignoring nodepool, not ready"`, and returns `ErrNodePoolsNotFound`.

### Simulation results

In the initial (low-fidelity) run without the status controller stub, this error appeared in every scenario variant at multiple depths — it was the dominant behavior. After adding the status controller stub to the harness, the error appeared only once across all scenario variants, at depth 38 in a specific interleaving where the lifecycle controller patches status between the status reconciler and provisioner runs. This confirms the issue is real but narrow: it requires a specific three-way ordering (lifecycle patch → provisioner check → status recompute) within a single reconcile cycle.

### Existing protections

- `cluster.Synced()` blocks scheduling until internal state matches the API server
- The provisioner runs as a singleton with a batcher, giving informers time to sync
- `Ready` depends only on `ValidationSucceeded` and `NodeClassReady`, not `NodeRegistrationHealthy`
- The `operatorpkg` status controller continuously recomputes `Ready`, keeping the staleness window small

### How it could get worse

- **Single NodePool clusters**: If the only NodePool appears not-ready, all provisioning halts until the next cycle. Multi-NodePool setups are resilient since other pools remain schedulable.
- **High NodeClaim churn**: Short `consolidateAfter` values or frequent launch failures increase `updateNodePoolRegistrationHealth` call frequency, widening the window.
- **Slow status controller**: Under very high QPS the status controller could fall behind, though in practice its reconcile is lightweight (condition recomputation only).

### Suggested change

Check the individual dependent conditions directly instead of the computed root:

```go
// Before (current)
if !np.StatusConditions().IsTrue(status.ConditionReady) {
    log.Error(err, "ignoring nodepool, not ready")
    return false
}

// After (proposed)
conditions := np.StatusConditions()
if !conditions.IsTrue(v1.ConditionTypeValidationSucceeded) ||
    !conditions.IsTrue(v1.ConditionTypeNodeClassReady) {
    log.WithValues("NodePool", klog.KObj(np)).
        Info("ignoring nodepool, dependent conditions not met",
            "ValidationSucceeded", conditions.Get(v1.ConditionTypeValidationSucceeded).GetStatus(),
            "NodeClassReady", conditions.Get(v1.ConditionTypeNodeClassReady).GetStatus())
    return false
}
```

### Trade-offs

- **Pro**: Eliminates the race entirely. Provisioning continues as long as the actual dependencies (validation, nodeclass) are met, regardless of root condition staleness from unrelated status patches.
- **Pro**: The log message becomes more actionable — it tells you which specific condition failed rather than just "not ready."
- **Con**: Bypasses the `operatorpkg` condition aggregation pattern. If a new dependent condition is added to `StatusConditions()` in the future, this check would need to be updated manually.
- **Con**: Minor divergence from the convention of checking the root condition. Other consumers of NodePool readiness would still use `IsTrue(ConditionReady)`.
- **Alternative**: Ensure the status patch in `updateNodePoolRegistrationHealth` preserves the `Ready` root condition by recomputing it inline before patching. This keeps the provisioner's check unchanged but adds complexity to every status patch site.

### Assessment

Given the narrow race window confirmed by simulation, this is a low-priority hardening change. The current behavior self-heals on the next provisioner cycle (typically within seconds). The suggested change is worth considering as defense-in-depth, especially for single-NodePool deployments.

## Finding 2: Double-provisioning of the same pod

### Severity: Medium (confirmed real behavior, causes temporary over-provisioning)

### What happens

The provisioner creates a NodeClaim for a pending pod at depth 2. The NodeClaim enters the lifecycle pipeline (launch, registration). The provisioner's singleton ticker fires again at depth 20. The pod is still `Unschedulable` with `spec.nodeName=""` because the NodeClaim hasn't finished launching and the pod hasn't been bound yet. `GetProvisionablePods()` returns the same pod. The provisioner creates a second NodeClaim for it.

The first NodeClaim eventually gets cleaned up by the liveness controller, but the cluster has temporarily over-provisioned — two nodes launched for one pod.

### Simulation results

This behavior reproduced consistently across both the low-fidelity and high-fidelity harness runs, confirming it is a genuine Karpenter behavior and not a harness artifact. In every scenario variant that reached sufficient depth, the provisioner created a second NodeClaim (`default-00002`, `default-00004`, etc.) for the same pending pod that already had an in-flight NodeClaim. The first NodeClaim was subsequently deleted at depth 31 by the liveness controller.

### Existing protections

- `GetProvisionablePods` filters for `PodScheduled=False, Reason=Unschedulable` and `nodeName=""`
- `cluster.Synced()` blocks until all NodeClaims have provider IDs
- `cluster.UpdateNodeClaim()` is called immediately after `Create()` to update internal state
- `cluster.UpdatePodToNodeClaimMapping()` tracks pod-to-NodeClaim associations
- The scheduler receives `stateNodes` which include in-flight NodeClaims

The gap: `UpdatePodToNodeClaimMapping` records the association, but `GetPendingPods` does not consult it. The pod remains "provisionable" as long as it has no `nodeName`, regardless of whether a NodeClaim already exists for it.

### How it gets worse

- **Larger clusters**: More pods trigger the batcher more frequently; cloud provider launch latency is variable (spot instances can take 30-90s), widening the window between NodeClaim creation and pod binding
- **Capacity crunches**: Launch failures cause the liveness controller to delete NodeClaims after 5min (launch timeout) or 15min (registration timeout), during which the pod remains pending and can be re-provisioned repeatedly
- **Interaction with Finding 1**: If the provisioner skips a cycle due to stale Ready, pods batch up. When the NodePool comes back, the provisioner schedules all batched pods at once, increasing the chance of duplicates for pods that already have in-flight NodeClaims

### Suggested change

Add a pod-to-NodeClaim check in `GetPendingPods`:

```go
func (p *Provisioner) GetPendingPods(ctx context.Context) ([]*corev1.Pod, error) {
    pods, err := nodeutils.GetProvisionablePods(ctx, p.kubeClient)
    if err != nil {
        return nil, fmt.Errorf("listing pods, %w", err)
    }
    // Filter out pods that already have an in-flight NodeClaim
    pods = lo.Filter(pods, func(po *corev1.Pod, _ int) bool {
        if p.cluster.HasPendingNodeClaimForPod(client.ObjectKeyFromObject(po)) {
            log.FromContext(ctx).WithValues("Pod", klog.KObj(po)).
                V(1).Info("skipping pod, already has in-flight NodeClaim")
            return false
        }
        return true
    })
    // ... rest of existing filtering
}
```

This requires exposing a `HasPendingNodeClaimForPod` method on `Cluster` that checks the `podToNodeClaim` sync.Map.

### Trade-offs

- **Pro**: Directly prevents double-provisioning regardless of controller ordering or timing.
- **Pro**: Uses existing `podToNodeClaim` tracking — no new state needed.
- **Con**: The `podToNodeClaim` mapping is populated in `CreateNodeClaims` after the NodeClaim is created. If the provisioner crashes between creating the NodeClaim and updating the mapping, the protection is lost for that cycle. However, the next cycle would pick it up via the state informer.
- **Con**: If a NodeClaim is deleted (e.g., by the liveness controller due to launch timeout) but the `podToNodeClaim` mapping isn't cleaned up promptly, the pod could be incorrectly skipped. The mapping cleanup would need to be tied to NodeClaim deletion events in `DeleteNodeClaim`.
- **Alternative**: Make the scheduler's existing in-flight NodeClaim awareness stronger by having it explicitly check whether each pending pod is already nominated to an existing NodeClaim. This is closer to the current architecture but requires the nomination to be durable (currently it's event-based via `RecordPodNomination`).

### Assessment

This is the higher-priority finding. Double-provisioning wastes cloud resources and can compound under load. The fix is straightforward — the `podToNodeClaim` mapping already exists and just needs to be consulted during pod filtering. The cleanup concern (stale mappings after NodeClaim deletion) is manageable by adding a `podToNodeClaim.Delete` call in the existing `DeleteNodeClaim` path.

## Simulation methodology

These findings were produced by Kamera's in-memory model checker, which wires up the real Karpenter v1.10.0 controller code against a fake `client.Client` and systematically explores all possible orderings of controller reconcile calls. No real Kubernetes cluster is needed.

The harness registers 13 reconcilers covering the provisioner, state informers (pod, node, nodepool, nodeclaim), NodeClaim lifecycle/hydration/consistency, node hydration, a node registrar shim, and an `operatorpkg` status controller stub. The exploration used DFS with a max depth of 50 steps per scenario, with 12 fuzzed parameter variants per input.

Initial results were validated by improving harness fidelity and re-running: behaviors that disappeared after the fix were classified as harness artifacts, while behaviors that persisted were classified as genuine Karpenter behaviors.
