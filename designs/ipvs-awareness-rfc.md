# RFC: In-Place Pod Vertical Scaling (IPVS) Awareness for Karpenter

| Field | Value |
|---|---|
| **Authors** | jonesflp |
| **Status** | Draft |
| **Created** | 2026-03-24 |
| **Last Updated** | 2026-03-24 |
| **Reviewers** | Karpenter maintainers, SIG-Autoscaling |
| **Tracking** | TBD |

## Summary

This RFC proposes adding In-Place Pod Vertical Scaling (IPVS) awareness to Karpenter so it makes correct capacity decisions when pod resource requests are mutable. Without this change, Karpenter's assumption that pod requests are immutable creates a destructive consolidation loop when workloads use IPVS. We introduce a new `InPlacePodVerticalScaling` feature gate that enables IPVS-aware resource tracking, consolidation protection, peak-envelope provisioning, and provisioning patience across four Karpenter subsystems.

## Motivation

Karpenter currently assumes pod resource requests are immutable. Since Kubernetes 1.27, the InPlacePodVerticalScaling feature allows pods to change their resource requests and limits without restarting. This creates a dangerous feedback loop: pods reduce their requests at steady state, Karpenter observes underutilization and consolidates (evicts) them, replacement pods start with high startup requests requiring larger nodes, then scale down again — and the cycle repeats.

This loop causes continuous pod churn, wasted compute spend, and degraded workload stability. Operators who want to adopt IPVS with Karpenter today must disable consolidation entirely, sacrificing one of Karpenter's core value propositions.

The problem affects any cluster operator running workloads that use VPA in in-place mode, applications with startup-heavy resource profiles, or any team adopting the upstream IPVS feature. As IPVS moves toward beta and GA in Kubernetes, this gap will affect an increasing number of Karpenter users.

- **As a cluster operator**, I want Karpenter to correctly track pod resource usage when pods resize in place, so that node utilization calculations reflect actual consumption rather than stale spec values.
- **As a workload owner**, I want to annotate my pods with their peak resource requirements, so that Karpenter provisions nodes large enough for the full pod lifecycle without triggering unnecessary consolidation.
- **As a cluster operator**, I want Karpenter to avoid consolidating nodes during active pod resizes, so that workloads are not disrupted by premature eviction during transient low-utilization periods.

## Goals and Non-Goals

### Goals

- Track effective pod resources as `max(spec.requests, allocatedResources, peak annotations)` when the IPVS feature gate is enabled.
- Exclude nodes with actively resizing pods (`Proposed` or `InProgress` status) from consolidation candidacy.
- Introduce a configurable grace period (default 5 minutes) after resize completion before reconsidering a node for consolidation.
- Use peak resource envelopes for instance type selection during provisioning, ensuring new nodes can accommodate the full pod lifecycle.
- Support steady-state annotations to defer provisioning for pods expected to scale down, avoiding over-provisioning during startup.
- Emit Prometheus metrics, Kubernetes events, and debug logs for all IPVS-related decisions.

### Non-Goals

- Automatically detecting peak resource requirements. Users must annotate pods explicitly.
- Modifying pod resource requests. Karpenter only reads pod specs and status; it never writes to them.
- Direct integration with VPA controllers. Annotations are the interface between workload intent and Karpenter behavior.
- Supporting IPVS for init containers or ephemeral containers.

### Future Goals (Deferred)

- Namespace-level default peak annotations.
- Automatic peak detection from historical metrics or VPA recommendation API.
- Per-pod (rather than per-node) grace period tracking.

## Proposal

We add IPVS awareness to Karpenter behind a new `InPlacePodVerticalScaling` feature gate (default: `false`). When enabled, the feature modifies four subsystems: resource tracking, consolidation protection, scheduling simulation, and provisioning patience. All behavior is fully gated — when disabled, Karpenter operates identically to today.

### Feature Gate

We add `InPlacePodVerticalScaling` to the existing `FeatureGates` struct in `pkg/operator/options/options.go`, following the same pattern as `NodeRepair` and `SpotToSpotConsolidation`. The gate defaults to `false` and is parsed from the `--feature-gates` CLI flag.

```
--feature-gates=InPlacePodVerticalScaling=true
```

### IPVS-Aware Resource Computation

A new function `IPVSAwareRequestsForPod` computes the effective resource requests for a pod:

```
effectiveCPU    = max(spec.requests.cpu,    allocatedResources.cpu,    peak-cpu annotation)
effectiveMemory = max(spec.requests.memory, allocatedResources.memory, peak-memory annotation)
```

