# Rollout-Restart Drain Strategy

## Summary

This feature adds an opt-in drain strategy that, for singleton (`spec.replicas: 1`) Deployments and StatefulSets, replaces the pod-eviction call during node drain with a `kubectl rollout restart`
–equivalent patch on the workload's pod template. The behavior is gated behind a new `RolloutRestartDrainStrategy` feature gate (default `false`) and only applies to pods whose owner is an apps/v1
Deployment or StatefulSet with `spec.replicas == 1`. All other pods continue to drain via the existing eviction path.

## Motivation

### Problem Statement

Karpenter drains nodes by calling the [eviction subresource](https://kubernetes.io/docs/concepts/scheduling-eviction/api-eviction/) on each non-DaemonSet pod. Eviction is the right primitive for the
multi-replica case — it respects `PodDisruptionBudget` (PDB), so Kubernetes can guarantee a minimum of available replicas during the drain. For singleton workloads, however, eviction has two
well-known shortcomings:

1. **PDB deadlock on singletons.** A common policy is to set `minAvailable: 1` (or `maxUnavailable: 0`) on every workload's PDB so that disruption tooling can never voluntarily take the workload below
   one Ready replica. For a Deployment or StatefulSet with `spec.replicas: 1`, that policy is *self-conflicting under eviction*: the eviction API will refuse the request indefinitely (HTTP 429,
   `Cannot evict pod as it would violate the pod's disruption budget`). Karpenter retries forever, the node never drains, and the only way out is either to forcibly delete the pod (via
   `terminationGracePeriod` expiry) — losing the PDB guarantee anyway — or for an operator to manually `kubectl rollout restart` the workload.

2. **Avoidable downtime on Deployments.** Even when no PDB blocks eviction, eviction on a singleton Deployment causes a full pod-lifetime of downtime: the old pod is deleted, then the Deployment
   controller creates a new pod, the scheduler places it, the kubelet pulls the image, and the application boots. For applications with multi-minute startup times (JVM apps such as Spring Boot are a
   common case, with 2+ minute boots), this is a multi-minute outage *every consolidation cycle*. A `kubectl rollout restart` on the same Deployment, by contrast, uses `maxSurge` to bring the
   replacement Ready *before* deleting the old pod — zero-downtime by design.

In both cases the operator's manual remediation is the same: trigger a rollout restart of the singleton's owner. Karpenter has the same information the operator has (the pod's owner chain, the owner's
replica count, the owner's rollout status) and is in a position to do the same thing automatically.

### Why This Feature Belongs in Karpenter

The drain step is owned by Karpenter — it is the component holding the node and deciding which API to call to remove each pod. Eviction is currently the only option in that step, and "drain a
singleton without downtime / without PDB deadlock" is a recurring failure mode of that single option:

- It is **not solvable at the workload layer** without giving up the PDB guarantee that platform teams want everywhere. The strict-PDB-everywhere policy is desirable; the singleton-deadlock is a
  side effect.
- It is **not solvable by PDB tuning** in a way that scales — relaxing PDB on singletons means writing per-workload PDBs, which is precisely what platform teams use admission policies to *prevent*.
- It is **not solvable by `terminationGracePeriod`** alone — that resolves the deadlock by forcefully terminating the pod after a timeout, which is exactly the outage the user is trying to avoid.

Rollout-restart-as-drain takes a primitive every Kubernetes user already understands (`kubectl rollout restart`) and uses it at the right layer (the node-drain step) for the case where eviction is
structurally a poor fit. Because the behavior is gated and scoped narrowly (singleton apps/v1 owners only), it does not change Karpenter's drain semantics for any workload that is not already failing
under the status quo.

### Use Cases

1. **Singleton service with strict PDB.** A Deployment with `replicas: 1` and a PDB `minAvailable: 1` (or any equivalent zero-disruption policy). Today: Karpenter is stuck draining the node forever,
   or `terminationGracePeriod` eventually force-kills the pod with downtime. With this feature: Karpenter patches the Deployment, `maxSurge` brings the replacement Ready, the old pod is gracefully
   removed, the node drains.

2. **Singleton with slow boot.** A Deployment with `replicas: 1`, no strict PDB, but a multi-minute application startup (JVM, ML model load, etc.). Today: every consolidation cycle causes a full-boot
   outage. With this feature: surge keeps the service Ready throughout.

3. **Singleton StatefulSet.** A StatefulSet with `replicas: 1` backing a piece of stateful infrastructure (a leader process, a Postgres primary, a singleton broker) behind a strict PDB. Today:
   eviction is refused, drain stalls. With this feature: the StatefulSet controller is asked to recreate the pod via its normal rollout path. StatefulSets cannot surge, so this is not zero-downtime —
   it is, however, the same amount of downtime a successful eviction would have caused, and crucially it *completes*, which eviction does not.

### Non-Goals

- **Multi-replica workloads.** Eviction + PDB is the correct primitive there, and rolling every replica of a multi-replica Deployment just to drain one node would be both wasteful and surprising. The
  feature deliberately gates on `spec.replicas == 1`.
- **Workloads with no rollout-capable owner.** Bare Pods, Jobs, custom controllers, etc. fall through to eviction unchanged.
- **Replacing the existing eviction path.** This is an additional path taken for one narrow case. The default behavior of Karpenter does not change when the feature gate is `false`.

## Proposal

### Feature Gate

A new feature gate is added to the existing `--feature-gates` flag / `FEATURE_GATES` env var:

```
--feature-gates=...,RolloutRestartDrainStrategy=true
```

- Default: `false`.
- When `false`, drain behavior is unchanged.
- When `true`, the drain queue applies the rollout-restart path described below before falling through to eviction.

### Behavior

When the queue picks up a pod to drain and the feature gate is on, Karpenter performs the following check before the existing eviction call:

1. **Identify a singleton rollout target.** Walk the pod's `ownerReferences`:
    - If the pod is directly owned by an apps/v1 `StatefulSet` and that StatefulSet has `spec.replicas == 1`, the target is that StatefulSet.
    - Otherwise, if the pod is owned by an apps/v1 `ReplicaSet` whose owner is an apps/v1 `Deployment` with `spec.replicas == 1`, the target is that Deployment.
    - Otherwise, there is no rollout target — fall through to eviction.

2. **Gate on the owner's rollout status.** If the target already has a rollout in progress (defined below), Karpenter does *not* patch — patching now would create a third revision and cause the
   in-flight replacement pod to be killed before it becomes Ready. Karpenter emits a `RolloutRestartInProgress` event on the pod, drops the pod from the in-memory drain queue, and lets the next
   reconcile pass re-examine the situation. This is the critical fix: a naive "patch every time we see this pod" implementation creates an infinite deploy loop on any workload whose boot time exceeds
   Karpenter's consolidation cadence.

3. **Trigger the rollout restart.** If no rollout is in progress, Karpenter issues a strategic-merge patch on the target's `spec.template.metadata.annotations` setting
   `kubectl.kubernetes.io/restartedAt` to the current RFC3339 timestamp — identical to what `kubectl rollout restart deployment/<name>` writes. The Deployment / StatefulSet controller then drives the
   rollout to completion as it would for any other restart. Karpenter emits a `RolloutRestarted` event on the pod and removes the pod from the drain queue; the pod will be deleted by the workload
   controller as part of the rollout, at which point the node-drain controller's next pass observes one fewer pod on the node.

4. **Fall through.** If no singleton rollout target exists, or if the target lookup hits a transient API error, the existing eviction code path runs.

### Defining "rollout in progress"

The in-flight gate mirrors `kubectl rollout status`. For each kind:

**Deployment** — a rollout is in progress if any of the following hold:

- `status.observedGeneration < metadata.generation` (the controller hasn't yet reconciled the latest spec)
- `status.updatedReplicas < spec.replicas` (the new ReplicaSet hasn't reached desired count)
- `status.replicas > status.updatedReplicas` (the **surge-cleanup window**: the new pod is up, possibly Ready, but the old pod hasn't been deleted yet)
- `status.availableReplicas < status.updatedReplicas` (new pods exist but aren't Available yet)

The `status.replicas > status.updatedReplicas` check is essential and is the reason a status-based gate was chosen over a timestamp-based one. During surge, `UpdatedReplicas` reaches the desired count
*before* the old pod is removed; without this clause the gate would report "rollout done" while the old pod still exists on the draining node, and Karpenter would patch again, starting a new rollout
that kills the not-yet-Ready surge pod. This is the failure mode observed in development against slow-booting workloads and is what the second commit in this series fixes.

**StatefulSet** — a rollout is in progress if any of:

- `status.observedGeneration < metadata.generation`
- `status.updatedReplicas < spec.replicas`
- `status.readyReplicas < spec.replicas`
- `status.currentRevision != status.updateRevision`

StatefulSets do not surge, so there is no surge-cleanup clause. `rollingUpdate.partition` is intentionally **not** special-cased — a partitioned rollout on a singleton is degenerate (any non-zero
partition means "do nothing"), and adding it as a gate would cause a partial rollout to look "in progress" forever.

### Events

Two new event reasons are added on the pod:

- `RolloutRestarted` — emitted when Karpenter has patched the owner.
- `RolloutRestartInProgress` — emitted when Karpenter observed an in-flight rollout and intentionally skipped patching. This is the diagnostic signal for "why isn't this node draining yet?" on
  slow-booting singletons.

Both are `Normal` events and use the pod name for dedupe so the same singleton on the same drain doesn't spam the event stream.

### Implementation Outline

```
pkg/operator/options/options.go
  + FeatureGates.RolloutRestartDrainStrategy bool (default false)
  + parse + default in DefaultFeatureGates / ParseFeatureGates

pkg/utils/pod/scheduling.go
  + type SingletonRolloutTarget { Object, Kind, RolloutInProgress }
  + SingletonRolloutTargetForPod(ctx, client, pod) — owner-chain walk + replica gate
  + deploymentRolloutInProgress / statefulSetRolloutInProgress — Status-based gates

pkg/controllers/node/termination/terminator/eviction.go
  + Queue.tryRolloutRestart(ctx, pod) — called before eviction.Create when gate is on
  + Queue.rolloutRestart(ctx, target) — strategic-merge patch of restartedAt

pkg/controllers/node/termination/terminator/events/events.go
  + RolloutRestartedPod / RolloutRestartInProgress event constructors

pkg/events/reason.go
  + Reason constants: RolloutRestarted, RolloutRestartInProgress
```

The change is self-contained inside the eviction queue. No NodeClaim, NodePool, or scheduling code is touched. The existing eviction code path is reached unchanged whenever `tryRolloutRestart` returns
`(false, nil)`.

## Examples

### Example 1: Singleton Deployment with strict PDB (zero-downtime)

```yaml
apiVersion: apps/v1
kind: Deployment
metadata:
  name: payments-api
spec:
  replicas: 1
  strategy:
    type: RollingUpdate
    rollingUpdate:
      maxSurge: 1
      maxUnavailable: 0
  template:
    spec:
      containers: [ ... ]
---
apiVersion: policy/v1
kind: PodDisruptionBudget
metadata:
  name: payments-api
spec:
  minAvailable: 1
  selector:
    matchLabels:
      app: payments-api
```

**Today (eviction):** Drain calls eviction → 429 (`would violate disruption budget`). Karpenter retries forever. The node never drains until `terminationGracePeriod` fires and force-deletes the pod.

**With `RolloutRestartDrainStrategy=true`:** Drain patches the Deployment's `restartedAt` annotation. The Deployment controller surges a new pod onto another node, the new pod becomes Ready, the old
pod is gracefully deleted by the Deployment controller. The PDB invariant `minAvailable: 1` is preserved throughout. The node drains.

### Example 2: Singleton Deployment with slow boot (no PDB conflict, but avoidable outage)

```yaml
apiVersion: apps/v1
kind: Deployment
metadata:
  name: spring-boot-singleton
spec:
  replicas: 1
  strategy:
    rollingUpdate:
      maxSurge: 1
      maxUnavailable: 0
  # No PDB on this workload.
```

**Today:** Eviction succeeds immediately, pod is deleted, ~2 minutes of downtime while the JVM starts on the replacement.

**With the feature on:** Same surge behavior as Example 1 — zero observed downtime.

### Example 3: Singleton StatefulSet behind a strict PDB

```yaml
apiVersion: apps/v1
kind: StatefulSet
metadata:
  name: leader
spec:
  replicas: 1
  # ...
---
apiVersion: policy/v1
kind: PodDisruptionBudget
metadata:
  name: leader
spec:
  minAvailable: 1
  selector:
    matchLabels:
      app: leader
```

**Today:** Eviction is refused. Drain stalls until `terminationGracePeriod`.

**With the feature on:** Karpenter patches the StatefulSet's `restartedAt`. The StatefulSet controller deletes the pod and recreates it (StatefulSets do not surge). Downtime = one pod lifetime — the
same downtime a successful eviction would have caused, but the drain *completes*.

### Example 4: Slow-booting singleton, mid-rollout (in-flight gate)

A second drain pass observes the same pod while the rollout from the first pass is still surging. Karpenter sees `status.replicas (2) > status.updatedReplicas (1)` — the new pod is Ready but the old
one is still being torn down — and emits `RolloutRestartInProgress` instead of patching. Without this gate, the second patch would create a third revision and kill the not-yet-fully-promoted
replacement, leading to an infinite restart loop. **This scenario is the reason the original timestamp-based dedup was replaced with a Status-based gate.**

### Example 5: Pod is not a singleton (no behavior change)

```yaml
apiVersion: apps/v1
kind: Deployment
metadata:
  name: api
spec:
  replicas: 5
```

`SingletonRolloutTargetForPod` returns `nil` because `spec.replicas != 1`. The drain falls through to the existing eviction path. The feature gate being on has no effect on this workload.

## Interaction with Other Features

### PodDisruptionBudget

The rollout-restart path **does not call eviction**, so it is not subject to PDB. This is intentional: the entire point of the feature is to drain workloads that the workload owner has declared (via
strict PDB) "should never go below one Ready replica," and the rollout primitive is the *only* mechanism that can honor that invariant for a singleton.

For Deployments, the surge behavior preserves the spirit of the PDB during the drain — the new pod is Ready before the old one goes away. For StatefulSets, the PDB invariant is briefly violated (one
pod-lifetime of downtime), exactly as it would be in a normal `kubectl rollout restart`. Operators who consider this unacceptable should leave the feature gate off, and accept that singleton
StatefulSets behind strict PDBs are not drainable without an admin override.

### `terminationGracePeriod`

This feature is complementary to NodeClaim `spec.terminationGracePeriod`:

| Mechanism                     | When it acts                                  | What it does                                                     |
|-------------------------------|-----------------------------------------------|------------------------------------------------------------------|
| `RolloutRestartDrainStrategy` | Drain-time, only for singleton apps/v1 owners | Surge / rollout the workload owner to gracefully replace the pod |
| `terminationGracePeriod`      | Drain-time, all pods, after timer expires     | Force-delete blocking pods so the node terminates                |

A node with both configured will, in the common case, drain via rollout restart well before the termination grace period fires. The termination grace period remains the safety net for cases the
rollout-restart path can't handle (workloads whose owners aren't singleton apps/v1, or whose rollouts themselves get stuck).

### `do-not-disrupt` (pod-level)

`do-not-disrupt` is evaluated upstream of the drain queue — a pod with active `do-not-disrupt` protection is not put into the drain queue, so this feature never sees it. No interaction.

### Pod `terminationGracePeriodSeconds`

The pod's `terminationGracePeriodSeconds` continues to govern how the old pod shuts down (the kubelet honors it during the Deployment/StatefulSet controller's deletion of the pod, just as it does for
`kubectl rollout restart`). This feature only changes *how* the deletion is initiated, not *how* it completes.

### Eviction metrics

`PodsEvictionRequestsTotal` is not incremented on the rollout-restart path because no eviction API call is made. `PodsDrainedTotal` should be updated by a follow-up to count rollout-driven drains
alongside eviction-driven drains (see Future Enhancements).

## Alternatives Considered

### Alternative 1: Leave it to operators

Document the singleton-strict-PDB failure mode and tell operators to either relax their PDB or pre-emptively `kubectl rollout restart` workloads before draining.

**Rejected**: this is the status quo, and the status quo is what motivated this RFC. Operationally, it requires either (a) relaxing PDBs on singleton workloads - which defeats the strict-PDB policies
platform teams commonly enforce via admission — or (b) external automation that watches Karpenter's drain activity and issues rollout restarts ahead of it. Option (b) duplicates information Karpenter
already has at the drain step (the pod, its owner, the owner's replica count, the owner's rollout status) and introduces an out-of-band controller that has to stay in sync with Karpenter's behavior.
Keeping this opt-in and gated leaves operators free to continue using either workaround if they prefer.

### Alternative 2: Use `kubectl delete pod` (or DELETE pods/<name>) directly

Skip eviction's PDB check by deleting the pod via the pods API instead. The workload controller will recreate the pod elsewhere.

**Rejected**: this is exactly what `terminationGracePeriod` already does after timeout, and it has the same downtime cost as eviction. It would resolve the *deadlock* but not the *outage*. The whole
reason to use the rollout primitive is that for Deployments it triggers `maxSurge`, which neither eviction nor direct delete can do.

### Alternative 3: Use `apps/v1/deployments/<name>/scale` to scale up, then evict

Scale the Deployment to 2 → wait for the new pod to be Ready → evict the old pod → scale back to 1.

**Rejected**: this fights the Deployment controller (the scale change is observable to anyone watching the workload), it requires Karpenter to remember to scale back down (which is fragile if
Karpenter dies mid-drain), and it doesn't help StatefulSets. Rollout restart is the primitive the platform already supports.

### Alternative 4: Annotation-timestamp dedup (the original approach)

The first commit in this series used a 2-minute window on the `restartedAt` annotation as the in-flight gate: "if we already patched this owner within 2 minutes, don't patch again."

**Rejected (mid-implementation)**: Karpenter's consolidation cadence matches the 2-minute window almost exactly, so the dedup never fired in practice. Combined with a Spring Boot–style slow boot, this
caused every consolidation cycle to patch the workload again *before* the previous rollout finished, killing the not-yet-Ready surge pod and starting over — an infinite deploy loop reproduced in
development. The status-based gate (current proposal) reads the owner's actual rollout progress instead of guessing from a timestamp and is robust to arbitrary boot times.

### Alternative 5: Apply to multi-replica workloads too

Rollout-restart every workload regardless of replica count.

**Rejected**: rolling 50 replicas to drain 1 pod is wasteful, surprising, and breaks the well-understood eviction+PDB contract that multi-replica workloads already rely on. The singleton case is the
unique one where eviction's contract fails the workload owner; multi-replica works fine as-is.

## Backward Compatibility

Fully backward-compatible:

- The feature gate defaults to `false`. Existing clusters see no behavior change on upgrade.
- When the gate is `on`, only pods with a singleton apps/v1 owner are routed to the new path. All other pods continue through the existing eviction path with identical semantics.
- No CRDs, annotations, or API contracts change.