This function is called from the state tracker (`StateNode.updateForPod`) when the gate is enabled, replacing the current `RequestsForPods` call. Invalid annotations are logged as warnings and ignored for that resource type.

### Pod Annotations

We introduce four annotations in the `karpenter.sh/` domain:

| Annotation | Type | Example | Purpose |
|---|---|---|---|
| `karpenter.sh/peak-cpu` | Kubernetes quantity | `2000m` | Maximum CPU the pod may request across its lifecycle |
| `karpenter.sh/peak-memory` | Kubernetes quantity | `4Gi` | Maximum memory the pod may request across its lifecycle |
| `karpenter.sh/steady-state-cpu` | Kubernetes quantity | `500m` | Expected CPU after startup scale-down |
| `karpenter.sh/steady-state-memory` | Kubernetes quantity | `1Gi` | Expected memory after startup scale-down |

Example pod with peak annotations:

```yaml
apiVersion: v1
kind: Pod
metadata:
  annotations:
    karpenter.sh/peak-cpu: "2"
    karpenter.sh/peak-memory: "4Gi"
spec:
  containers:
  - name: app
    resources:
      requests:
        cpu: 500m
        memory: 1Gi
```

With the IPVS gate enabled, Karpenter treats this pod as requiring 2 CPU and 4Gi memory for all capacity decisions, even though its current spec requests are 500m and 1Gi.

### Consolidation Protection

When the IPVS gate is enabled, the consolidation engine's `ShouldDisrupt` method adds two checks before existing logic:

1. **Active resize check:** If any pod on the candidate node has a `Resize` status of `Proposed` or `InProgress`, the node is excluded from consolidation. An `Unconsolidatable` event is emitted on the Node.

2. **Grace period check:** After a pod's resize completes (status transitions from `InProgress` to empty), the node is protected from consolidation for a configurable grace period. The default is 5 minutes, configurable per NodePool:

```yaml
apiVersion: karpenter.sh/v1
kind: NodePool
spec:
  disruption:
    ipvsConsolidationGracePeriod: 5m
```

### Scheduling Simulator

The scheduling simulator's `updateCachedPodData` method uses `IPVSAwareRequestsForPod` when the gate is enabled. This ensures that when evaluating whether pods from a candidate node can fit on remaining nodes during consolidation, the peak resource envelope is used. A pod with a peak annotation of 2 CPU but current requests of 500m is evaluated as needing 2 CPU.

### Provisioning Patience

When a pending pod has steady-state annotations and the IPVS gate is enabled:

1. The provisioner evaluates whether the pod fits on existing nodes using steady-state values.
2. If it fits at steady-state, provisioning is deferred for a configurable patience duration (default 60 seconds, via `--ipvs-patience-duration`).
3. If patience expires and the pod remains unschedulable, the provisioner provisions using the current (higher) spec requests.

Steady-state values that exceed spec requests are logged as warnings and ignored.

### Observability

Two new Prometheus counters:

- `karpenter_ipvs_resource_adjustment_total{resource_type}` — increments when IPVS-aware computation produces a different effective resource than spec-based computation.
- `karpenter_ipvs_consolidation_deferred_total{reason}` — increments when consolidation is deferred due to `active_resize` or `grace_period`.

Kubernetes events are emitted on Node objects when consolidation is skipped due to IPVS reasons. Debug-level logs include pod name, original requests, and adjusted values.

## Alternatives Considered

### Alternative: VPA Integration via Webhook

Intercept VPA recommendations via a mutating webhook and adjust Karpenter's internal view of pod resources. This approach was rejected because it tightly couples Karpenter to VPA, doesn't handle non-VPA IPVS use cases, and adds operational complexity with the webhook.

### Alternative: Always Use AllocatedResources

Track only `pod.Status.ContainerStatuses[].AllocatedResources` instead of spec requests. This prevents underestimation during active resizes but doesn't account for peak requirements or future scale-ups. It solves only half the problem.

### Alternative: Disable Consolidation for IPVS Pods

Skip consolidation entirely for any pod with resize activity. This is too coarse-grained — it prevents legitimate consolidation after resize stabilizes and doesn't address provisioning.

### Alternative: Do Nothing

IPVS adoption with Karpenter causes a destructive consolidation loop. Operators must disable consolidation entirely, losing one of Karpenter's core features. As IPVS moves toward GA in Kubernetes, this gap affects an increasing number of users.

## Risks and Mitigations

| Risk | Severity | Mitigation |
|---|---|---|
| Peak annotations are stale or incorrect, causing over-provisioning | Medium | Annotations are optional and operator-controlled; validation logs warnings for invalid values |
| Grace period delays legitimate consolidation | Low | Configurable per NodePool; default 5m is conservative but tunable |
| Patience mechanism delays provisioning for pods that won't scale down | Low | Short default (60s); only applies to pods with explicit steady-state annotations |
| Feature gate runtime toggle causes transient inconsistency | Low | Pod informer re-reconciliation (every 1 minute) naturally re-evaluates all pods |

### Backward Compatibility

All IPVS behavior is gated behind the `InPlacePodVerticalScaling` feature gate, which defaults to `false`. When disabled, there is zero behavioral change. No existing APIs, fields, or behaviors are modified. The feature gate can be disabled at any time to revert to current behavior.

### Performance

The additional computation is minimal: one `max()` operation per resource type per pod per reconciliation cycle. The grace period tracking adds one timestamp field per `StateNode`. No additional API calls are introduced.

## Testing and Validation

### Property-Based Tests (11 properties, 100+ iterations each)

- **P1:** IPVS-aware resource computation returns max across all sources
- **P2:** Feature gate disabled preserves current behavior
- **P3:** Invalid peak annotations fall back to effective requests
- **P4:** Nodes with actively resizing pods are excluded from consolidation
- **P5:** Consolidation grace period defers node consideration
- **P6:** Scheduling simulator uses peak envelope for pod fit
- **P7:** Provisioning uses peak envelope for instance type selection
- **P8:** Steady-state annotation validation (exceeds spec → capped)
- **P9:** Provisioning patience defers then falls back
- **P10:** Resource adjustment metric increments on adjustment
- **P11:** Consolidation deferred metric increments on deferral

### Unit Tests

- Feature gate parsing and defaults
- Annotation parsing: valid values (`500m`, `1Gi`, `2000m`, `4Gi`) and invalid values (`""`, `"abc"`, `"-1"`)
- Resize status handling for each status value
- Grace period defaults (5m) and custom configuration
- Steady-state exceeds spec fallback
- Event emission on consolidation skip
- Metric registration and counter increments

### Integration Tests

- Consolidation blocked during active resize, blocked during grace period, proceeds after grace period expires
- Provisioning with peak annotations selects correct instance types
- Provisioning patience defers then provisions after expiry
- Feature gate disabled preserves all existing behavior

### Edge Cases

- Nil `ContainerStatuses` (pod hasn't started)
- Multiple pods resizing on the same node (most recent completion time used)
- Steady-state annotation exceeding spec requests
- Patience expiration with no existing nodes

## Migration and Rollout Plan

1. **Alpha (current):** Opt-in via `--feature-gates=InPlacePodVerticalScaling=true`. All behavior gated. Default off.
2. **Beta:** After community feedback and upstream IPVS stabilization, change default to `true`. Existing clusters can opt out.
3. **GA:** Feature gate removed. IPVS awareness becomes standard behavior.

### Migration Steps

1. Enable the feature gate: `--feature-gates=InPlacePodVerticalScaling=true`
2. Add `karpenter.sh/peak-cpu` and `karpenter.sh/peak-memory` annotations to workloads that use IPVS
3. Optionally add `karpenter.sh/steady-state-cpu` and `karpenter.sh/steady-state-memory` for workloads with startup-heavy profiles
4. Optionally configure `ipvsConsolidationGracePeriod` per NodePool

### Rollback Plan

Disable the feature gate: `--feature-gates=InPlacePodVerticalScaling=false`. All IPVS behavior stops immediately. Karpenter reverts to spec-only resource tracking on the next reconciliation cycle (within 1 minute). No data migration or cleanup is needed.

### Documentation

- Feature gate reference page
- Annotation reference for peak and steady-state annotations
- NodePool disruption configuration guide (grace period)
- Troubleshooting guide for the consolidation loop problem

## Open Questions

1. Should peak annotations support namespace-level defaults, so operators don't need to annotate every pod? [For SIG-Autoscaling]
2. Should the grace period be tracked per-pod rather than per-node? Currently, any resize on a node protects the entire node. [For Karpenter maintainers]
3. Should we integrate with the VPA recommendation API directly, in addition to or instead of annotations? [For SIG-Autoscaling]
4. Is 60 seconds the right default patience duration? Slow-starting workloads may need longer. [For end users]
5. Should `Deferred` resize status be treated as an active resize state? Currently only `Proposed` and `InProgress` trigger consolidation protection. [For Karpenter maintainers]
